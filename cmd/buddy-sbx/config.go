package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const (
	defaultContextName   = "default"
	defaultNamespaceName = "default"
	defaultEndpoint      = "http://localhost:8080"
)

// CLIConfig models ~/.buddy/config.yaml.
type CLIConfig struct {
	CurrentContext string          `yaml:"current_context"`
	Contexts       []ContextConfig `yaml:"contexts"`
}

// ContextConfig defines a connection context.
type ContextConfig struct {
	Name             string `yaml:"name"`
	Endpoint         string `yaml:"endpoint"`
	Token            string `yaml:"token"`
	DefaultNamespace string `yaml:"default_namespace"`
}

func getDefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".buddy/config.yaml"
	}
	return filepath.Join(home, ".buddy", "config.yaml")
}

func loadConfig(path string) (*CLIConfig, error) {
	if path == "" {
		path = getDefaultConfigPath()
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Return default fallback config
			return &CLIConfig{
				CurrentContext: defaultContextName,
				Contexts: []ContextConfig{
					{
						Name:             defaultContextName,
						Endpoint:         defaultEndpoint,
						Token:            "",
						DefaultNamespace: defaultNamespaceName,
					},
				},
			}, nil
		}
		return nil, fmt.Errorf("failed to read config file %s: %w", path, err)
	}

	var cfg CLIConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %w", path, err)
	}

	return &cfg, nil
}

func saveConfig(path string, cfg *CLIConfig) error {
	if path == "" {
		path = getDefaultConfigPath()
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create config directory %s: %w", dir, err)
	}
	if filepath.Clean(dir) == filepath.Clean(filepath.Dir(getDefaultConfigPath())) {
		_ = os.Chmod(dir, 0700)
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	return os.WriteFile(path, data, 0600)
}

func (c *CLIConfig) GetContext(name string) *ContextConfig {
	for i := range c.Contexts {
		if c.Contexts[i].Name == name {
			return &c.Contexts[i]
		}
	}
	return nil
}

func (c *CLIConfig) ActiveContext() *ContextConfig {
	if c.CurrentContext != "" {
		if ctx := c.GetContext(c.CurrentContext); ctx != nil {
			return ctx
		}
	}
	if len(c.Contexts) > 0 {
		return &c.Contexts[0]
	}
	return nil
}
