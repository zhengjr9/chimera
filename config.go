package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    ServerConfig     `yaml:"server"`
	Providers []ProviderConfig `yaml:"providers"`
	Codex     CodexConfig      `yaml:"codex"`
	Kiro      KiroConfig       `yaml:"kiro"`
}

type ServerConfig struct {
	Host               string   `yaml:"host"`
	Port               int      `yaml:"port"`
	EnableAuth         *bool    `yaml:"enable-auth"`
	APIKeys            []string `yaml:"api-keys"`
	ToolDiagnosticsLog string   `yaml:"tool-diagnostics-log"`
}

type ProviderConfig struct {
	Name               string            `yaml:"name"`
	Type               string            `yaml:"type"`
	BaseURL            string            `yaml:"base-url"`
	APIKey             string            `yaml:"api-key"`
	APIKeys            []string          `yaml:"api-keys"`
	Proxy              string            `yaml:"proxy"`
	CAFile             string            `yaml:"ca-file"`
	Insecure           bool              `yaml:"insecure-skip-verify"`
	Headers            map[string]string `yaml:"headers"`
	AuthDir            string            `yaml:"auth-dir"`
	Models             []ModelConfig     `yaml:"models"`
	MaxTokens          int               `yaml:"max-tokens"`
	UseAPIKey          bool              `yaml:"use-api-key"`
	PlainStringContent bool              `yaml:"plain-string-content"`
}

type CodexConfig struct {
	CallbackBaseURL string `yaml:"callback-base-url"`
}

type KiroConfig struct {
	CallbackBaseURL string `yaml:"callback-base-url"`
}

type ModelConfig struct {
	Name               string   `yaml:"name"`
	Alias              string   `yaml:"-"`
	Aliases            []string `yaml:"-"`
	PlainStringContent *bool    `yaml:"plain-string-content"`
}

func (m *ModelConfig) UnmarshalYAML(value *yaml.Node) error {
	type rawModelConfig struct {
		Name               string    `yaml:"name"`
		Alias              yaml.Node `yaml:"alias"`
		PlainStringContent *bool     `yaml:"plain-string-content"`
	}

	var raw rawModelConfig
	if err := value.Decode(&raw); err != nil {
		return err
	}

	m.Name = raw.Name
	m.PlainStringContent = raw.PlainStringContent
	if raw.Alias.Kind == 0 {
		return nil
	}

	switch raw.Alias.Kind {
	case yaml.ScalarNode:
		var alias string
		if err := raw.Alias.Decode(&alias); err != nil {
			return err
		}
		m.Alias = alias
		if alias != "" {
			m.Aliases = []string{alias}
		}
		return nil
	case yaml.SequenceNode:
		var aliases []string
		if err := raw.Alias.Decode(&aliases); err != nil {
			return err
		}
		m.Aliases = aliases
		if len(aliases) > 0 {
			m.Alias = aliases[0]
		}
		return nil
	default:
		return fmt.Errorf("alias must be a string or string array")
	}
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	for i := range cfg.Providers {
		if err := validateProviderTransportConfig(&cfg.Providers[i]); err != nil {
			return nil, fmt.Errorf("providers[%d]: %w", i, err)
		}
	}
	return &cfg, nil
}

func (s ServerConfig) AuthEnabled() bool {
	if s.EnableAuth != nil {
		return *s.EnableAuth
	}
	return len(s.APIKeys) > 0
}
