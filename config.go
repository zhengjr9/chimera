package main

import (
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
	Name  string `yaml:"name"`
	Alias string `yaml:"alias"`
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
