// Package config loads runtime configuration from environment variables
// with sensible defaults for local development.
//
// No secrets are committed. See .env.example.
package config

import (
	"log/slog"
	"os"
	"strings"
)

// Config holds all runtime settings for the wam server.
type Config struct {
	// Addr is the TCP address to listen on, e.g. ":8080".
	Addr string
	// Env is the deployment environment: development, staging, production.
	Env string
	// LogLevelStr controls slog verbosity: debug, info, warn, error.
	LogLevelStr string

	// PublicBaseURL is used to build absolute URLs in OpenAPI servers + Swagger UI.
	PublicBaseURL string

	// DBPath is the SQLite file, e.g. ./data/wam.db. Shared by app tables
	// and the whatsmeow sqlstore container.
	DBPath string
	// DataDir holds uploads/exports and is created at boot.
	DataDir string
	// AdminPassword is the single-admin password (env-only, never committed).
	// Empty means auth is disabled (local dev convenience).
	AdminPassword string
	// SessionSecret signs the admin session cookie (32+ bytes in production).
	SessionSecret string
}

func getenv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// Load reads configuration from the environment.
func Load() Config {
	return Config{
		Addr:          getenv("WAM_ADDR", ":8080"),
		Env:           getenv("WAM_ENV", "development"),
		LogLevelStr:   getenv("WAM_LOG_LEVEL", "info"),
		PublicBaseURL: getenv("WAM_PUBLIC_BASE_URL", "http://localhost:8080"),
		DBPath:        getenv("WAM_DB_PATH", "./data/wam.db"),
		DataDir:       getenv("WAM_DATA_DIR", "./data"),
		AdminPassword: getenv("WAM_ADMIN_PASSWORD", ""),
		SessionSecret: getenv("WAM_SESSION_SECRET", "dev-secret-change-me-32-bytes!!"),
	}
}

// AuthEnabled reports whether admin auth is enforced.
func (c Config) AuthEnabled() bool {
	return strings.TrimSpace(c.AdminPassword) != ""
}

// LogLevel parses LogLevelStr into a slog.Level.
func (c Config) LogLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(c.LogLevelStr)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// IsDev reports whether we run in development mode.
func (c Config) IsDev() bool {
	return strings.EqualFold(c.Env, "development") || strings.EqualFold(c.Env, "dev") || c.Env == ""
}
