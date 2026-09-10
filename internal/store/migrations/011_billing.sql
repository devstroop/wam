-- 011_billing: plans, subscriptions, invoices (provider-agnostic core).
--
-- Metering is DERIVED, not counted: monthly sends come from
-- campaign_recipients.sent_at, contacts/accounts/members from COUNT(*).
-- No write amplification, no counter races; quota checks are reads.
-- The payment-gateway branch (Razorpay adapter) writes subscriptions +
-- invoices; this branch owns the money domain shape + enforcement reads.
--
-- Limits JSON: {"accounts":N,"contacts":N,"msgs_per_month":N,"members":N}
-- with -1 = unlimited. Amounts in paise (INR).
CREATE TABLE IF NOT EXISTS plans (
	id TEXT PRIMARY KEY,
	code TEXT NOT NULL UNIQUE,
	name TEXT NOT NULL,
	limits TEXT NOT NULL DEFAULT '{}',
	price_paise INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'))
);
INSERT INTO plans (id, code, name, limits, price_paise) VALUES
	('plan_free', 'free', 'Free',
		'{"accounts":1,"contacts":500,"msgs_per_month":1000,"members":3}', 0),
	('plan_starter', 'starter', 'Starter',
		'{"accounts":3,"contacts":5000,"msgs_per_month":10000,"members":10}', 49900),
	('plan_growth', 'growth', 'Growth',
		'{"accounts":10,"contacts":50000,"msgs_per_month":100000,"members":25}', 199900),
	('plan_scale', 'scale', 'Scale',
		'{"accounts":-1,"contacts":-1,"msgs_per_month":-1,"members":-1}', 499900)
	ON CONFLICT (id) DO NOTHING;

-- One row per org (upserted). NULL plan = free. Trial/active/past_due/
-- canceled mirror the provider state; enforcement treats anything but
-- active/trial as free.
CREATE TABLE IF NOT EXISTS subscriptions (
	org_id TEXT PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
	plan_id TEXT REFERENCES plans(id),
	status TEXT NOT NULL DEFAULT 'trial',
	current_period_end TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	provider_id TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')),
	updated_at TEXT NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'))
);

CREATE TABLE IF NOT EXISTS invoices (
	id TEXT PRIMARY KEY,
	org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
	provider TEXT NOT NULL DEFAULT '',
	provider_id TEXT NOT NULL DEFAULT '',
	amount_paise INTEGER NOT NULL DEFAULT 0,
	currency TEXT NOT NULL DEFAULT 'INR',
	status TEXT NOT NULL DEFAULT 'open',
	created_at TEXT NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'))
);
CREATE INDEX IF NOT EXISTS idx_invoices_org ON invoices(org_id);

-- Tenant RLS (fail-closed): request paths read via scoped handles.
-- Provider webhooks carry no session: the payment-gateway branch reads/
-- writes these tables with the owner handle by provider/org id.
ALTER TABLE subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE subscriptions FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS org_isolation ON subscriptions;
CREATE POLICY org_isolation ON subscriptions
	USING (org_id = current_setting('app.org_id', true))
	WITH CHECK (org_id = current_setting('app.org_id', true));
ALTER TABLE invoices ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoices FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS org_isolation ON invoices;
CREATE POLICY org_isolation ON invoices
	USING (org_id = current_setting('app.org_id', true))
	WITH CHECK (org_id = current_setting('app.org_id', true));

-- Runtime grants for the new tables (explicit per-table convention from 003).
-- plans: read-only for the app role (managed out-of-band / later admin UI).
GRANT SELECT ON plans TO wam_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON subscriptions, invoices TO wam_app;
