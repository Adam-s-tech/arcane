// Package config handles CLI configuration loading and persistence.
//
// Configuration is stored in a YAML file at ~/.config/arcanecli.yml.
// This package provides functions to load, save, and access configuration
// values including the server URL, API key, and default environment.
//
// # Configuration File
//
// The configuration file uses the following format:
//
//	server_url: https://your-server.com
//	api_key: your-api-key
//	default_environment: "0"
//	log_level: info
//
// # Version Information
//
// Version and Revision variables are set at build time via ldflags:
//
//	go build -ldflags "-X github.com/getarcaneapp/arcane/cli/v2/internal/config.Version=1.0.0"
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samber/hot"
	"go.getarcane.app/acfs/atomic"
	"go.yaml.in/yaml/v4"

	"github.com/getarcaneapp/arcane/cli/v2/internal/types"
)

// Version and build information - set via ldflags at build time
var (
	Version          = "dev"
	Revision         = "unknown"
	CLIStableBaseURL = "https://github.com/getarcaneapp/arcane/releases/download"
	CLINextBaseURL   = "https://bucket.getarcane.app/bin/cli-next"
)

const (
	configFileName             = "arcanecli.yml"
	defaultPaginationInitLimit = 20
)

var customConfigPath string

var configCache = hot.NewHotCache[string, *types.Config](hot.LRU, 4).
	WithCopyOnRead((*types.Config).Clone).
	WithCopyOnWrite((*types.Config).Clone).
	Build()

func normalizeConfigInternal(cfg *types.Config) *types.Config {
	if cfg == nil {
		return DefaultConfig()
	}
	normalized := cfg.Clone()
	if normalized == nil {
		return DefaultConfig()
	}

	if normalized.Pagination.Resources == nil {
		normalized.Pagination.Resources = make(map[string]types.PaginationResourceConfig)
	}

	return normalized
}

func invalidateCacheInternal() {
	configCache.Purge()
}

// DefaultConfig returns a Config with sensible default values.
// The defaults are:
//   - ServerURL: http://localhost:3552
//   - DefaultEnvironment: "0"
//   - LogLevel: "info"
func DefaultConfig() *types.Config {
	return &types.Config{
		ServerURL:          "http://localhost:3552",
		DefaultEnvironment: "0",
		LogLevel:           "info",
	}
}

// ConfigPath returns the absolute path to the configuration file.
// The config file is located at ~/.config/arcanecli.yml.
// Returns an error if the user's home directory cannot be determined.
func ConfigPath() (string, error) {
	if customConfigPath != "" {
		return customConfigPath, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	return filepath.Join(home, ".config", configFileName), nil
}

// SetConfigPath overrides the default configuration file location.
// Accepts absolute or relative paths and expands a leading ~ to the home directory.
func SetConfigPath(path string) error {
	if strings.TrimSpace(path) == "" {
		customConfigPath = ""
		invalidateCacheInternal()
		return nil
	}

	path = strings.TrimSpace(path)
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("failed to expand config path: %w", err)
		}
		rel := strings.TrimPrefix(path, "~")
		path = filepath.Join(home, strings.TrimPrefix(rel, string(os.PathSeparator)))
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("failed to resolve config path: %w", err)
	}

	customConfigPath = absPath
	invalidateCacheInternal()
	return nil
}

// Load reads the configuration from disk and returns it.
// If the config file does not exist, default values are returned.
// Returns an error if the file exists but cannot be read or parsed.
func Load() (*types.Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}

	if cfg, ok, _ := configCache.Get(path); ok {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			def := DefaultConfig()
			configCache.Set(path, def)
			return def, nil
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	cfg := DefaultConfig()
	if parseErr := yaml.Unmarshal(data, cfg); parseErr != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", parseErr)
	}
	normalized := normalizeConfigInternal(cfg)

	configCache.Set(path, normalized)

	return normalized, nil
}

// Save writes the configuration to disk.
// The config directory is created if it does not exist.
// The file is created with 0600 permissions for security.
func Save(c *types.Config) error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}

	cfg := normalizeConfigInternal(c)
	cfg.Pagination.Default.Limit = max(0, cfg.Pagination.Default.Limit)
	resources := cfg.Pagination.Resources
	cfg.Pagination.Resources = make(map[string]types.PaginationResourceConfig, len(resources))
	for resource, rc := range resources {
		if rc.Limit > 0 {
			cfg.SetResourceLimit(resource, rc.Limit)
		}
	}

	if writeErr := writeConfigInternal(path, cfg, false); writeErr != nil {
		return writeErr
	}

	configCache.Set(path, cfg)

	return nil
}

// InitDefaultFile creates a default config file with credential placeholders.
// It returns true when a file is created, or false when an
// existing file is left unchanged.
func InitDefaultFile() (bool, error) {
	path, err := ConfigPath()
	if err != nil {
		return false, err
	}

	info, err := os.Stat(path)
	switch {
	case err == nil:
		if info.IsDir() {
			return false, fmt.Errorf("config path is a directory: %s", path)
		}
		return false, nil
	case os.IsNotExist(err):
		// Continue and create the file below.
	default:
		return false, fmt.Errorf("failed to stat config path: %w", err)
	}

	cfg := DefaultConfig()
	cfg.SetDefaultLimit(defaultPaginationInitLimit)
	for _, resource := range types.KnownPaginatedResources {
		cfg.SetResourceLimit(resource, defaultPaginationInitLimit)
	}
	var template yaml.Node
	if encodeErr := template.Encode(cfg); encodeErr != nil {
		return false, fmt.Errorf("failed to encode config template: %w", encodeErr)
	}
	// Show empty authentication fields even though normal saves omit them.
	for _, key := range []string{"api_key", "jwt_token", "refresh_token", "federated_audience"} {
		template.Content = append(template.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: ""},
		)
	}

	if writeErr := writeConfigInternal(path, &template, true); writeErr != nil {
		if errors.Is(writeErr, os.ErrExist) {
			return false, nil
		}
		return false, writeErr
	}

	invalidateCacheInternal()
	return true, nil
}

func writeConfigInternal(path string, cfg any, exclusive bool) (err error) {
	data, marshalErr := yaml.Marshal(cfg)
	if marshalErr != nil {
		return fmt.Errorf("failed to marshal config: %w", marshalErr)
	}
	if mkdirErr := os.MkdirAll(filepath.Dir(path), 0o700); mkdirErr != nil {
		return fmt.Errorf("failed to create config directory: %w", mkdirErr)
	}
	writePath := path
	if exclusive {
		stagingDir, tempErr := os.MkdirTemp(filepath.Dir(path), ".arcanecli-*")
		if tempErr != nil {
			return fmt.Errorf("failed to create config staging directory: %w", tempErr)
		}
		defer func() {
			if cleanupErr := os.RemoveAll(stagingDir); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("failed to remove config staging directory: %w", cleanupErr))
			}
		}()
		writePath = filepath.Join(stagingDir, configFileName)
	}
	if writeErr := atomic.WriteFile(writePath, data, 0o600); writeErr != nil {
		return fmt.Errorf("failed to write config file: %w", writeErr)
	}
	if exclusive {
		// Linking publishes the complete file without replacing an existing path.
		if linkErr := os.Link(writePath, path); linkErr != nil {
			return fmt.Errorf("failed to create config file: %w", linkErr)
		}
	}
	return nil
}

// BackupFile moves the active config file to a .bak path and removes the
// original file from its previous location. If no config file exists, it
// returns moved=false and no error.
func BackupFile() (backupPath string, moved bool, err error) {
	path, err := ConfigPath()
	if err != nil {
		return "", false, err
	}
	backupPath = path + ".bak"

	info, err := os.Stat(path)
	switch {
	case err == nil:
		if info.IsDir() {
			return "", false, fmt.Errorf("config path is a directory: %s", path)
		}
	case os.IsNotExist(err):
		return backupPath, false, nil
	default:
		return "", false, fmt.Errorf("failed to stat config path: %w", err)
	}

	if existingBackup, backupErr := os.Stat(backupPath); backupErr == nil {
		if existingBackup.IsDir() {
			return "", false, fmt.Errorf("backup path is a directory: %s", backupPath)
		}
		rotatedPath := fmt.Sprintf("%s.%s", backupPath, time.Now().UTC().Format("20060102150405"))
		if renameErr := os.Rename(backupPath, rotatedPath); renameErr != nil {
			return "", false, fmt.Errorf("failed to rotate existing backup %s: %w", backupPath, renameErr)
		}
	} else if !os.IsNotExist(backupErr) {
		return "", false, fmt.Errorf("failed to stat backup path: %w", backupErr)
	}

	if backupConfigErr := os.Rename(path, backupPath); backupConfigErr != nil {
		return "", false, fmt.Errorf("failed to move config to backup: %w", backupConfigErr)
	}

	invalidateCacheInternal()
	return backupPath, true, nil
}
