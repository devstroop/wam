package server

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/devstroop/wam/internal/auth"
	"github.com/devstroop/wam/internal/config"
	"github.com/devstroop/wam/internal/store"
	"github.com/devstroop/wam/internal/wa"
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

// persistPairedDevice records a successful pairing on the account row.
// Owner handle: callbacks carry no org context (fail-closed RLS would hide
// the row); the update is PK-scoped to the paired account.
func persistPairedDevice(owner *store.DB, accountID, jid string, log *slog.Logger) {
	phone := ""
	if user, _, ok := strings.Cut(jid, "@"); ok {
		phone = "+" + strings.TrimPrefix(user, "+")
	}
	if err := owner.MarkAccountPaired(accountID, phone, jid, jid); err != nil {
		log.Warn("seed: record pairing failed", "account", accountID, "err", err)
		return
	}
	log.Info("wa: pairing recorded", "account", accountID, "phone", phone)
}

// ensureLegacyAccount backfills one account row for pre-multi-account
// sessions (single-device upgrades): if org_default has no accounts but the
// session store holds a paired device, adopt it.
func ensureLegacyAccount(owner *store.DB, mgr *wa.Manager, log *slog.Logger) {
	accounts, err := owner.AccountsByOrg(store.DefaultOrgID)
	if err != nil {
		log.Warn("seed: legacy account check failed", "err", err)
		return
	}
	if len(accounts) > 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	jid, err := mgr.ProbeFirstDevice(ctx)
	if err != nil || jid == "" {
		return
	}
	phone := ""
	if user, _, ok := strings.Cut(jid, "@"); ok {
		phone = "+" + strings.TrimPrefix(user, "+")
	}
	a, err := owner.CreateAccount(store.DefaultOrgID, "My WhatsApp number", "")
	if err != nil {
		log.Warn("seed: legacy account row failed", "err", err)
		return
	}
	if err := owner.MarkAccountPaired(a.ID, phone, jid, jid); err != nil {
		log.Warn("seed: legacy account pairing failed", "err", err)
		return
	}
	log.Info("seed: adopted legacy WhatsApp session", "account", a.ID, "phone", phone)
}

// connectEntries lists paired accounts for boot auto-connect (owner:
// fail-closed RLS hides rows from unscoped app handles).
func connectEntries(owner *store.DB, log *slog.Logger) []wa.AccountRef {
	accounts, err := owner.AllAccounts()
	if err != nil {
		log.Warn("wa: account list failed", "err", err)
		return nil
	}
	out := make([]wa.AccountRef, 0, len(accounts))
	for _, a := range accounts {
		if a.DeviceJID == "" || a.Status == store.AccountLoggedOut {
			continue
		}
		out = append(out, wa.AccountRef{ID: a.ID, DeviceJID: a.DeviceJID})
	}
	return out
}
