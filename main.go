package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/viper"
	"golang.org/x/net/proxy"
)

// ---------------------- 配置结构 ----------------------

type Config struct {
	ThinkingServices []ThinkingService  `mapstructure:"thinking_services"`
	Channels         map[string]Channel `mapstructure:"channels"`
	Global           GlobalConfig       `mapstructure:"global"`
}

type ThinkingService struct {
	ID                    int      `mapstructure:"id"`
	Name                  string   `mapstructure:"name"`
	Model                 string   `mapstructure:"model"`
	BaseURL               string   `mapstructure:"base_url"`
	APIPath               string   `mapstructure:"api_path"`
	APIKey                string   `mapstructure:"api_key"`
	Timeout               int      `mapstructure:"timeout"`
	Retry                 int      `mapstructure:"retry"`
	Weight                int      `mapstructure:"weight"`
	Proxy                 string   `mapstructure:"proxy"`
	Mode                  string   `mapstructure:"mode"` // "standard" 或 "full"
	ReasoningEffort       string   `mapstructure:"reasoning_effort"`
	ReasoningFormat       string   `mapstructure:"reasoning_format"`
	Temperature           *float64 `mapstructure:"temperature"`
	ForceStopDeepThinking bool     `mapstructure:"force_stop_deep_thinking"` // 配置项：标准模式下遇到 content 时是否立即停止
}

func (s *ThinkingService) GetFullURL() string {
	return s.BaseURL + s.APIPath
}

type Channel struct {
	Name    string `mapstructure:"name"`
	BaseURL string `mapstructure:"base_url"`
	APIPath string `mapstructure:"api_path"`
	Timeout int    `mapstructure:"timeout"`
	Proxy   string `mapstructure:"proxy"`
}

func (c *Channel) GetFullURL() string {
	return c.BaseURL + c.APIPath
}

type LogConfig struct {
	Level    string      `mapstructure:"level"`
	Format   string      `mapstructure:"format"`
	Output   string      `mapstructure:"output"`
	FilePath string      `mapstructure:"file_path"`
	Debug    DebugConfig `mapstructure:"debug"`
}

type DebugConfig struct {
	Enabled          bool `mapstructure:"enabled"`
	PrintRequest     bool `mapstructure:"print_request"`
	PrintResponse    bool `mapstructure:"print_response"`
	MaxContentLength int  `mapstructure:"max_content_length"`
}

type ProxyConfig struct {
	Enabled       bool   `mapstructure:"enabled"`
	Default       string `mapstructure:"default"`
	AllowInsecure bool   `mapstructure:"allow_insecure"`
}

type GlobalConfig struct {
	MaxRetries     int            `mapstructure:"max_retries"`
	DefaultTimeout int            `mapstructure:"default_timeout"`
	ErrorCodes     struct {
		RetryOn []int `mapstructure:"retry_on"`
	} `mapstructure:"error_codes"`
	Log         LogConfig      `mapstructure:"log"`
	Server      ServerConfig   `mapstructure:"server"`
	Proxy       ProxyConfig    `mapstructure:"proxy"`
	ConfigPaths []string       `mapstructure:"config_paths"`
	Thinking    ThinkingConfig `mapstructure:"thinking"`
	Security    SecurityConfig `mapstructure:"security"`
}

type ServerConfig struct {
	Port         int    `mapstructure:"port"`
	Host         string `mapstructure:"host"`
	ReadTimeout  int    `mapstructure:"read_timeout"`
	WriteTimeout int    `mapstructure:"write_timeout"`
	IdleTimeout  int    `mapstructure:"idle_timeout"`
}

type ThinkingConfig struct {
	Enabled          bool `mapstructure:"enabled"`
	AddToAllRequests bool `mapstructure:"add_to_all_requests"`
	Timeout          int  `mapstructure:"timeout"`
}

// ---------------------- API 相关结构 ----------------------

type ChatCompletionRequest struct {
	Model       string                  `json:"model"`
	Messages    []ChatCompletionMessage `json:"messages"`
	Temperature float64                 `json:"temperature,omitempty"`
	MaxTokens   int                     `json:"max_tokens,omitempty"`
	Stream      bool                    `json:"stream,omitempty"`
	APIKey      string                  `json:"-"` // 内部传递，不序列化
}

type ChatCompletionMessage struct {
	Role             string      `json:"role"`
	Content          string      `json:"content"`
	ReasoningContent interface{} `json:"reasoning_content,omitempty"`
}

type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

type Choice struct {
	Index        int                   `json:"index"`
	Message      ChatCompletionMessage `json:"message"`
	FinishReason string                `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ---------------------- 日志工具 ----------------------

type RequestLogger struct {
	RequestID string
	Model     string
	StartTime time.Time
	logs      []string
	config    *Config
}

func NewRequestLogger(config *Config) *RequestLogger {
	return &RequestLogger{
		RequestID: uuid.New().String(),
		StartTime: time.Now(),
		logs:      make([]string, 0),
		config:    config,
	}
}

func (l *RequestLogger) Log(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	l.logs = append(l.logs, fmt.Sprintf("[%s] %s", time.Now().Format(time.RFC3339), msg))
	log.Printf("[RequestID: %s] %s", l.RequestID, msg)
}

func (l *RequestLogger) LogContent(contentType string, content interface{}, maxLength int) {
	if !l.config.Global.Log.Debug.Enabled {
		return
	}
	sanitizedContent := sanitizeJSON(content)
	truncatedContent := truncateContent(sanitizedContent, maxLength)
	l.Log("%s Content:\n%s", contentType, truncatedContent)
}

func truncateContent(content string, maxLength int) string {
	if len(content) <= maxLength {
		return content
	}
	return content[:maxLength] + "... (truncated)"
}

func sanitizeJSON(data interface{}) string {
	sanitized, err := json.Marshal(data)
	if err != nil {
		return "Failed to marshal JSON"
	}
	content := string(sanitized)
	sensitivePattern := `"api_key":\s*"[^"]*"`
	content = regexp.MustCompile(sensitivePattern).ReplaceAllString(content, `"api_key":"****"`)
	return content
}

func extractRealAPIKey(fullKey string) string {
	parts := strings.Split(fullKey, "-")
	if len(parts) >= 3 && (parts[0] == "deep" || parts[0] == "openai") {
		return strings.Join(parts[2:], "-")
	}
	return fullKey
}

func extractChannelID(fullKey string) string {
	parts := strings.Split(fullKey, "-")
	if len(parts) >= 2 && (parts[0] == "deep" || parts[0] == "openai") {
		return parts[1]
	}
	return "1" // 默认渠道
}

func logAPIKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "..." + key[len(key)-4:]
}

// ---------------------- Server 结构 ----------------------

type Server struct {
	config      *Config
	srv         *http.Server
	rateLimiter *SimpleRateLimiter
}

var (
	randMu  sync.Mutex
	randGen = rand.New(rand.NewSource(time.Now().UnixNano()))
)

func NewServer(config *Config) *Server {
	// 初始化速率限制器，默认值
	requestsPerMinute := 60
	if config.Global.Security.RateLimit.RequestsPerMinute > 0 {
		requestsPerMinute = config.Global.Security.RateLimit.RequestsPerMinute
	}
	
	return &Server{
		config:      config,
		rateLimiter: NewSimpleRateLimiter(requestsPerMinute),
	}
}

func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.handleOpenAIRequests)
	mux.HandleFunc("/v1/models", s.handleOpenAIRequests)
	mux.HandleFunc("/health", s.handleHealth)

	s.srv = &http.Server{
		Addr:         fmt.Sprintf("%s:%d", s.config.Global.Server.Host, s.config.Global.Server.Port),
		Handler:      mux,
		ReadTimeout:  time.Duration(s.config.Global.Server.ReadTimeout) * time.Second,
		WriteTimeout: time.Duration(s.config.Global.Server.WriteTimeout) * time.Second,
		IdleTimeout:  time.Duration(s.config.Global.Server.IdleTimeout) * time.Second,
	}

	log.Printf("Server starting on %s\n", s.srv.Addr)
	return s.srv.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
}

func (s *Server) handleOpenAIRequests(w http.ResponseWriter, r *http.Request) {
	logger := NewRequestLogger(s.config)

	// 速率限制检查
	clientIP := getClientIP(r)
	if !s.rateLimiter.Allow(clientIP) {
		logger.Log("Rate limit exceeded for IP: %s", clientIP)
		http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	fullAPIKey := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	apiKey := extractRealAPIKey(fullAPIKey)
	channelID := extractChannelID(fullAPIKey)

	// API密钥验证
	if s.config.Global.Security.EnableAuth {
		if !validateAPIKey(apiKey, s.config.Global.Security.TrustedAPIKeys) {
			logger.Log("Unauthorized API key access from IP: %s", clientIP)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	logger.Log("Received request for %s with API Key: %s", r.URL.Path, logAPIKey(fullAPIKey))
	logger.Log("Extracted channel ID: %s", channelID)
	logger.Log("Extracted real API Key: %s", logAPIKey(apiKey))

	targetChannel, ok := s.config.Channels[channelID]
	if !ok {
		http.Error(w, "Invalid channel", http.StatusBadRequest)
		return
	}

	if r.URL.Path == "/v1/models" {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		req := &ChatCompletionRequest{APIKey: apiKey}
		s.forwardModelsRequest(w, r.Context(), req, targetChannel)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 请求大小限制
	maxSize := int64(10 * 1024 * 1024) // 默认10MB
	if s.config.Global.Security.MaxRequestSize > 0 {
		maxSize = s.config.Global.Security.MaxRequestSize
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSize)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		logger.Log("Error reading request body: %v", err)
		http.Error(w, "Failed to read request", http.StatusBadRequest)
		return
	}
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewBuffer(body))

	if s.config.Global.Log.Debug.PrintRequest {
		logger.LogContent("Request", string(body), s.config.Global.Log.Debug.MaxContentLength)
	}

	var req ChatCompletionRequest
	if err := json.NewDecoder(bytes.NewBuffer(body)).Decode(&req); err != nil {
		logger.Log("Invalid JSON in request: %v", err)
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	
	// 输入验证
	if err := validateChatCompletionRequest(&req); err != nil {
		logger.Log("Request validation failed: %v", err)
		http.Error(w, "Invalid request parameters", http.StatusBadRequest)
		return
	}
	
	req.APIKey = apiKey

	thinkingService := s.getWeightedRandomThinkingService()
	logger.Log("Using thinking service: %s with API Key: %s", thinkingService.Name, logAPIKey(thinkingService.APIKey))

	if req.Stream {
		handler, err := NewStreamHandler(w, thinkingService, targetChannel, s.config)
		if err != nil {
			sendSecureErrorResponse(w, "Streaming not supported", err, logger)
			return
		}
		if err := handler.HandleRequest(r.Context(), &req); err != nil {
			logger.Log("Stream handler error: %v", err)
		}
	} else {
		thinkingResp, err := s.processThinkingContent(r.Context(), &req, thinkingService)
		if err != nil {
			logger.Log("Error processing thinking content: %v", err)
			sendSecureErrorResponse(w, "Service temporarily unavailable", err, logger)
			return
		}
		enhancedReq := s.prepareEnhancedRequest(&req, thinkingResp, thinkingService)
		s.forwardRequest(w, r.Context(), enhancedReq, targetChannel)
	}
}

func (s *Server) getWeightedRandomThinkingService() ThinkingService {
	thinkingServices := s.config.ThinkingServices
	if len(thinkingServices) == 0 {
		return ThinkingService{}
	}
	totalWeight := 0
	for _, svc := range thinkingServices {
		totalWeight += svc.Weight
	}
	if totalWeight <= 0 {
		log.Println("Warning: Total weight of thinking services is not positive, using first service as default.")
		return thinkingServices[0]
	}
	randMu.Lock()
	randNum := randGen.Intn(totalWeight)
	randMu.Unlock()
	currentSum := 0
	for _, svc := range thinkingServices {
		currentSum += svc.Weight
		if randNum < currentSum {
			return svc
		}
	}
	return thinkingServices[0]
}

// ---------------------- 非流式处理思考服务 ----------------------

type ThinkingResponse struct {
	Content          string
	ReasoningContent string
}

func (s *Server) processThinkingContent(ctx context.Context, req *ChatCompletionRequest, svc ThinkingService) (*ThinkingResponse, error) {
	logger := NewRequestLogger(s.config)
	log.Printf("Getting thinking content from service: %s (mode=%s)", svc.Name, svc.Mode)

	thinkingReq := *req
	thinkingReq.Model = svc.Model
	thinkingReq.APIKey = svc.APIKey

	var systemPrompt string
	if svc.Mode == "full" {
		systemPrompt = "Provide a detailed step-by-step analysis of the question. Your entire response will be used as reasoning and won't be shown to the user directly."
	} else {
		systemPrompt = "Please provide a detailed reasoning process for your response. Think step by step."
	}
	thinkingReq.Messages = append([]ChatCompletionMessage{
		{Role: "system", Content: systemPrompt},
	}, thinkingReq.Messages...)

	temp := 0.7
	if svc.Temperature != nil {
		temp = *svc.Temperature
	}
	payload := map[string]interface{}{
		"model":       svc.Model,
		"messages":    thinkingReq.Messages,
		"stream":      false,
		"temperature": temp,
	}
	if isValidReasoningEffort(svc.ReasoningEffort) {
		payload["reasoning_effort"] = svc.ReasoningEffort
	}
	if isValidReasoningFormat(svc.ReasoningFormat) {
		payload["reasoning_format"] = svc.ReasoningFormat
	}

	if s.config.Global.Log.Debug.PrintRequest {
		logger.LogContent("Thinking Service Request", payload, s.config.Global.Log.Debug.MaxContentLength)
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal thinking request: %v", err)
	}
	client, err := createHTTPClient(svc.Proxy, time.Duration(svc.Timeout)*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP client: %v", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, svc.GetFullURL(), bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+svc.APIKey)

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send thinking request: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %v", err)
	}
	if s.config.Global.Log.Debug.PrintResponse {
		logger.LogContent("Thinking Service Response", string(respBody), s.config.Global.Log.Debug.MaxContentLength)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("thinking service returned %d: %s", resp.StatusCode, string(respBody))
	}

	var thinkingResp ChatCompletionResponse
	if err := json.Unmarshal(respBody, &thinkingResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal thinking response: %v", err)
	}
	if len(thinkingResp.Choices) == 0 {
		return nil, fmt.Errorf("thinking service returned no choices")
	}

	result := &ThinkingResponse{}
	choice := thinkingResp.Choices[0]

	if svc.Mode == "full" {
		result.ReasoningContent = choice.Message.Content
		result.Content = "Based on the above detailed analysis."
	} else {
		if choice.Message.ReasoningContent != nil {
			switch v := choice.Message.ReasoningContent.(type) {
			case string:
				result.ReasoningContent = v
			case map[string]interface{}:
				if j, err := json.Marshal(v); err == nil {
					result.ReasoningContent = string(j)
				}
			}
		}
		if result.ReasoningContent == "" {
			result.ReasoningContent = choice.Message.Content
		}
		result.Content = "Based on the above reasoning."
	}
	return result, nil
}

func (s *Server) prepareEnhancedRequest(originalReq *ChatCompletionRequest, thinkingResp *ThinkingResponse, svc ThinkingService) *ChatCompletionRequest {
	newReq := *originalReq
	var systemPrompt string
	if svc.Mode == "full" {
		systemPrompt = fmt.Sprintf(`Consider the following detailed analysis (not shown to user):
%s

Provide a clear, concise response that incorporates insights from this analysis.`, thinkingResp.ReasoningContent)
	} else {
		systemPrompt = fmt.Sprintf(`Previous thinking process:
%s
Please consider the above thinking process in your response.`, thinkingResp.ReasoningContent)
	}
	newReq.Messages = append([]ChatCompletionMessage{
		{Role: "system", Content: systemPrompt},
	}, newReq.Messages...)
	return &newReq
}

func (s *Server) forwardRequest(w http.ResponseWriter, ctx context.Context, req *ChatCompletionRequest, channel Channel) {
	logger := NewRequestLogger(s.config)
	if s.config.Global.Log.Debug.PrintRequest {
		logger.LogContent("Forward Request", req, s.config.Global.Log.Debug.MaxContentLength)
	}
	jsonData, err := json.Marshal(req)
	if err != nil {
		sendSecureErrorResponse(w, "Request processing failed", err, logger)
		return
	}

	client, err := createHTTPClient(channel.Proxy, time.Duration(channel.Timeout)*time.Second)
	if err != nil {
		sendSecureErrorResponse(w, "Service configuration error", err, logger)
		return
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, channel.GetFullURL(), bytes.NewBuffer(jsonData))
	if err != nil {
		sendSecureErrorResponse(w, "Request creation failed", err, logger)
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)

	resp, err := client.Do(httpReq)
	if err != nil {
		sendSecureErrorResponse(w, "Service temporarily unavailable", err, logger)
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		sendSecureErrorResponse(w, "Response processing failed", err, logger)
		return
	}
	if s.config.Global.Log.Debug.PrintResponse {
		logger.LogContent("Forward Response", string(respBody), s.config.Global.Log.Debug.MaxContentLength)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logger.Log("Target server returned status: %d", resp.StatusCode)
		http.Error(w, "Service error", resp.StatusCode)
		return
	}

	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

func (s *Server) forwardModelsRequest(w http.ResponseWriter, ctx context.Context, req *ChatCompletionRequest, targetChannel Channel) {
	logger := NewRequestLogger(s.config)
	if s.config.Global.Log.Debug.PrintRequest {
		logger.LogContent("/v1/models Request", req, s.config.Global.Log.Debug.MaxContentLength)
	}
	fullChatURL := targetChannel.GetFullURL()
	parsedURL, err := url.Parse(fullChatURL)
	if err != nil {
		http.Error(w, "Failed to parse channel URL", http.StatusInternalServerError)
		return
	}
	baseURL := parsedURL.Scheme + "://" + parsedURL.Host
	modelsURL := strings.TrimSuffix(baseURL, "/") + "/v1/models"

	client, err := createHTTPClient(targetChannel.Proxy, time.Duration(targetChannel.Timeout)*time.Second)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create HTTP client: %v", err), http.StatusInternalServerError)
		return
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		http.Error(w, "Failed to create request", http.StatusInternalServerError)
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)

	resp, err := client.Do(httpReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to forward request: %v", err), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "Failed to read response", http.StatusInternalServerError)
		return
	}
	if s.config.Global.Log.Debug.PrintResponse {
		logger.LogContent("/v1/models Response", string(respBody), s.config.Global.Log.Debug.MaxContentLength)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		http.Error(w, fmt.Sprintf("Target server error: %s", resp.Status), resp.StatusCode)
		return
	}

	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

// ---------------------- 流式处理 ----------------------

// collectedReasoningBuffer 用于收集思考服务返回的 reasoning_content（标准模式只收集 reasoning_content；full 模式收集全部）
type collectedReasoningBuffer struct {
	builder strings.Builder
	mode    string
}

func (cb *collectedReasoningBuffer) append(text string) {
	cb.builder.WriteString(text)
}

func (cb *collectedReasoningBuffer) get() string {
	return cb.builder.String()
}

type StreamHandler struct {
	thinkingService ThinkingService
	targetChannel   Channel
	writer          http.ResponseWriter
	flusher         http.Flusher
	config          *Config
}

func NewStreamHandler(w http.ResponseWriter, thinkingService ThinkingService, targetChannel Channel, config *Config) (*StreamHandler, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("streaming not supported by this writer")
	}
	return &StreamHandler{
		thinkingService: thinkingService,
		targetChannel:   targetChannel,
		writer:          w,
		flusher:         flusher,
		config:          config,
	}, nil
}

// HandleRequest 分两阶段：先流式接收思考服务 SSE 并转发给客户端（只转发包含 reasoning_content 的 chunk，标准模式遇到纯 content 则中断）；
// 然后使用收集到的 reasoning_content 构造最终请求，发起目标 Channel 的 SSE 并转发给客户端。
func (h *StreamHandler) HandleRequest(ctx context.Context, req *ChatCompletionRequest) error {
	h.writer.Header().Set("Content-Type", "text/event-stream")
	h.writer.Header().Set("Cache-Control", "no-cache")
	h.writer.Header().Set("Connection", "keep-alive")

	logger := NewRequestLogger(h.config)
	reasonBuf := &collectedReasoningBuffer{mode: h.thinkingService.Mode}

	// 阶段一：流式接收思考服务 SSE
	if err := h.streamThinkingService(ctx, req, reasonBuf, logger); err != nil {
		return err
	}

	// 阶段二：构造最终请求，并流式转发目标 Channel SSE
	finalReq := h.prepareFinalRequest(req, reasonBuf.get())
	if err := h.streamFinalChannel(ctx, finalReq, logger); err != nil {
		return err
	}
	return nil
}

// streamThinkingService 连接思考服务 SSE，原样转发包含 reasoning_content 的 SSE chunk给客户端，同时收集 reasoning_content。
// 在标准模式下，一旦遇到只返回 non-empty 的 content（且 reasoning_content 为空），则立即中断，不转发该 chunk。
func (h *StreamHandler) streamThinkingService(ctx context.Context, req *ChatCompletionRequest, reasonBuf *collectedReasoningBuffer, logger *RequestLogger) error {
	thinkingReq := *req
	thinkingReq.Model = h.thinkingService.Model
	thinkingReq.APIKey = h.thinkingService.APIKey

	var systemPrompt string
	if h.thinkingService.Mode == "full" {
		systemPrompt = "Provide a detailed step-by-step analysis of the question. Your entire response will be used as reasoning and won't be shown to the user directly."
	} else {
		systemPrompt = "Please provide a detailed reasoning process for your response. Think step by step."
	}
	messages := append([]ChatCompletionMessage{
		{Role: "system", Content: systemPrompt},
	}, thinkingReq.Messages...)

	temp := 0.7
	if h.thinkingService.Temperature != nil {
		temp = *h.thinkingService.Temperature
	}
	bodyMap := map[string]interface{}{
		"model":       thinkingReq.Model,
		"messages":    messages,
		"temperature": temp,
		"stream":      true,
	}
	if isValidReasoningEffort(h.thinkingService.ReasoningEffort) {
		bodyMap["reasoning_effort"] = h.thinkingService.ReasoningEffort
	}
	if isValidReasoningFormat(h.thinkingService.ReasoningFormat) {
		bodyMap["reasoning_format"] = h.thinkingService.ReasoningFormat
	}

	jsonData, err := json.Marshal(bodyMap)
	if err != nil {
		return err
	}

	client, err := createHTTPClient(h.thinkingService.Proxy, time.Duration(h.thinkingService.Timeout)*time.Second)
	if err != nil {
		return err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.thinkingService.GetFullURL(), bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+h.thinkingService.APIKey)

	log.Printf("Starting ThinkingService SSE => %s (mode=%s, force_stop=%v)", h.thinkingService.GetFullURL(), h.thinkingService.Mode, h.thinkingService.ForceStopDeepThinking)
	resp, err := client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("thinking service status=%d, body=%s", resp.StatusCode, string(b))
	}

	reader := bufio.NewReader(resp.Body)
	var lastLine string
	// 标识是否需中断思考 SSE（标准模式下）
	forceStop := false

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" || line == lastLine {
			continue
		}
		lastLine = line

		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		dataPart := strings.TrimPrefix(line, "data: ")
		if dataPart == "[DONE]" {
			// 思考服务结束：直接中断（不发送特殊标记）
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content,omitempty"`
					ReasoningContent string `json:"reasoning_content,omitempty"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason,omitempty"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(dataPart), &chunk); err != nil {
			// 如果解析失败，直接原样转发
			h.writer.Write([]byte("data: " + dataPart + "\n\n"))
			h.flusher.Flush()
			continue
		}

		if len(chunk.Choices) > 0 {
			c := chunk.Choices[0]
			if h.config.Global.Log.Debug.PrintResponse {
				logger.LogContent("Thinking SSE chunk", chunk, h.config.Global.Log.Debug.MaxContentLength)
			}

			if h.thinkingService.Mode == "full" {
				// full 模式：收集所有内容
				if c.Delta.ReasoningContent != "" {
					reasonBuf.append(c.Delta.ReasoningContent)
				}
				if c.Delta.Content != "" {
					reasonBuf.append(c.Delta.Content)
				}
				// 原样转发整 chunk
				forwardLine := "data: " + dataPart + "\n\n"
				h.writer.Write([]byte(forwardLine))
				h.flusher.Flush()
			} else {
				// standard 模式：只收集 reasoning_content
				if c.Delta.ReasoningContent != "" {
					reasonBuf.append(c.Delta.ReasoningContent)
					// 转发该 chunk
					forwardLine := "data: " + dataPart + "\n\n"
					h.writer.Write([]byte(forwardLine))
					h.flusher.Flush()
				}
				// 如果遇到 chunk 中只有 content（且 reasoning_content 为空），认为思考链结束，不转发该 chunk
				if c.Delta.Content != "" && strings.TrimSpace(c.Delta.ReasoningContent) == "" {
					forceStop = true
				}
			}

			// 如果 finishReason 非空，也认为结束
			if c.FinishReason != nil && *c.FinishReason != "" {
				forceStop = true
			}
		}

		if forceStop {
			break
		}
	}
	// 读空剩余数据
	io.Copy(io.Discard, reader)
	return nil
}

func (h *StreamHandler) prepareFinalRequest(originalReq *ChatCompletionRequest, reasoningCollected string) *ChatCompletionRequest {
	req := *originalReq
	var systemPrompt string
	if h.thinkingService.Mode == "full" {
		systemPrompt = fmt.Sprintf(
			`Consider the following detailed analysis (not shown to user):
%s

Provide a clear, concise response that incorporates insights from this analysis.`,
			reasoningCollected,
		)
	} else {
		systemPrompt = fmt.Sprintf(
			`Previous thinking process:
%s
Please consider the above thinking process in your response.`,
			reasoningCollected,
		)
	}
	req.Messages = append([]ChatCompletionMessage{
		{Role: "system", Content: systemPrompt},
	}, req.Messages...)
	return &req
}

func (h *StreamHandler) streamFinalChannel(ctx context.Context, req *ChatCompletionRequest, logger *RequestLogger) error {
	if h.config.Global.Log.Debug.PrintRequest {
		logger.LogContent("Final Request to Channel", req, h.config.Global.Log.Debug.MaxContentLength)
	}

	jsonData, err := json.Marshal(req)
	if err != nil {
		return err
	}
	client, err := createHTTPClient(h.targetChannel.Proxy, time.Duration(h.targetChannel.Timeout)*time.Second)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.targetChannel.GetFullURL(), bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)

	resp, err := client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("target channel status=%d, body=%s", resp.StatusCode, string(b))
	}

	reader := bufio.NewReader(resp.Body)
	var lastLine string

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" || line == lastLine {
			continue
		}
		lastLine = line

		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			doneLine := "data: [DONE]\n\n"
			h.writer.Write([]byte(doneLine))
			h.flusher.Flush()
			break
		}
		forwardLine := "data: " + data + "\n\n"
		h.writer.Write([]byte(forwardLine))
		h.flusher.Flush()

		if h.config.Global.Log.Debug.PrintResponse {
			logger.LogContent("Channel SSE chunk", forwardLine, h.config.Global.Log.Debug.MaxContentLength)
		}
	}
	io.Copy(io.Discard, reader)
	return nil
}

// ---------------------- HTTP Client 工具 ----------------------

func createHTTPClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if proxyURL != "" {
		// 验证代理URL的安全性
		if err := validateProxyURL(proxyURL, []string{}); err != nil {
			return nil, fmt.Errorf("proxy validation failed: %v", err)
		}
		
		parsedURL, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL: %v", err)
		}
		switch parsedURL.Scheme {
		case "http", "https":
			transport.Proxy = http.ProxyURL(parsedURL)
		case "socks5":
			dialer, err := proxy.FromURL(parsedURL, proxy.Direct)
			if err != nil {
				return nil, fmt.Errorf("failed to create SOCKS5 dialer: %v", err)
			}
			transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			}
		default:
			return nil, fmt.Errorf("unsupported proxy scheme: %s", parsedURL.Scheme)
		}
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}, nil
}

func maskSensitiveHeaders(headers http.Header) http.Header {
	masked := make(http.Header)
	for k, vals := range headers {
		if strings.ToLower(k) == "authorization" {
			masked[k] = []string{"Bearer ****"}
		} else {
			masked[k] = vals
		}
	}
	return masked
}

func isValidReasoningEffort(effort string) bool {
	switch strings.ToLower(effort) {
	case "low", "medium", "high":
		return true
	}
	return false
}

func isValidReasoningFormat(format string) bool {
	switch strings.ToLower(format) {
	case "parsed", "raw", "hidden":
		return true
	}
	return false
}

// ---------------------- 配置加载与验证 ----------------------

func loadConfig() (*Config, error) {
	var configFile string
	flag.StringVar(&configFile, "config", "", "path to config file")
	flag.Parse()
	viper.SetConfigType("yaml")

	if configFile != "" {
		viper.SetConfigFile(configFile)
	} else {
		ex, err := os.Executable()
		if err != nil {
			return nil, err
		}
		exePath := filepath.Dir(ex)
		defaultPaths := []string{
			filepath.Join(exePath, "config.yaml"),
			filepath.Join(exePath, "conf", "config.yaml"),
			"./config.yaml",
			"./conf/config.yaml",
		}
		if os.PathSeparator == '\\' {
			programData := os.Getenv("PROGRAMDATA")
			if programData != "" {
				defaultPaths = append(defaultPaths, filepath.Join(programData, "DeepAI", "config.yaml"))
			}
		} else {
			defaultPaths = append(defaultPaths, "/etc/deepai/config.yaml")
		}
		for _, p := range defaultPaths {
			viper.AddConfigPath(filepath.Dir(p))
			if strings.Contains(p, ".yaml") {
				viper.SetConfigName(strings.TrimSuffix(filepath.Base(p), ".yaml"))
			}
		}
	}

	if err := viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config: %v", err)
	}

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config error: %v", err)
	}
	if err := validateConfig(&cfg); err != nil {
		return nil, fmt.Errorf("config validation error: %v", err)
	}
	return &cfg, nil
}

func validateConfig(config *Config) error {
	if len(config.ThinkingServices) == 0 {
		return fmt.Errorf("no thinking services configured")
	}
	if len(config.Channels) == 0 {
		return fmt.Errorf("no channels configured")
	}
	for i, svc := range config.ThinkingServices {
		if svc.BaseURL == "" {
			return fmt.Errorf("thinking service %s has empty baseURL", svc.Name)
		}
		if svc.APIKey == "" {
			return fmt.Errorf("thinking service %s has empty apiKey", svc.Name)
		}
		if svc.Timeout <= 0 {
			return fmt.Errorf("thinking service %s has invalid timeout", svc.Name)
		}
		if svc.Model == "" {
			return fmt.Errorf("thinking service %s has empty model", svc.Name)
		}
		if svc.Mode == "" {
			config.ThinkingServices[i].Mode = "standard"
		} else if svc.Mode != "standard" && svc.Mode != "full" {
			return fmt.Errorf("thinking service %s unknown mode=%s", svc.Name, svc.Mode)
		}
	}
	return nil
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.Lshortfile)

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	log.Printf("Using config file: %s", viper.ConfigFileUsed())

	server := NewServer(cfg)

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if err := server.Start(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("start server error: %v", err)
		}
	}()
	log.Printf("Server started successfully")

	<-done
	log.Print("Server stopping...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("Server forced to shutdown: %v", err)
	}
	log.Print("Server stopped")
}