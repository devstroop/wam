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
	"github.com/devstroop/wam/internal/middleware"
	"github.com/devstroop/wam/internal/store"
	"github.com/devstroop/wam/internal/views"
	"github.com/devstroop/wam/internal/wa"
	webassets "github.com/devstroop/wam/web"
)

// New builds the *http.Server with all routes and middleware.
// It opens SQLite (creating DataDir), panicking on fatal errors like the
// existing views/static setup.
func New(cfg config.Config, log *slog.Logger) *http.Server {
	if cfg.DataDir != "" {
		if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
			panic("datadir: " + err.Error())
		}
	}
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		panic("store: " + err.Error())
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
	wasvc := wa.New(cfg.DBPath, log)
	go wasvc.AutoConnect()
	web := &handlers.Web{Views: v, Store: db, WA: wasvc}
	authH := &handlers.Auth{Session: sess, Login: v.RenderPage}
	conn := &handlers.Connection{WA: wasvc, Views: v}
	msg := &handlers.Messaging{WA: wasvc}
	contacts := &handlers.Contacts{Store: db, Views: v}
	groups := &handlers.Groups{Store: db, Views: v}
	tmpls := &handlers.Templates{Store: db, Views: v}
	camps := &handlers.Campaigns{Store: db, Views: v}
	analytics := &handlers.Analytics{Store: db, Views: v}

	// Sender worker (single-flight). Started with the server; receipts flow
	// back through wa.OnReceipt.
	worker := campaigns.New(db, wasvc, log)
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
	mux.HandleFunc("GET /login", authH.LoginPage)
	mux.HandleFunc("POST /login", authH.LoginSubmit)
	mux.HandleFunc("POST /logout", authH.Logout)
	mux.HandleFunc("GET /logout", authH.Logout)
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
	h = sess.RequireAdmin(h)
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
