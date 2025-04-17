package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Package config handles loading, validation, and access to application configuration.

// Config holds the application configuration.
type Config struct {
	LLM struct {
		APIKey    string `yaml:"api_key"` // Gemini API Key (mandatory)
		ProjectID string `yaml:"project_id"` // GCP Project ID (needed for ADC fallback)
		Location  string `yaml:"location"`   // GCP Location (needed for ADC fallback)
		ModelName string `yaml:"model_name"` // e.g., gemini-1.5-flash
	} `yaml:"llm"`

	// Add other configuration sections as needed (e.g., Memory, Judge)
	Terminal struct {
		DefaultShell string `yaml:"default_shell,omitempty"` // Optional: force a specific shell
	} `yaml:"terminal,omitempty"`

	Log struct {
		Level string `yaml:"level,omitempty"` // e.g., debug, info, warn, error
	} `yaml:"log,omitempty"`
}

const (
	defaultConfigDirName  = ".constructo"
	defaultConfigFileName = "config.yaml"
	defaultModelName      = "gemini-1.5-flash"
	defaultLocation       = "us-central1"
)

// Load tries to load configuration from standard locations.
// Priority: ./{fileName}, ~/{dirName}/{fileName}
func Load() (*Config, error) {
	fileName := defaultConfigFileName
	dirName := defaultConfigDirName

	// 1. Check current directory
	currentDirConfigPath := fileName
	cfg, err := loadFromFile(currentDirConfigPath)
	if err == nil {
		fmt.Printf("Configuration loaded from: %s\n", currentDirConfigPath)
		applyDefaults(cfg) // Apply defaults after loading
		return cfg, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("error reading config from %s: %w", currentDirConfigPath, err)
	}

	// 2. Check home directory
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("could not get user home directory: %w", err)
	}
	homeConfigPath := filepath.Join(homeDir, dirName, fileName)
	cfg, err = loadFromFile(homeConfigPath)
	if err == nil {
		fmt.Printf("Configuration loaded from: %s\n", homeConfigPath)
		applyDefaults(cfg) // Apply defaults after loading
		return cfg, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("error reading config from %s: %w", homeConfigPath, err)
	}

	// 3. No config file found, return default config (with warning)
	fmt.Println("Warning: No config file found. Using default settings. Please create config.yaml.")
	defaultCfg := &Config{}
	applyDefaults(defaultCfg)
	return defaultCfg, nil
	// Alternatively, return an error if config is mandatory
	// return nil, fmt.Errorf("configuration file not found in ./ or ~/%s/", dirName)
}

func loadFromFile(filePath string) (*Config, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err // Propagate error (including os.IsNotExist)
	}

	var cfg Config
	err = yaml.Unmarshal(data, &cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal config yaml %s: %w", filePath, err)
	}

	return &cfg, nil
}

// applyDefaults ensures essential fields have default values if not set.
func applyDefaults(cfg *Config) {
	if cfg.LLM.ModelName == "" {
		cfg.LLM.ModelName = defaultModelName
	}
	if cfg.LLM.Location == "" {
		cfg.LLM.Location = defaultLocation
	}
	// API Key and Project ID don't have safe defaults, must be provided.
}