# DeepAI 安全最佳实践指南

## 概述

本文档提供了部署和配置 DeepAI 服务的安全最佳实践建议。请在生产环境中遵循这些安全准则。

## 安全配置

### 1. 认证和授权

**启用API密钥认证**:
```yaml
global:
  security:
    enable_auth: true
    trusted_api_keys:
      - "sk-your-secure-api-key-1"
      - "sk-your-secure-api-key-2"
```

**最佳实践**:
- 使用强随机API密钥 (至少32字符)
- 定期轮换API密钥
- 为不同的客户端使用不同的API密钥
- 监控API密钥使用情况

### 2. 速率限制

**配置合理的速率限制**:
```yaml
global:
  security:
    rate_limit:
      requests_per_minute: 60  # 根据实际需求调整
      burst_size: 10          # 允许的突发请求数
```

**建议值**:
- 开发环境: 10-30 requests/minute
- 生产环境: 60-120 requests/minute
- 高流量环境: 根据服务器容量调整

### 3. 请求大小限制

**防止资源耗尽攻击**:
```yaml
global:
  security:
    max_request_size: 10485760  # 10MB
    request_timeout: 300        # 5分钟超时
```

**建议配置**:
- 最大请求大小: 1MB - 10MB
- 请求超时: 30秒 - 300秒
- 根据实际使用场景调整

### 4. 代理安全

**配置安全的代理设置**:
```yaml
global:
  security:
    allowed_proxy_hosts:
      - "trusted-proxy.example.com"
      - "127.0.0.1"  # 仅在必要时允许本地代理
```

**安全准则**:
- 仅允许受信任的代理主机
- 避免使用公共代理服务
- 定期审查代理配置
- 监控代理连接

## 网络安全

### 1. HTTPS/TLS

**强制使用HTTPS**:
- 在生产环境中始终使用HTTPS
- 配置有效的SSL/TLS证书
- 使用TLS 1.2或更高版本
- 定期更新证书

**Nginx反向代理示例**:
```nginx
server {
    listen 443 ssl http2;
    server_name your-domain.com;
    
    ssl_certificate /path/to/cert.pem;
    ssl_certificate_key /path/to/key.pem;
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_ciphers ECDHE-RSA-AES256-GCM-SHA512:DHE-RSA-AES256-GCM-SHA512:ECDHE-RSA-AES256-GCM-SHA384:DHE-RSA-AES256-GCM-SHA384;
    
    location / {
        proxy_pass http://127.0.0.1:8888;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

### 2. 防火墙配置

**网络层防护**:
```bash
# 仅允许必要的端口
ufw allow 22/tcp      # SSH (如果需要)
ufw allow 80/tcp      # HTTP (重定向到HTTPS)
ufw allow 443/tcp     # HTTPS
ufw deny 8888/tcp     # 阻止直接访问应用端口
ufw enable

# 或使用iptables
iptables -A INPUT -p tcp --dport 22 -j ACCEPT
iptables -A INPUT -p tcp --dport 80 -j ACCEPT
iptables -A INPUT -p tcp --dport 443 -j ACCEPT
iptables -A INPUT -p tcp --dport 8888 -j DROP
```

### 3. IP白名单 (可选)

如果需要限制访问来源:
```nginx
# 在Nginx中配置IP白名单
location / {
    allow 192.168.1.0/24;   # 允许内网
    allow 203.0.113.0/24;   # 允许特定IP段
    deny all;               # 拒绝其他所有IP
    
    proxy_pass http://127.0.0.1:8888;
}
```

## 日志和监控

### 1. 安全日志配置

**配置安全的日志记录**:
```yaml
global:
  log:
    level: "info"
    format: "json"
    output: "file"
    file_path: "/var/log/deepai/deepai.log"
    debug:
      enabled: false  # 生产环境中禁用详细调试
      print_request: false
      print_response: false
```

### 2. 日志轮转

**配置日志轮转** (使用logrotate):
```
/var/log/deepai/*.log {
    daily
    missingok
    rotate 30
    compress
    delaycompress
    notifempty
    create 0644 deepai deepai
    postrotate
        systemctl reload deepai
    endscript
}
```

### 3. 监控指标

**关键监控指标**:
- 请求速率和延迟
- 错误率 (按状态码分类)
- 资源使用 (CPU, 内存, 连接数)
- 安全事件 (认证失败, 速率限制触发)

**Prometheus监控示例**:
```yaml
# 可以添加metrics端点监控
- job_name: 'deepai'
  static_configs:
    - targets: ['localhost:8888']
  metrics_path: '/metrics'  # 如果实现了metrics端点
```

## 部署安全

### 1. 容器化部署

**Docker安全配置**:
```dockerfile
# 使用非root用户
FROM golang:1.24-alpine AS builder
# ... 构建步骤 ...

FROM alpine:latest
RUN addgroup -g 1001 deepai && \
    adduser -u 1001 -G deepai -s /bin/sh -D deepai
USER deepai
COPY --from=builder /app/deepai /usr/local/bin/deepai
EXPOSE 8888
CMD ["deepai", "-config", "/etc/deepai/config.yaml"]
```

**Docker运行安全选项**:
```bash
docker run -d \
  --name deepai \
  --user 1001:1001 \
  --read-only \
  --tmpfs /tmp \
  --cap-drop ALL \
  --cap-add NET_BIND_SERVICE \
  --security-opt no-new-privileges \
  -p 127.0.0.1:8888:8888 \
  -v /path/to/config:/etc/deepai:ro \
  deepai:latest
```

### 2. 系统服务配置

**systemd服务配置**:
```ini
[Unit]
Description=DeepAI Proxy Service
After=network.target

[Service]
Type=simple
User=deepai
Group=deepai
ExecStart=/usr/local/bin/deepai -config /etc/deepai/config.yaml
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal

# 安全选项
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/log/deepai
CapabilityBoundingSet=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
```

## 配置文件安全

### 1. 文件权限

**设置适当的文件权限**:
```bash
# 配置文件权限
chmod 600 /etc/deepai/config.yaml
chown deepai:deepai /etc/deepai/config.yaml

# 日志目录权限
chmod 755 /var/log/deepai
chown deepai:deepai /var/log/deepai
```

### 2. 敏感信息保护

**使用环境变量或密钥管理**:
```bash
# 使用环境变量传递敏感配置
export THINKING_SERVICE_API_KEY="sk-your-secret-key"
export CHANNEL_API_KEY="sk-another-secret-key"

# 在配置文件中引用
# api_key: "${THINKING_SERVICE_API_KEY}"
```

## 安全检查清单

### 部署前检查

- [ ] 已启用API密钥认证
- [ ] 配置了合理的速率限制
- [ ] 设置了请求大小限制
- [ ] 验证了代理配置安全性
- [ ] 配置了HTTPS/TLS
- [ ] 设置了防火墙规则
- [ ] 配置了日志轮转
- [ ] 设置了适当的文件权限
- [ ] 使用了非root用户运行服务

### 定期安全维护

- [ ] 轮换API密钥
- [ ] 更新SSL证书
- [ ] 审查访问日志
- [ ] 检查安全补丁
- [ ] 监控资源使用
- [ ] 备份配置文件
- [ ] 测试灾难恢复

## 事件响应

### 1. 安全事件类型

**常见安全事件**:
- 认证失败攻击
- 速率限制触发
- 异常请求模式
- 代理滥用
- 资源耗尽攻击

### 2. 响应措施

**自动响应**:
- 临时IP封禁
- 速率限制调整
- 服务降级

**手动响应**:
- 分析攻击模式
- 更新安全规则
- 通知相关人员
- 文档化事件

## 合规性考虑

### 数据保护

- 确保API密钥安全存储
- 实施数据最小化原则
- 定期删除旧日志
- 遵循相关法规要求

### 审计要求

- 记录所有API访问
- 保留审计日志
- 实施访问控制
- 定期安全审计

---

**重要提醒**: 安全是一个持续的过程，请定期审查和更新安全配置，关注最新的安全威胁和最佳实践。