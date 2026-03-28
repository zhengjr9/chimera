# Chimera - API Protocol Translation Proxy

[中文文档](README_zh.md) | [日本語](README_ja.md) | English


Chimera is a lightweight API proxy server that seamlessly translates between Claude and OpenAI API protocols. It allows you to use Claude Code and other AI coding tools with any OpenAI-compatible API provider.

## Features

- **Protocol Translation**: Automatically translates between Claude and OpenAI API formats
- **Model Aliasing**: Support for model name aliases for easy routing
- **Multiple Aliases Per Model**: `alias` supports either a string or a string array
- **Multiple Providers**: Configure multiple API providers with different models
- **Streaming Support**: Full support for streaming responses (SSE)
- **API Key Authentication**: Secure your proxy with API key authentication
- **Tool Support**: Compatible with tool use and function calling

## Installation

```bash
go build -o chimera
```

## Configuration

Create a `config.yaml` file in the same directory as the executable, or specify a custom path:

```bash
./chimera [config-path]
```

### Configuration File Format

```yaml
server:
  host: ""  # Listen address, empty for all interfaces
  port: 8080  # Listen port
  api-keys:  # API keys for accessing the proxy
    - "your-proxy-api-key"

providers:
  - name: "provider-name"  # Provider identifier
    type: "openai-compatible"  # Provider type
    base-url: "https://api.example.com/v1"  # Provider API base URL
    api-key: "your-provider-api-key"  # Provider API key
    models:
      - name: "actual-model-name"  # Real model name on provider
        alias: "alias-name"  # Optional alias for the model
      - name: "another-model-name"
        alias: ["alias-a", "alias-b"]  # Or multiple aliases
```

See `config.example.yaml` for a complete example.

## Usage with Claude Code

Chimera enables you to use Claude Code with any OpenAI-compatible API provider.

### Step 1: Start Chimera Server

```bash
./chimera config.yaml
```

The server will start listening on the configured host and port (default: `:8080`).

### Step 2: Configure Claude Code

Edit or create the Claude Code configuration file at `~/.claude/settings.json`:

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

Replace:
- `your-proxy-api-key` with an API key from your `config.yaml` server.api-keys
- `your-model-alias` with a model name or alias defined in your providers config

### Step 3: Run Claude Code

Navigate to your working directory and run:

```bash
claude
```

Select "Trust This Folder" when prompted to allow file access.

## Usage with Codex CLI

Chimera also works with Codex CLI and other OpenAI-compatible tools.

### Configuration

Set the following environment variables:

```bash
export OPENAI_API_BASE=http://localhost:8080/v1
export OPENAI_API_KEY=your-proxy-api-key
```

Or configure directly in your Codex CLI settings.

### Running Codex CLI

```bash
codex
```

## Usage with Claude Code VS Code Extension

### Step 1: Install the Extension

Install Claude Code Extension for VS Code from the marketplace.

### Step 2: Configure Settings

Open VS Code settings and search for "Claude Code", or edit your `settings.json`:

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

## API Endpoints

Chimera provides the following endpoints:

- `POST /v1/messages` - Claude Messages API (translates to OpenAI format)
- `POST /v1/chat/completions` - OpenAI Chat Completions API (passthrough)
- `GET /v1/models` - List available models

## Example Provider Configurations

### Using with MiniMax API

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

### Using with Multiple Providers

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

## Advanced Configuration

### Custom Headers

You can add custom headers to be sent to the provider:

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

### Security Notes

1. **API Keys**: Always use strong, unique API keys for your proxy server
2. **Network**: Consider running behind a reverse proxy with TLS for production
3. **Access Control**: The proxy accepts connections from any host by default; configure `host` to restrict

## Troubleshooting

### Connection Refused

Ensure Chimera is running and listening on the correct port:

```bash
curl http://localhost:8080/v1/models
```

### Authentication Error

Verify your API key matches one in `server.api-keys`:

```bash
curl -H "Authorization: Bearer your-proxy-api-key" http://localhost:8080/v1/models
```

### Model Not Found

Check that the model name or alias is correctly defined in your `config.yaml` providers section.

### Timeout Issues

Increase `API_TIMEOUT_MS` in your Claude Code settings for long-running requests.

## License

MIT License
