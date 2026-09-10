// Command wam is the WhatsApp Marketing (WAM) server entrypoint.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/devstroop/wam/internal/config"
	"github.com/devstroop/wam/internal/server"
	"github.com/devstroop/wam/internal/store"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		runMigrate(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "migrate-schema" {
		runMigrateSchema(os.Args[2:])
		return
	}
	cfg := config.Load()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel(),
	}))
	slog.SetDefault(logger)

	srv := server.New(cfg, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("wam listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "err", err)
		os.Exit(1)
	}
	logger.Info("shutdown complete")
}

// runMigrateSchema applies pending Postgres schema migrations and exits.
// Run with an owner/superuser DSN; the runtime app role cannot run DDL.
func runMigrateSchema(args []string) {
	fs := flag.NewFlagSet("migrate-schema", flag.ExitOnError)
	dbURL := fs.String("database-url", os.Getenv("WAM_DATABASE_URL"), "Postgres DSN (owner)")
	_ = fs.Parse(args)
	if *dbURL == "" {
		fmt.Fprintln(os.Stderr, "migrate-schema: set --database-url or WAM_DATABASE_URL")
		os.Exit(2)
	}
	db, err := store.OpenPostgres(*dbURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate-schema:", err)
		os.Exit(1)
	}
	_ = db.Close()
	fmt.Println("migrate-schema: up to date")
}

// runMigrate imports a legacy SQLite file into Postgres:
// wam migrate --from-sqlite ./data/wam.db [--database-url ...].
// Falls back to WAM_DB_PATH / WAM_DATABASE_URL env when flags are empty.
func runMigrate(args []string) {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	from := fs.String("from-sqlite", os.Getenv("WAM_DB_PATH"), "source SQLite file")
	dbURL := fs.String("database-url", os.Getenv("WAM_DATABASE_URL"), "target Postgres DSN")
	_ = fs.Parse(args)
	if *from == "" {
		*from = "./data/wam.db"
	}
	if *dbURL == "" {
		fmt.Fprintln(os.Stderr, "migrate: set --database-url or WAM_DATABASE_URL")
		os.Exit(2)
	}
	counts, err := store.MigrateSQLiteToPostgres(*from, *dbURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
	fmt.Println("migrate: done", counts)
}
