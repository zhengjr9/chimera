package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    ServerConfig     `yaml:"server"`
	Providers []ProviderConfig `yaml:"providers"`
}

type ServerConfig struct {
	Host    string   `yaml:"host"`
	Port    int      `yaml:"port"`
	APIKeys []string `yaml:"api-keys"`
}

type ProviderConfig struct {
	Name    string            `yaml:"name"`
	Type    string            `yaml:"type"`
	BaseURL string            `yaml:"base-url"`
	APIKey  string            `yaml:"api-key"`
	Headers map[string]string `yaml:"headers"`
	Models  []ModelConfig     `yaml:"models"`
}

type ModelConfig struct {
	Name    string   `yaml:"name"`
	Alias   string   `yaml:"-"`
	Aliases []string `yaml:"-"`
}

func (m *ModelConfig) UnmarshalYAML(value *yaml.Node) error {
	type rawModelConfig struct {
		Name  string    `yaml:"name"`
		Alias yaml.Node `yaml:"alias"`
	}

	var raw rawModelConfig
	if err := value.Decode(&raw); err != nil {
		return err
	}

	m.Name = raw.Name
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
	return &cfg, nil
}
