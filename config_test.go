package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigDefaultPort(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	content := `
server:
  host: "0.0.0.0"
providers:
  - name: test
    base-url: http://localhost:8080
    api-key: test-key
    models:
      - name: test-model
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.Server.Port != 8080 {
		t.Errorf("expected default port 8080, got %d", cfg.Server.Port)
	}
}

func TestLoadConfigExplicitPort(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	content := `
server:
  host: "0.0.0.0"
  port: 3000
providers:
  - name: test
    base-url: http://localhost:8080
    api-key: test-key
    models:
      - name: test-model
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.Server.Port != 3000 {
		t.Errorf("expected port 3000, got %d", cfg.Server.Port)
	}
}

func TestLoadConfigAPIKeys(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	content := `
server:
  api-keys:
    - key1
    - key2
providers:
  - name: test
    base-url: http://localhost:8080
    api-key: test-key
    models:
      - name: test-model
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if len(cfg.Server.APIKeys) != 2 {
		t.Errorf("expected 2 api keys, got %d", len(cfg.Server.APIKeys))
	}
	if cfg.Server.APIKeys[0] != "key1" || cfg.Server.APIKeys[1] != "key2" {
		t.Errorf("unexpected api keys: %v", cfg.Server.APIKeys)
	}
}

func TestLoadConfigProviders(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	content := `
server:
  port: 8080
providers:
  - name: openai
    type: openai
    base-url: https://api.openai.com/v1
    api-key: sk-test
    models:
      - name: gpt-4
        alias: gpt4
      - name: gpt-3.5-turbo
        alias: gpt35
  - name: anthropic
    type: anthropic
    base-url: https://api.anthropic.com/v1
    api-key: sk-ant-test
    models:
      - name: claude-3-opus
        alias: claude-opus
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if len(cfg.Providers) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(cfg.Providers))
	}

	if cfg.Providers[0].Name != "openai" {
		t.Errorf("expected first provider name 'openai', got %q", cfg.Providers[0].Name)
	}
	if len(cfg.Providers[0].Models) != 2 {
		t.Errorf("expected 2 models in first provider, got %d", len(cfg.Providers[0].Models))
	}
	if cfg.Providers[0].Models[0].Alias != "gpt4" {
		t.Errorf("expected model alias 'gpt4', got %q", cfg.Providers[0].Models[0].Alias)
	}
}

func TestLoadConfigFileNotFound(t *testing.T) {
	_, err := LoadConfig("/nonexistent/config.yaml")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestLoadConfigInvalidYAML(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	content := `invalid: yaml: content: [`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := LoadConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestLoadConfigHeaders(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	content := `
server:
  port: 8080
providers:
  - name: custom
    base-url: http://localhost:8080
    api-key: test-key
    headers:
      X-Custom-Header: custom-value
      Another-Header: another-value
    models:
      - name: test-model
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if len(cfg.Providers[0].Headers) != 2 {
		t.Errorf("expected 2 headers, got %d", len(cfg.Providers[0].Headers))
	}
	if cfg.Providers[0].Headers["X-Custom-Header"] != "custom-value" {
		t.Errorf("unexpected header value: %v", cfg.Providers[0].Headers)
	}
}
