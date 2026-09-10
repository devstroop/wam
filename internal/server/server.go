// Package server wires the HTTP router, middleware, handlers and static assets.
//
// Route map:
//
//	GET /healthz, /readyz        system probes (JSON)
//	GET /api-docs/, /api-docs/openapi.yaml   API docs (Swagger UI + spec, dev only)
//	GET /openapi.yaml            spec alias (convenience, dev only)
//	GET /swagger, /swagger/      301 → /api-docs/ (legacy compat, dev only)
//	GET /                        public landing
//	GET /login                   admin login
//	GET /dashboard               marketing home
//	GET /connect                 WhatsApp pairing
//	GET /contacts                audiences
//	GET /groups                  groups
//	GET /campaigns, /campaigns/{id}  campaigns
//	GET /analytics               analytics
//	GET /settings                settings
//	GET /partials/*              htmx fragments (no layout)
//	GET /api/v1/*                JSON APIs
//	GET /static/*                embedded css/js
package server

import (
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/devstroop/wam/internal/auth"
	"github.com/devstroop/wam/internal/campaigns"
	"github.com/devstroop/wam/internal/config"
	"github.com/devstroop/wam/internal/handlers"
	"github.com/devstroop/wam/internal/mail"
	"github.com/devstroop/wam/internal/middleware"
	"github.com/devstroop/wam/internal/store"
	"github.com/devstroop/wam/internal/views"
	"github.com/devstroop/wam/internal/wa"
	webassets "github.com/devstroop/wam/web"
)

// New builds the *http.Server with all routes and middleware.
// It opens the store (Postgres when WAM_DATABASE_URL is set, else SQLite),
// creating DataDir, panicking on fatal errors like the existing
// views/static setup.
func New(cfg config.Config, log *slog.Logger) *http.Server {
	if cfg.DataDir != "" {
		if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
			panic("datadir: " + err.Error())
		}
	}
	db, ownerDB, err := store.OpenAuto(cfg.DBPath, cfg.DatabaseURL, cfg.AppDatabaseURL)
	if err != nil {
		if cfg.UsesPostgres() && cfg.AppDatabaseURL != "" {
			panic("store: run wam migrate-schema with the owner DSN first: " + err.Error())
		}
		panic("store: " + err.Error())
	}
	// Postgres boots the UMS stack (DB users + RBAC). If no users exist and
	// an admin password is configured, seed the first admin so single-env
	// bootstrap keeps working (email via WAM_ADMIN_EMAIL).
	if cfg.UsesPostgres() {
		seedAdmin(ownerDB, cfg, log)
	}
	sess := auth.New(cfg.AdminPassword, cfg.SessionSecret)
	const htmxVersion = "4.0.0"
	const swaggerUIVersion = "5.17.14"
	v, err := views.New(map[string]any{
		"AppName":          "WAM",
		"AppTagline":       "WhatsApp Marketing",
		"Env":              cfg.Env,
		"BaseURL":          cfg.PublicBaseURL,
		"HTMXVersion":      htmxVersion,
		"SwaggerUIVersion": swaggerUIVersion,
		"IsDev":            cfg.IsDev(),
		"Year":             time.Now().Year(),
	})
	if err != nil {
		panic("views: " + err.Error())
	}
	wasvc := wa.NewManager(cfg.DBPath, cfg.WAURL(), !cfg.UsesPostgres(), log)
	if cfg.UsesPostgres() {
		// Pairing callbacks persist device/account state; legacy sessions
		// upgrade into a default account row on first boot after migration.
		wasvc.OnPaired = func(accountID, jid string) {
			persistPairedDevice(db, accountID, jid, log)
		}
		ensureLegacyAccount(ownerDB, wasvc, log)
		go wasvc.AutoConnect(connectEntries(ownerDB, log))
	} else {
		go wasvc.AutoConnect(nil)
	}
	web := &handlers.Web{Views: v, Store: db, WA: wasvc}
	authH := &handlers.Auth{Session: sess, Login: v.RenderPage}
	conn := &handlers.Connection{WA: wasvc, Views: v, Store: db}
	msg := &handlers.Messaging{WA: wasvc, Store: db}
	contacts := &handlers.Contacts{Store: db, Views: v}
	groups := &handlers.Groups{Store: db, Views: v}
	tmpls := &handlers.Templates{Store: db, Views: v}
	camps := &handlers.Campaigns{Store: db, Views: v}
	analytics := &handlers.Analytics{Store: db, Views: v}

	// Sender worker (single-flight). Started with the server; receipts flow
	// back through wa.OnReceipt. The worker gets the owner handle for org
	// enumeration plus the runtime handle: all tenant data access runs
	// inside WithOrg on the runtime handle (fail-closed RLS).
	worker := campaigns.New(db, ownerDB, wasvc, log)
	worker.Start()

	mux := http.NewServeMux()

	// System.
	mux.HandleFunc("GET /healthz", handlers.Healthz)
	mux.HandleFunc("GET /readyz", handlers.Readyz)

	// API docs (canonical). Dev only: unregistered paths fall through to
	// web.Index, which 404s anything but "/".
	if cfg.IsDev() {
		mux.HandleFunc("GET /api-docs", handlers.APIDocs(swaggerUIVersion))
		mux.HandleFunc("GET /api-docs/", handlers.APIDocs(swaggerUIVersion))
		mux.HandleFunc("GET /api-docs/openapi.yaml", handlers.OpenAPIYAML)
		// Spec alias + legacy redirects.
		mux.HandleFunc("GET /openapi.yaml", handlers.OpenAPIYAML)
		mux.HandleFunc("GET /swagger", handlers.SwaggerRedirect)
		mux.HandleFunc("GET /swagger/", handlers.SwaggerRedirect)
		mux.HandleFunc("GET /swagger/openapi.yaml", handlers.SwaggerRedirect)
	}

	// Web (htmx): public + dashboard shell.
	mux.HandleFunc("GET /", web.Index)
	if cfg.UsesPostgres() {
		// UMS stack replaces single-admin auth (legacy kept for SQLite).
		ums := &handlers.UMS{Store: db, Views: v, Session: sess, Mailer: mail.LogMailer{Log: log}, BaseURL: cfg.PublicBaseURL}
		mux.HandleFunc("GET /login", ums.LoginPage)
		mux.HandleFunc("POST /login", ums.LoginSubmit)
		mux.HandleFunc("POST /logout", ums.LogoutSubmit)
		mux.HandleFunc("GET /logout", ums.LogoutSubmit)
		mux.HandleFunc("GET /signup", ums.SignupPage)
		mux.HandleFunc("POST /signup", ums.SignupSubmit)
		mux.HandleFunc("GET /verify", ums.VerifyPage)
		mux.HandleFunc("GET /forgot", ums.ForgotPage)
		mux.HandleFunc("POST /forgot", ums.ForgotSubmit)
		mux.HandleFunc("GET /reset", ums.ResetPage)
		mux.HandleFunc("POST /reset", ums.ResetSubmit)
		mux.HandleFunc("GET /invite/accept", ums.InviteAcceptPage)
		mux.HandleFunc("POST /invite/accept", ums.InviteAcceptSubmit)
		mux.HandleFunc("GET /api/v1/auth/me", ums.Me)
		mux.HandleFunc("POST /api/v1/auth/switch", ums.SwitchSubmit)
		mux.HandleFunc("GET /api/v1/members", ums.ListMembers)
		mux.HandleFunc("POST /api/v1/members/invite", ums.InviteMember)
		mux.HandleFunc("PATCH /api/v1/members/{user_id}", ums.UpdateMemberRole)
		mux.HandleFunc("DELETE /api/v1/members/{user_id}", ums.RemoveMember)
		mux.HandleFunc("GET /api/v1/grants", ums.ListGrants)
		mux.HandleFunc("POST /api/v1/grants", ums.SetGrantSubmit)
		mux.HandleFunc("DELETE /api/v1/grants", ums.RemoveGrantSubmit)
	} else {
		mux.HandleFunc("GET /login", authH.LoginPage)
		mux.HandleFunc("POST /login", authH.LoginSubmit)
		mux.HandleFunc("POST /logout", authH.Logout)
		mux.HandleFunc("GET /logout", authH.Logout)
	}
	mux.HandleFunc("GET /dashboard", web.Dashboard)
	mux.HandleFunc("GET /connect", web.Connect)
	mux.HandleFunc("GET /contacts", web.Contacts)
	mux.HandleFunc("GET /groups", web.Groups)
	mux.HandleFunc("GET /templates", tmpls.Page)
	mux.HandleFunc("GET /campaigns", web.Campaigns)
	mux.HandleFunc("GET /campaigns/{id}", web.CampaignDetail)
	mux.HandleFunc("GET /analytics", web.Analytics)
	mux.HandleFunc("GET /settings", web.Settings)
	// Live campaign fragments.
	mux.HandleFunc("GET /partials/campaign-rows", camps.Rows)
	mux.HandleFunc("GET /partials/recipient-rows", camps.RecipientRows)
	// Live audience fragments.
	mux.HandleFunc("GET /partials/contact-rows", contacts.Rows)
	mux.HandleFunc("GET /partials/group-rows", groups.Rows)
	mux.HandleFunc("GET /partials/template-rows", tmpls.Rows)
	mux.HandleFunc("GET /partials/audience-contacts", camps.AudienceContacts)
	// Live WhatsApp fragments.
	// sidebar-connection: compose-style block; connection-detail: rich status card;
	// connect-dialog: dialog body (status or QR/Pair tabs).
	mux.HandleFunc("GET /partials/sidebar-connection", conn.Sidebar)
	mux.HandleFunc("GET /partials/connection-detail", conn.Detail)
	mux.HandleFunc("GET /partials/connect-dialog", conn.Dialog)
	mux.HandleFunc("GET /partials/qr", conn.QR)

	// API v1.
	mux.HandleFunc("GET /api/v1/connection", conn.Status)
	mux.HandleFunc("POST /api/v1/connection/qr", conn.QR)
	mux.HandleFunc("POST /api/v1/connection/pair", conn.Pair)
	mux.HandleFunc("POST /api/v1/connection/logout", conn.Logout)
	// WhatsApp accounts (multi-account; no-ops on legacy SQLite).
	accts := &handlers.Accounts{Store: db, Mgr: wasvc}
	mux.HandleFunc("GET /api/v1/accounts", accts.List)
	mux.HandleFunc("POST /api/v1/accounts", accts.Create)
	mux.HandleFunc("GET /api/v1/accounts/{id}", accts.Get)
	mux.HandleFunc("PATCH /api/v1/accounts/{id}", accts.UpdateLabel)
	mux.HandleFunc("DELETE /api/v1/accounts/{id}", accts.Delete)
	mux.HandleFunc("POST /api/v1/accounts/{id}/qr", accts.QR)
	mux.HandleFunc("POST /api/v1/accounts/{id}/pair", accts.Pair)
	mux.HandleFunc("POST /api/v1/accounts/{id}/logout", accts.Logout)
	// Contacts + groups/labels (live SQLite).
	mux.HandleFunc("GET /api/v1/contacts", contacts.List)
	mux.HandleFunc("POST /api/v1/contacts", contacts.Create)
	mux.HandleFunc("GET /api/v1/contacts/{id}", contacts.Get)
	mux.HandleFunc("PATCH /api/v1/contacts/{id}", contacts.Update)
	mux.HandleFunc("DELETE /api/v1/contacts/{id}", contacts.Delete)
	mux.HandleFunc("POST /api/v1/contacts/import", contacts.Import)
	mux.HandleFunc("POST /api/v1/contacts/check", msg.Check)
	mux.HandleFunc("GET /api/v1/groups", groups.List)
	mux.HandleFunc("POST /api/v1/groups", groups.Create)
	mux.HandleFunc("GET /api/v1/groups/{id}", groups.Get)
	mux.HandleFunc("PATCH /api/v1/groups/{id}", groups.Update)
	mux.HandleFunc("DELETE /api/v1/groups/{id}", groups.Delete)
	mux.HandleFunc("POST /api/v1/groups/{id}/members", groups.SetMembers)
	// Templates.
	mux.HandleFunc("GET /api/v1/templates", tmpls.List)
	mux.HandleFunc("POST /api/v1/templates", tmpls.Create)
	mux.HandleFunc("GET /api/v1/templates/{id}", tmpls.Get)
	mux.HandleFunc("PATCH /api/v1/templates/{id}", tmpls.Update)
	mux.HandleFunc("DELETE /api/v1/templates/{id}", tmpls.Delete)
	mux.HandleFunc("POST /api/v1/templates/{id}/duplicate", tmpls.Duplicate)
	// Campaigns (live SQLite + sender worker).
	mux.HandleFunc("GET /api/v1/campaigns", camps.List)
	mux.HandleFunc("POST /api/v1/campaigns", camps.Create)
	mux.HandleFunc("GET /api/v1/campaigns/{id}", camps.Get)
	mux.HandleFunc("PATCH /api/v1/campaigns/{id}", camps.Update)
	mux.HandleFunc("DELETE /api/v1/campaigns/{id}", camps.Delete)
	mux.HandleFunc("POST /api/v1/campaigns/{id}/start", camps.Start)
	mux.HandleFunc("POST /api/v1/campaigns/{id}/pause", camps.Pause)
	mux.HandleFunc("POST /api/v1/campaigns/{id}/resume", camps.Resume)
	mux.HandleFunc("POST /api/v1/campaigns/{id}/cancel", camps.Cancel)
	mux.HandleFunc("GET /api/v1/campaigns/{id}/recipients", camps.Recipients)
	mux.HandleFunc("GET /api/v1/analytics/campaigns/{id}", camps.Funnel)
	mux.HandleFunc("GET /api/v1/analytics/overview", analytics.Overview)
	mux.HandleFunc("GET /api/v1/campaigns/{id}/export", analytics.Export)
	mux.HandleFunc("GET /partials/analytics-rows", analytics.Rows)
	mux.HandleFunc("GET /partials/analytics-chart", analytics.Chart)
	mux.HandleFunc("POST /api/v1/messages/send", msg.Send)

	// Static assets (embedded web/static).
	staticFS, err := fs.Sub(webassets.StaticFS, "static")
	if err != nil {
		panic("static: " + err.Error())
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	var h http.Handler = mux
	if cfg.UsesPostgres() {
		h = sess.RequireUMS(db, log)(h)
	} else {
		h = sess.RequireAdmin(h)
	}
	h = middleware.SecurityHeaders(h)
	h = middleware.CORS(h)
	h = middleware.RequestID(h)
	h = middleware.Recover(log)(h)
	h = middleware.Logger(log)(h)

	return &http.Server{
		Addr:              cfg.Addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}
