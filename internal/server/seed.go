package server

import (
	"log/slog"
	"strings"

	"github.com/devstroop/wam/internal/auth"
	"github.com/devstroop/wam/internal/config"
	"github.com/devstroop/wam/internal/store"
)

// seedAdmin creates the first admin on a fresh Postgres boot when
// WAM_ADMIN_PASSWORD is set (single-env bootstrap). Email via
// WAM_ADMIN_EMAIL, default admin@wam.local. No-op otherwise.
func seedAdmin(owner *store.DB, cfg config.Config, log *slog.Logger) {
	if strings.TrimSpace(cfg.AdminPassword) == "" {
		return
	}
	n, err := owner.UserCount()
	if err != nil || n > 0 {
		return
	}
	email := strings.ToLower(strings.TrimSpace(cfg.AdminEmail))
	if email == "" {
		email = "admin@wam.local"
	}
	hash, err := auth.HashPassword(cfg.AdminPassword)
	if err != nil {
		log.Warn("seed: weak admin password, skipping seed", "err", err)
		return
	}
	user, err := owner.CreateUser(email, "Admin", hash)
	if err != nil {
		log.Warn("seed: create admin failed", "err", err)
		return
	}
	if err := owner.VerifyUserEmail(user.ID); err != nil {
		log.Warn("seed: verify admin failed", "err", err)
		return
	}
	org, err := owner.DefaultOrg()
	if err != nil {
		log.Warn("seed: default org failed", "err", err)
		return
	}
	if _, err := owner.AddMembership(user.ID, org.ID, store.RoleAdmin); err != nil {
		log.Warn("seed: admin membership failed", "err", err)
		return
	}
	log.Info("seed: initial admin created", "email", email, "org", org.ID)
}
