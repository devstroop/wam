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

	//	DBPath is the SQLite file, e.g. ./data/wam.db. Shared by app tables
	//	and the whatsmeow sqlstore container (legacy single-tenant path —
	//	used only when DatabaseURL is empty).
	DBPath string
	// DatabaseURL is the owner/migration Postgres DSN, e.g.
	// postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable.
	// Runs DDL migrations at boot (wam migrate-schema uses this too).
	DatabaseURL string
	// AppDatabaseURL is the least-privilege runtime DSN (wam_app role, no DDL).
	// When set, the server migrates with DatabaseURL then serves via this DSN
	// so RLS tenant isolation bites (superusers bypass RLS unconditionally).
	// Empty = serve via the owner DSN with a startup warning (dev only).
	AppDatabaseURL string
	// WADatabaseURL is the whatsmeow session-store DSN (needs DDL for Upgrade).
	// Empty = falls back to DatabaseURL. Never the app role: session tables
	// are created at runtime by the WA service.
	WADatabaseURL string
	// RedisURL is reserved for the sender queue (sender-scale branch).
	// Parsed but not yet dialed in the platform branch.
	RedisURL string
	// DataDir holds uploads/exports and is created at boot.
	DataDir string
	// AdminPassword is the single-admin password (env-only, never committed).
	// Empty means auth is disabled (local dev convenience). On Postgres with
	// zero users it seeds the first admin (email via AdminEmail).
	AdminPassword string
	// AdminEmail sets the seeded admin's email (Postgres bootstrap).
	AdminEmail string
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
		Addr:           getenv("WAM_ADDR", ":8080"),
		Env:            getenv("WAM_ENV", "development"),
		LogLevelStr:    getenv("WAM_LOG_LEVEL", "info"),
		PublicBaseURL:  getenv("WAM_PUBLIC_BASE_URL", "http://localhost:8080"),
		DBPath:         getenv("WAM_DB_PATH", "./data/wam.db"),
		DatabaseURL:    getenv("WAM_DATABASE_URL", ""),
		AppDatabaseURL: getenv("WAM_APP_DATABASE_URL", ""),
		WADatabaseURL:  getenv("WAM_WA_DATABASE_URL", ""),
		RedisURL:       getenv("WAM_REDIS_URL", ""),
		DataDir:        getenv("WAM_DATA_DIR", "./data"),
		AdminPassword:  getenv("WAM_ADMIN_PASSWORD", ""),
		AdminEmail:     getenv("WAM_ADMIN_EMAIL", ""),
		SessionSecret:  getenv("WAM_SESSION_SECRET", "dev-secret-change-me-32-bytes!!"),
	}
}

// AuthEnabled reports whether admin auth is enforced.
func (c Config) AuthEnabled() bool {
	return strings.TrimSpace(c.AdminPassword) != ""
}

// UsesPostgres reports whether the SaaS Postgres path is active (either the
// owner DSN or the app-only runtime DSN suffices).
func (c Config) UsesPostgres() bool {
	return strings.TrimSpace(c.DatabaseURL) != "" || strings.TrimSpace(c.AppDatabaseURL) != ""
}

// WAURL resolves the session-store DSN: explicit WAM_WA_DATABASE_URL, else
// the owner DatabaseURL, else the app DSN (works only if session tables
// already exist — the app role cannot run Upgrade DDL).
func (c Config) WAURL() string {
	if strings.TrimSpace(c.WADatabaseURL) != "" {
		return c.WADatabaseURL
	}
	if strings.TrimSpace(c.DatabaseURL) != "" {
		return c.DatabaseURL
	}
	return c.AppDatabaseURL
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
