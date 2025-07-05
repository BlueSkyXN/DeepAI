package main

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// SecurityConfig 安全配置
type SecurityConfig struct {
	MaxRequestSize    int64         `mapstructure:"max_request_size"`    // 最大请求体大小 (bytes)
	RequestTimeout    time.Duration `mapstructure:"request_timeout"`     // 请求超时时间
	RateLimit         RateLimit     `mapstructure:"rate_limit"`          // 速率限制配置
	AllowedProxyHosts []string      `mapstructure:"allowed_proxy_hosts"` // 允许的代理主机白名单
	EnableAuth        bool          `mapstructure:"enable_auth"`         // 是否启用认证
	TrustedAPIKeys    []string      `mapstructure:"trusted_api_keys"`    // 受信任的API密钥列表
}

type RateLimit struct {
	RequestsPerMinute int `mapstructure:"requests_per_minute"` // 每分钟请求数限制
	BurstSize         int `mapstructure:"burst_size"`          // 突发请求数
}

// 输入验证函数
func validateChatCompletionRequest(req *ChatCompletionRequest) error {
	if req.Model == "" {
		return fmt.Errorf("model field is required")
	}
	
	if len(req.Messages) == 0 {
		return fmt.Errorf("messages field is required and cannot be empty")
	}
	
	// 验证消息内容长度
	for i, msg := range req.Messages {
		if len(msg.Content) > 100000 { // 限制单条消息最大长度
			return fmt.Errorf("message %d content too long (max: 100000 chars)", i)
		}
		if msg.Role == "" {
			return fmt.Errorf("message %d role is required", i)
		}
		if !isValidRole(msg.Role) {
			return fmt.Errorf("message %d has invalid role: %s", i, msg.Role)
		}
	}
	
	// 验证temperature范围
	if req.Temperature < 0 || req.Temperature > 2 {
		return fmt.Errorf("temperature must be between 0 and 2")
	}
	
	// 验证max_tokens
	if req.MaxTokens < 0 || req.MaxTokens > 100000 {
		return fmt.Errorf("max_tokens must be between 0 and 100000")
	}
	
	return nil
}

func isValidRole(role string) bool {
	validRoles := []string{"system", "user", "assistant", "function"}
	for _, valid := range validRoles {
		if role == valid {
			return true
		}
	}
	return false
}

// 安全的错误响应函数
func sendSecureErrorResponse(w http.ResponseWriter, userMsg string, internalErr error, logger *RequestLogger) {
	// 只向客户端发送通用错误信息
	http.Error(w, userMsg, http.StatusInternalServerError)
	
	// 详细错误仅记录在服务器日志中
	if logger != nil && internalErr != nil {
		logger.Log("Internal error: %v", internalErr)
	}
}

// 清理敏感信息的错误处理
func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	
	errStr := err.Error()
	
	// 移除可能的API密钥泄露
	apiKeyPattern := regexp.MustCompile(`(?i)(api[_-]?key|authorization|bearer)[\s:=]+[\w\-\.]+`)
	errStr = apiKeyPattern.ReplaceAllString(errStr, "${1}: [REDACTED]")
	
	// 移除可能的URL中的敏感信息
	urlPattern := regexp.MustCompile(`https?://[^/\s]+`)
	errStr = urlPattern.ReplaceAllStringFunc(errStr, func(url string) string {
		if strings.Contains(url, "@") {
			return "[REDACTED_URL]"
		}
		return url
	})
	
	return errStr
}

// 验证代理URL安全性
func validateProxyURL(proxyURL string, allowedHosts []string) error {
	if proxyURL == "" {
		return nil
	}
	
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return fmt.Errorf("invalid proxy URL format")
	}
	
	// 检查是否为内网地址
	if isPrivateIP(parsed.Hostname()) {
		return fmt.Errorf("proxy URL points to private network")
	}
	
	// 检查是否在白名单中
	if len(allowedHosts) > 0 {
		allowed := false
		for _, host := range allowedHosts {
			if parsed.Hostname() == host {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("proxy host not in allowed list")
		}
	}
	
	return nil
}

// 检查是否为内网IP
func isPrivateIP(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		// 如果不是IP地址，检查是否为本地域名
		if host == "localhost" || strings.HasSuffix(host, ".local") {
			return true
		}
		return false
	}
	
	// 检查私有IP段
	privateRanges := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"169.254.0.0/16", // 链路本地地址
		"::1/128",        // IPv6 localhost
		"fc00::/7",       // IPv6 私有地址
	}
	
	for _, cidr := range privateRanges {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if network.Contains(ip) {
			return true
		}
	}
	
	return false
}

// 请求大小限制中间件
func requestSizeLimitMiddleware(maxSize int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > maxSize {
				http.Error(w, "Request entity too large", http.StatusRequestEntityTooLarge)
				return
			}
			
			// 为了防止Content-Length伪造，限制实际读取的大小
			r.Body = http.MaxBytesReader(w, r.Body, maxSize)
			
			next.ServeHTTP(w, r)
		})
	}
}

// 简单的内存速率限制实现
type SimpleRateLimiter struct {
	requests map[string][]time.Time
	limit    int
	window   time.Duration
}

func NewSimpleRateLimiter(requestsPerMinute int) *SimpleRateLimiter {
	return &SimpleRateLimiter{
		requests: make(map[string][]time.Time),
		limit:    requestsPerMinute,
		window:   time.Minute,
	}
}

func (rl *SimpleRateLimiter) Allow(clientIP string) bool {
	now := time.Now()
	
	// 清理过期记录
	if times, exists := rl.requests[clientIP]; exists {
		var validTimes []time.Time
		for _, t := range times {
			if now.Sub(t) < rl.window {
				validTimes = append(validTimes, t)
			}
		}
		rl.requests[clientIP] = validTimes
	}
	
	// 检查是否超过限制
	if len(rl.requests[clientIP]) >= rl.limit {
		return false
	}
	
	// 记录本次请求
	rl.requests[clientIP] = append(rl.requests[clientIP], now)
	return true
}

// 获取客户端真实IP
func getClientIP(r *http.Request) string {
	// 检查X-Forwarded-For头
	xForwardedFor := r.Header.Get("X-Forwarded-For")
	if xForwardedFor != "" {
		// 取第一个IP（最原始的客户端IP）
		ips := strings.Split(xForwardedFor, ",")
		if len(ips) > 0 {
			return strings.TrimSpace(ips[0])
		}
	}
	
	// 检查X-Real-IP头
	xRealIP := r.Header.Get("X-Real-IP")
	if xRealIP != "" {
		return xRealIP
	}
	
	// 使用RemoteAddr
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

// 基础认证验证
func validateAPIKey(apiKey string, trustedKeys []string) bool {
	if len(trustedKeys) == 0 {
		return true // 如果没有配置受信任密钥，允许所有请求
	}
	
	for _, trusted := range trustedKeys {
		if apiKey == trusted {
			return true
		}
	}
	return false
}