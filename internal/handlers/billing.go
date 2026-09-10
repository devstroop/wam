package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/devstroop/wam/internal/store"
)

// Billing serves plan/usage/subscription/invoices (PG SaaS path only).
type Billing struct {
	Store *store.DB
}

// Overview returns the effective plan + derived usage (+ raw subscription).
func (h *Billing) Overview(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	plan, err := db.EffectivePlan(id.OrgID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	usage, err := db.UsageForOrg(id.OrgID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	sub, err := db.GetSubscription(id.OrgID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeStoreError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"plan": plan, "usage": usage, "subscription": sub,
	})
}

// Plans lists tiers (any member).
func (h *Billing) Plans(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	plans, err := db.ListPlans()
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": plans})
}

// SetSubscription switches tier (admin, billing:manage). Manual switches
// land as trial; the payment gateway upgrades to active on webhook.
// Paid use without payment is a product violation, not a code path.
func (h *Billing) SetSubscription(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, store.PermBillingManage)
	if !ok {
		return
	}
	var body struct {
		PlanID string `json:"plan_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.PlanID) == "" {
		WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "plan_id is required")
		return
	}
	sub, err := db.SetSubscription(id.OrgID, body.PlanID, "trial", "", "", "")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, sub)
}

// Invoices lists charges (any member).
func (h *Billing) Invoices(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	invs, err := db.InvoicesForOrg(id.OrgID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": invs})
}
