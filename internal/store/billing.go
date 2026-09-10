package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Limits bound an org. -1 = unlimited.
type Limits struct {
	Accounts     int `json:"accounts"`
	Contacts     int `json:"contacts"`
	MsgsPerMonth int `json:"msgs_per_month"`
	Members      int `json:"members"`
}

// Plan is a price tier (seeded by migration 011).
type Plan struct {
	ID         string `json:"id"`
	Code       string `json:"code"`
	Name       string `json:"name"`
	Limits     Limits `json:"limits"`
	PricePaise int    `json:"pricePaise"`
}

// Subscription pins an org to a plan. Anything but trial/active enforces
// the free tier (provider state mirrors here via webhooks).
type Subscription struct {
	OrgID            string `json:"orgId"`
	PlanID           string `json:"planId,omitempty"`
	Status           string `json:"status"`
	CurrentPeriodEnd string `json:"currentPeriodEnd,omitempty"`
	Provider         string `json:"provider,omitempty"`
	ProviderID       string `json:"providerId,omitempty"`
}

// Invoice records one charge (provider adapter writes these).
type Invoice struct {
	ID          string `json:"id"`
	OrgID       string `json:"orgId"`
	Provider    string `json:"provider,omitempty"`
	ProviderID  string `json:"providerId,omitempty"`
	AmountPaise int    `json:"amountPaise"`
	Currency    string `json:"currency"`
	Status      string `json:"status"`
	CreatedAt   string `json:"createdAt"`
}

// Usage is derived metering (no counters): counts straight from data.
type Usage struct {
	Period        string `json:"period"`
	Accounts      int    `json:"accounts"`
	Contacts      int    `json:"contacts"`
	Members       int    `json:"members"`
	MsgsThisMonth int    `json:"msgsThisMonth"`
}

// QuotaError denies an action past plan limits (handlers map to 402).
type QuotaError struct {
	Resource string
	Limit    int
	Plan     string
}

func (e *QuotaError) Error() string {
	return fmt.Sprintf("%s quota exceeded (limit %d on %s plan)", e.Resource, e.Limit, e.Plan)
}

func billingPG(db *DB) error {
	if !db.IsPostgres() {
		return fmt.Errorf("store: billing requires postgres")
	}
	return nil
}

// ListPlans returns tiers cheapest-first.
func (db *DB) ListPlans() ([]Plan, error) {
	if err := billingPG(db); err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT id, code, name, limits, price_paise FROM plans ORDER BY price_paise, code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Plan
	for rows.Next() {
		var p Plan
		var limits string
		if err := rows.Scan(&p.ID, &p.Code, &p.Name, &limits, &p.PricePaise); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(limits), &p.Limits)
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPlan fetches one tier by id or code.
func (db *DB) GetPlan(idOrCode string) (*Plan, error) {
	if err := billingPG(db); err != nil {
		return nil, err
	}
	p := &Plan{}
	var limits string
	if err := db.QueryRow(`SELECT id, code, name, limits, price_paise FROM plans WHERE id = ? OR code = ?`,
		idOrCode, idOrCode).Scan(&p.ID, &p.Code, &p.Name, &limits, &p.PricePaise); err != nil {
		if err == sql.ErrNoRows {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	_ = json.Unmarshal([]byte(limits), &p.Limits)
	return p, nil
}

// EffectivePlan resolves the enforcing tier: trial/active subscription plan,
// else free. Missing subscription row also means free.
func (db *DB) EffectivePlan(orgID string) (*Plan, error) {
	if err := billingPG(db); err != nil {
		return nil, err
	}
	free, err := db.GetPlan("free")
	if err != nil {
		return nil, err
	}
	var planID, status string
	if err := db.QueryRow(`SELECT plan_id, status FROM subscriptions WHERE org_id = ?`, orgID).
		Scan(&planID, &status); err != nil {
		if err == sql.ErrNoRows {
			return free, nil
		}
		return nil, err
	}
	if status != "trial" && status != "active" {
		return free, nil
	}
	p, err := db.GetPlan(planID)
	if err != nil {
		return free, nil
	}
	return p, nil
}

// monthCutoff returns (YYYY-MM period, RFC3339 month-start UTC).
func monthCutoff(now time.Time) (string, string) {
	now = now.UTC()
	return now.Format("2006-01"),
		time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
}

// UsageForOrg derives current consumption (scoped reads; RLS applies).
func (db *DB) UsageForOrg(orgID string) (Usage, error) {
	if err := billingPG(db); err != nil {
		return Usage{}, err
	}
	period, cutoff := monthCutoff(time.Now())
	var u Usage
	u.Period = period
	queries := []struct {
		dest *int
		q    string
		args []any
	}{
		{&u.Accounts, `SELECT COUNT(*) FROM wa_accounts WHERE org_id = ?`, []any{orgID}},
		{&u.Contacts, `SELECT COUNT(*) FROM contacts WHERE org_id = ?`, []any{orgID}},
		{&u.Members, `SELECT COUNT(*) FROM memberships WHERE org_id = ?`, []any{orgID}},
		{&u.MsgsThisMonth, `SELECT COUNT(*) FROM campaign_recipients
			WHERE org_id = ? AND status IN ('sent','delivered','read','replied') AND sent_at >= ?`,
			[]any{orgID, cutoff}},
	}
	for _, qq := range queries {
		if err := db.QueryRow(qq.q, qq.args...).Scan(qq.dest); err != nil {
			return Usage{}, err
		}
	}
	return u, nil
}

// CheckSendQuota denies when the month's message quota is exhausted
// (worker runtime gate; Start-time check lives in the handlers).
func (db *DB) CheckSendQuota() error {
	if !db.IsPostgres() {
		return nil
	}
	return db.CheckQuota(db.orgOr(DefaultOrgID), "message")
}

// CheckQuota verifies one more unit of resource fits the effective plan.
func (db *DB) CheckQuota(orgID, kind string) error {
	if err := billingPG(db); err != nil {
		return err
	}
	plan, err := db.EffectivePlan(orgID)
	if err != nil {
		return err
	}
	usage, err := db.UsageForOrg(orgID)
	if err != nil {
		return err
	}
	over := func(used, max int) bool { return max >= 0 && used+1 > max }
	switch kind {
	case "account":
		if over(usage.Accounts, plan.Limits.Accounts) {
			return &QuotaError{"accounts", plan.Limits.Accounts, plan.Code}
		}
	case "contact":
		if over(usage.Contacts, plan.Limits.Contacts) {
			return &QuotaError{"contacts", plan.Limits.Contacts, plan.Code}
		}
	case "member":
		if over(usage.Members, plan.Limits.Members) {
			return &QuotaError{"members", plan.Limits.Members, plan.Code}
		}
	case "message":
		if over(usage.MsgsThisMonth, plan.Limits.MsgsPerMonth) {
			return &QuotaError{"messages this month", plan.Limits.MsgsPerMonth, plan.Code}
		}
	default:
		return fmt.Errorf("unknown quota kind %q", kind)
	}
	return nil
}

// SetSubscription upserts the org's subscription (self-serve + webhooks).
// Empty planID clears to free.
func (db *DB) SetSubscription(orgID, planID, status, periodEnd, provider, providerID string) (*Subscription, error) {
	if err := billingPG(db); err != nil {
		return nil, err
	}
	resolvedID := ""
	if planID != "" {
		p, err := db.GetPlan(planID)
		if err != nil {
			return nil, fmt.Errorf("unknown plan %q", planID)
		}
		resolvedID = p.ID
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO subscriptions (org_id, plan_id, status, current_period_end, provider, provider_id, updated_at)
		VALUES (?, NULLIF(?, ''), ?, ?, ?, ?, ?)
		ON CONFLICT (org_id) DO UPDATE SET plan_id = EXCLUDED.plan_id, status = EXCLUDED.status,
			current_period_end = EXCLUDED.current_period_end, provider = EXCLUDED.provider,
			provider_id = EXCLUDED.provider_id, updated_at = EXCLUDED.updated_at`,
		orgID, resolvedID, status, periodEnd, provider, providerID, now); err != nil {
		return nil, err
	}
	return db.GetSubscription(orgID)
}

// GetSubscription fetches the raw row (nil plan when none).
func (db *DB) GetSubscription(orgID string) (*Subscription, error) {
	if err := billingPG(db); err != nil {
		return nil, err
	}
	s := &Subscription{}
	var planID sql.NullString
	if err := db.QueryRow(`SELECT org_id, plan_id, status, current_period_end, provider, provider_id
		FROM subscriptions WHERE org_id = ?`, orgID).
		Scan(&s.OrgID, &planID, &s.Status, &s.CurrentPeriodEnd, &s.Provider, &s.ProviderID); err != nil {
		if err == sql.ErrNoRows {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	s.PlanID = planID.String
	return s, nil
}

// RecordInvoice inserts a charge row (provider adapters).
func (db *DB) RecordInvoice(orgID, provider, providerID string, amountPaise int, currency, status string) (*Invoice, error) {
	if err := billingPG(db); err != nil {
		return nil, err
	}
	if currency == "" {
		currency = "INR"
	}
	if status == "" {
		status = "open"
	}
	inv := &Invoice{ID: uuid.NewString(), OrgID: orgID, Provider: provider, ProviderID: providerID,
		AmountPaise: amountPaise, Currency: currency, Status: status}
	if _, err := db.Exec(`INSERT INTO invoices (id, org_id, provider, provider_id, amount_paise, currency, status)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, inv.ID, inv.OrgID, inv.Provider, inv.ProviderID, inv.AmountPaise, inv.Currency, inv.Status); err != nil {
		return nil, err
	}
	return inv, nil
}

// InvoicesForOrg lists charges newest-first.
func (db *DB) InvoicesForOrg(orgID string) ([]Invoice, error) {
	if err := billingPG(db); err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT id, org_id, provider, provider_id, amount_paise, currency, status, created_at
		FROM invoices WHERE org_id = ? ORDER BY created_at DESC, id DESC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invoice
	for rows.Next() {
		var inv Invoice
		if err := rows.Scan(&inv.ID, &inv.OrgID, &inv.Provider, &inv.ProviderID, &inv.AmountPaise,
			&inv.Currency, &inv.Status, &inv.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}
