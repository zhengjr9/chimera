# Chimera - API 协议转换代理

中文 | [English](README.md)

Chimera 是一个轻量级的 API 代理服务器,可以在 Claude 和 OpenAI API 协议之间无缝转换。它允许您在任何兼容 OpenAI 的 API 提供商上使用 Claude Code 和其他 AI 编程工具。

## 功能特性

- **协议转换**: 自动在 Claude 和 OpenAI API 格式之间转换
- **模型别名**: 支持模型名称别名,便于路由
- **单模型多别名**: `alias` 同时支持字符串和字符串数组
- **多提供商支持**: 配置多个 API 提供商和不同的模型
- **流式响应支持**: 完整支持流式响应 (SSE)
- **API 密钥认证**: 通过 API 密钥保护您的代理
- **工具调用支持**: 兼容工具使用和函数调用

## 安装

```bash
go build -o chimera
```

## 配置

在可执行文件同目录下创建 `config.yaml` 文件,或指定自定义路径:

```bash
./chimera [配置文件路径]
```

### 配置文件格式

```yaml
server:
  host: ""  # 监听地址,留空表示监听所有网卡
  port: 8080  # 监听端口
  api-keys:  # 访问代理的 API 密钥
    - "your-proxy-api-key"

providers:
  - name: "provider-name"  # 提供商标识
    type: "openai-compatible"  # 提供商类型
    base-url: "https://api.example.com/v1"  # 提供商 API 基础 URL
    api-key: "your-provider-api-key"  # 提供商 API 密钥
    models:
      - name: "actual-model-name"  # 提供商上的真实模型名称
        alias: "alias-name"  # 模型的可选别名
      - name: "another-model-name"
        alias: ["alias-a", "alias-b"]  # 或多个别名
```

完整示例请参见 `config.example.yaml`。

## 与 Claude Code 配合使用

Chimera 让您可以在任何兼容 OpenAI 的 API 提供商上使用 Claude Code。

### 步骤 1: 启动 Chimera 服务器

```bash
./chimera config.yaml
```

服务器将在配置的主机和端口上开始监听(默认: `:8080`)。

### 步骤 2: 配置 Claude Code

编辑或创建 Claude Code 配置文件 `~/.claude/settings.json`:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:8080/v1",
    "ANTHROPIC_AUTH_TOKEN": "your-proxy-api-key",
    "API_TIMEOUT_MS": "3000000",
    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": 1,
    "ANTHROPIC_MODEL": "your-model-alias",
    "ANTHROPIC_SMALL_FAST_MODEL": "your-model-alias",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "your-model-alias",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "your-model-alias",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "your-model-alias"
  }
}
```

请替换:
- `your-proxy-api-key` - 替换为 `config.yaml` 中 server.api-keys 里配置的 API 密钥
- `your-model-alias` - 替换为在 providers 配置中定义的模型名称或别名

### 步骤 3: 运行 Claude Code

进入您的工作目录并运行:

```bash
claude
```

当提示时选择"Trust This Folder"以允许文件访问。

## 与 Codex CLI 配合使用

Chimera 也支持 Codex CLI 和其他兼容 OpenAI 的工具。

### 配置

设置以下环境变量:

```bash
export OPENAI_API_BASE=http://localhost:8080/v1
export OPENAI_API_KEY=your-proxy-api-key
```

或在 Codex CLI 设置中直接配置。

### 运行 Codex CLI

```bash
codex
```

## 与 Claude Code VS Code 扩展配合使用

### 步骤 1: 安装扩展

从 VS Code 扩展市场安装 Claude Code Extension。

### 步骤 2: 配置设置

打开 VS Code 设置并搜索"Claude Code",或编辑您的 `settings.json`:

```json
{
  "claudeCode.preferredLocation": "panel",
  "claudeCode.selectedModel": "your-model-alias",
  "claudeCode.environmentVariables": [
    {
      "name": "ANTHROPIC_BASE_URL",
      "value": "http://localhost:8080/v1"
    },
    {
      "name": "ANTHROPIC_AUTH_TOKEN",
      "value": "your-proxy-api-key"
    },
    {
      "name": "API_TIMEOUT_MS",
      "value": "3000000"
    },
    {
      "name": "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
      "value": "1"
    },
    {
      "name": "ANTHROPIC_MODEL",
      "value": "your-model-alias"
    },
    {
      "name": "ANTHROPIC_SMALL_FAST_MODEL",
      "value": "your-model-alias"
    },
    {
      "name": "ANTHROPIC_DEFAULT_SONNET_MODEL",
      "value": "your-model-alias"
    },
    {
      "name": "ANTHROPIC_DEFAULT_OPUS_MODEL",
      "value": "your-model-alias"
    },
    {
      "name": "ANTHROPIC_DEFAULT_HAIKU_MODEL",
      "value": "your-model-alias"
    }
  ]
}
```

## API 端点

Chimera 提供以下端点:

- `POST /v1/messages` - Claude Messages API (转换为 OpenAI 格式)
- `POST /v1/chat/completions` - OpenAI Chat Completions API (直接透传)
- `GET /v1/models` - 列出可用模型

## 提供商配置示例

### 与 MiniMax API 配合使用

```yaml
server:
  host: ""
  port: 8080
  api-keys:
    - "your-secret-key"

providers:
  - name: "minimax"
    type: "openai-compatible"
    base-url: "https://api.minimax.io/v1"
    api-key: "your-minimax-api-key"
    models:
      - name: "MiniMax-M1"
        alias: "claude-sonnet-4-20250514"
      - name: "MiniMax-M1"
        alias: "claude-3-5-sonnet-20241022"
      - name: "MiniMax-M1"
        alias: "claude-opus-4-20250514"
      - name: "MiniMax-M1"
        alias: "claude-3-opus-20240229"
      - name: "MiniMax-M1"
        alias: "claude-3-5-haiku-20241022"
```

### 使用多个提供商

```yaml
server:
  host: ""
  port: 8080
  api-keys:
    - "your-secret-key"

providers:
  - name: "provider-a"
    type: "openai-compatible"
    base-url: "https://api.provider-a.com/v1"
    api-key: "provider-a-api-key"
    models:
      - name: "model-1"
        alias: "fast-model"

  - name: "provider-b"
    type: "openai-compatible"
    base-url: "https://api.provider-b.com/v1"
    api-key: "provider-b-api-key"
    models:
      - name: "model-2"
        alias: "smart-model"
```

## 高级配置

### 自定义请求头

您可以添加发送给提供商的自定义请求头:

```yaml
providers:
  - name: "custom-provider"
    type: "openai-compatible"
    base-url: "https://api.example.com/v1"
    api-key: "your-api-key"
    headers:
      X-Custom-Header: "custom-value"
    models:
      - name: "model-name"
```

### 安全注意事项

1. **API 密钥**: 始终为您的代理服务器使用强且唯一的 API 密钥
2. **网络安全**: 生产环境建议在反向代理后运行并启用 TLS
3. **访问控制**: 默认接受来自任何主机的连接;可通过配置 `host` 来限制

## 故障排除

### 连接被拒绝

确保 Chimera 正在运行并在正确的端口监听:

```bash
curl http://localhost:8080/v1/models
```

### 认证错误

验证您的 API 密钥是否与 `server.api-keys` 中的某一项匹配:

```bash
curl -H "Authorization: Bearer your-proxy-api-key" http://localhost:8080/v1/models
```

### 模型未找到

检查模型名称或别名是否在 `config.yaml` 的 providers 部分正确定义。

### 超时问题

对于长时间运行的请求,在 Claude Code 设置中增加 `API_TIMEOUT_MS` 的值。

## 许可证

MIT License
