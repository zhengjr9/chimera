# Chimera - API プロトコル変換プロキシ

[中文文档](README_zh.md) | [English](README.md) | 日本語

Chimera は、Claude と OpenAI API プロトコル間をシームレスに変換する軽量な API プロキシサーバーです。Claude Code やその他の AI プログラミングツールを、OpenAI 互換の API プロバイダーで使用できるようにします。

## 機能

- **プロトコル変換**: Claude と OpenAI API フォーマット間を自動変換
- **モデルエイリアス**: ルーティング用のモデル名エイリアスをサポート
- **複数エイリアス**: `alias` は文字列または文字列配列をサポート
- **複数プロバイダー**: 異なるモデルを持つ複数の API プロバイダーを設定可能
- **ストリーミング対応**: ストリーミングレスポンス (SSE) を完全サポート
- **API キー認証**: API キーでプロキシを保護
- **ツール対応**: ツール使用と関数呼び出しに対応

## インストール

```bash
go build -o chimera
```

## 設定

実行ファイルと同じディレクトリに `config.yaml` ファイルを作成するか、カスタムパスを指定します:

```bash
./chimera [設定ファイルパス]
```

### 設定ファイル形式

```yaml
server:
  host: ""  # リッスンアドレス、空の場合は全インターフェース
  port: 8080  # リッスンポート
  api-keys:  # プロキシアクセス用 API キー
    - "your-proxy-api-key"

providers:
  - name: "provider-name"  # プロバイダー識別子
    type: "openai-compatible"  # プロバイダータイプ
    base-url: "https://api.example.com/v1"  # プロバイダー API ベース URL
    api-key: "your-provider-api-key"  # プロバイダー API キー
    models:
      - name: "actual-model-name"  # プロバイダー上の実際のモデル名
        alias: "alias-name"  # モデルのエイリアス（オプション）
      - name: "another-model-name"
        alias: ["alias-a", "alias-b"]  # または複数エイリアス
```

完全な例は `config.example.yaml` を参照してください。

## Claude Code での使用

Chimera を使用すると、OpenAI 互換の API プロバイダーで Claude Code を使用できます。

### ステップ 1: Chimera サーバーの起動

```bash
./chimera config.yaml
```

サーバーは設定されたホストとポートでリッスンを開始します（デフォルト: `:8080`）。

### ステップ 2: Claude Code の設定

Claude Code 設定ファイルを `~/.claude/settings.json` に編集または作成します:

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

以下を置き換えてください:
- `your-proxy-api-key` - `config.yaml` の server.api-keys に設定された API キー
- `your-model-alias` - providers 設定で定義されたモデル名またはエイリアス

### ステップ 3: Claude Code の実行

作業ディレクトリに移動して実行:

```bash
claude
```

プロンプトが表示されたら"Trust This Folder"を選択してファイルアクセスを許可します。

## Codex CLI での使用

Chimera は Codex CLI やその他の OpenAI 互換ツールでも動作します。

### 設定

以下の環境変数を設定します:

```bash
export OPENAI_API_BASE=http://localhost:8080/v1
export OPENAI_API_KEY=your-proxy-api-key
```

または Codex CLI 設定で直接設定します。

### Codex CLI の実行

```bash
codex
```

## Claude Code VS Code 拡張機能での使用

### ステップ 1: 拡張機能のインストール

VS Code 拡張機能市場から Claude Code Extension をインストールします。

### ステップ 2: 設定

VS Code 設定を開き"Claude Code"を検索するか、`settings.json` を編集します:

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

## API エンドポイント

Chimera は以下のエンドポイントを提供します:

- `POST /v1/messages` - Claude Messages API (OpenAI 形式に変換)
- `POST /v1/chat/completions` - OpenAI Chat Completions API (パススルー)
- `GET /v1/models` - 利用可能なモデル一覧

## プロバイダー設定例

### MiniMax API での使用

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

### 複数プロバイダーの使用

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

## 詳細設定

### カスタムヘッダー

プロバイダーに送信するカスタムヘッダーを追加できます:

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

### セキュリティに関する注意

1. **API キー**: プロキシサーバーには常に強力で一意の API キーを使用してください
2. **ネットワーク**: 本番環境では TLS を有効にしたリバースプロキシの背後で実行することを検討してください
3. **アクセス制御**: デフォルトで任意のホストからの接続を受け入れます; `host` を設定して制限できます

## トラブルシューティング

### 接続拒否

Chimera が実行中で正しいポートでリッスンしていることを確認:

```bash
curl http://localhost:8080/v1/models
```

### 認証エラー

API キーが `server.api-keys` のいずれかと一致することを確認:

```bash
curl -H "Authorization: Bearer your-proxy-api-key" http://localhost:8080/v1/models
```

### モデルが見つからない

モデル名またはエイリアスが `config.yaml` の providers セクションで正しく定義されていることを確認してください。

### タイムアウト問題

長時間実行されるリクエストの場合、Claude Code 設定で `API_TIMEOUT_MS` を増やしてください。

## ライセンス

MIT License
