package server

import (
	"log/slog"
	"os"
	"testing"

	"github.com/devstroop/wam/internal/config"
)

// TestNewRegistersRoutes guards route wiring: duplicate/conflicting mux
// patterns panic inside New (bit us with a doubled grants DELETE).
// SQLite keeps it hermetic (no PG, no network); WA auto-connect is lazy.
func TestNewRegistersRoutes(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{
		Addr:          "127.0.0.1:0",
		Env:           "test",
		PublicBaseURL: "http://test",
		DBPath:        dir + "/wam.db",
		DataDir:       dir,
	}
	srv := New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if srv == nil || srv.Handler == nil {
		t.Fatal("nil server")
	}
}
