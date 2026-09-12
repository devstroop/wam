package handlers

import (
	"net/http"
	"strconv"

	"github.com/devstroop/wam/internal/middleware"
	"github.com/devstroop/wam/internal/store"
	"github.com/devstroop/wam/internal/views"
	"github.com/devstroop/wam/internal/wa"
)

// Web holds HTML/htmx handlers.
type Web struct {
	Views *views.Views
	Store *store.DB
	WA    *wa.Manager
}

// Index renders the public landing page. It is the catch-all for GET /,
// so it must reject non-root paths (Go's "/" pattern matches everything
// not otherwise matched) to avoid masking 404s for removed demo routes.
func (h *Web) Index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	h.Views.RenderPage(w, "index.html", map[string]any{"Title": "WAM"})
}

// Dashboard is the marketing home (live counts, funnel, WA status).
func (h *Web) Dashboard(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	var contacts, groups, campaigns, queued int
	if db != nil {
		contacts, groups, campaigns, queued = db.Stats()
	}
	var funnel store.Funnel
	if db != nil {
		if o, err := db.Overview(); err == nil {
			funnel = o.Funnel
		}
	}
	conn := h.connBadge(id, db)
	h.Views.RenderApp(w, "dashboard.html", map[string]any{
		"Title": "Dashboard", "Nav": "dashboard",
		"Contacts": contacts, "Groups": groups,
		"Campaigns": campaigns, "Queued": queued,
		"Funnel": funnel, "Conn": conn,
	})
}

// connBadge resolves the default account status for page chrome.
// Best-effort: any ambiguity yields the disconnected display, never an error.
func (h *Web) connBadge(id middleware.Identity, db *store.DB) map[string]any {
	conn := map[string]any{"Connected": false, "LoggedIn": false}
	if h.WA == nil || db == nil {
		return conn
	}
	aid := store.LegacyAccountID
	if db.IsPostgres() {
		var err error
		if aid, err = ResolveViewerAccount(db, id, ""); err != nil {
			return conn
		}
	}
	st := h.WA.Status(aid)
	return map[string]any{"Connected": st.Connected, "LoggedIn": st.LoggedIn, "Phone": st.Phone}
}

// Connect shows WhatsApp pairing status (QR/phone).
func (h *Web) Connect(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	isConnected := false
	if h.WA != nil {
		if aid, derr := ResolveViewerAccount(db, id, r.URL.Query().Get("account")); derr == nil {
			st := h.WA.Status(aid)
			isConnected = st.Connected && st.LoggedIn
		}
	}
	var accounts []store.Account
	showAccounts := false
	if db != nil && db.IsPostgres() {
		showAccounts = true
		if as, err := db.AccountsByOrg(id.OrgID); err == nil {
			accounts = visibleAccounts(id, as)
		}
	}
	h.Views.RenderApp(w, "connect.html", map[string]any{"Title": "Connect", "Nav": "connect", "IsConnected": isConnected, "Accounts": accounts, "ShowAccounts": showAccounts})
}

// accountRow is one WhatsApp number with live pairing state for pages.
type accountRow struct {
	store.Account
	Connected bool
	LoggedIn  bool
}

// accountRows lists org accounts with live status; mineOnly filters to
// grant-visible accounts (admins see all either way).
func (h *Web) accountRows(db *store.DB, id middleware.Identity, mineOnly bool) []accountRow {
	rows := []accountRow{}
	if db == nil || !db.IsPostgres() {
		return rows
	}
	accounts, err := db.AccountsByOrg(id.OrgID)
	if err != nil {
		return rows
	}
	if mineOnly {
		accounts = visibleAccounts(id, accounts)
	}
	for _, a := range accounts {
		row := accountRow{Account: a}
		if h.WA != nil {
			st := h.WA.Status(a.ID)
			row.Connected, row.LoggedIn = st.Connected, st.LoggedIn
		}
		rows = append(rows, row)
	}
	return rows
}

// Accounts renders the caller's WhatsApp numbers (grants-filtered for
// members; pairing controls need accounts:pair).
func (h *Web) Accounts(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	h.Views.RenderApp(w, "accounts.html", map[string]any{
		"Title": "Numbers", "Nav": "accounts",
		"ShowAccounts": db != nil && db.IsPostgres(),
		"Accounts":     h.accountRows(db, id, true),
		"CanPair":      store.Can(id.Role, store.PermAccountsPair),
	})
}

// AdminAccounts renders every org number with lifecycle controls.
func (h *Web) AdminAccounts(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, store.PermMembersManage)
	if !ok {
		return
	}
	orgName := "Local workspace"
	if db != nil && db.IsPostgres() {
		if org, err := db.GetOrg(id.OrgID); err == nil {
			orgName = org.Name
		}
	}
	h.Views.RenderAdmin(w, "admin/accounts.html", map[string]any{
		"Title": "Numbers", "Nav": "admin-accounts",
		"OrgName":      orgName,
		"ShowAccounts": db != nil && db.IsPostgres(),
		"Accounts":     h.accountRows(db, id, false),
	})
}

// Contacts lists audiences (live table + group filter options).
func (h *Web) Contacts(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	var groups []store.Group
	if db != nil {
		groups, _ = db.ListGroups()
	}
	h.Views.RenderApp(w, "contacts.html", map[string]any{"Title": "Contacts", "Nav": "contacts", "Groups": groups})
}

// Groups redirects to Contacts — groups are managed from the drawer there.
func (h *Web) Groups(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/contacts", http.StatusSeeOther)
}

// Campaigns lists broadcasts + new-campaign form (group + template options included).
func (h *Web) Campaigns(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	var groups []store.Group
	var templates []store.Template
	var accounts []store.Account
	if db != nil {
		groups, _ = db.ListGroups()
		templates, _ = db.ListTemplates()
		if db.IsPostgres() {
			if as, err := db.AccountsByOrg(id.OrgID); err == nil {
				accounts = visibleAccounts(id, as)
			}
		}
	}
	h.Views.RenderApp(w, "campaigns.html", map[string]any{"Title": "Campaigns", "Nav": "campaigns", "Groups": groups, "Templates": templates, "Accounts": accounts})
}

// CampaignDetail shows one campaign funnel + recipients (live poll).
func (h *Web) CampaignDetail(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	id := r.PathValue("id")
	c, err := db.GetCampaign(id)
	if err != nil {
		http.Error(w, "campaign not found", http.StatusNotFound)
		return
	}
	h.Views.RenderApp(w, "campaign-detail.html", map[string]any{"Title": c.Name, "Nav": "campaigns", "ID": id, "Campaign": c})
}

// Settings shows channel, app and data shortcuts.
func (h *Web) Settings(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	var contacts, groups, campaigns, _ int
	var templates int
	if db != nil {
		contacts, groups, campaigns, _ = db.Stats()
		if ts, err := db.ListTemplates(); err == nil {
			templates = len(ts)
		}
	}
	conn := h.connBadge(id, db)
	h.Views.RenderApp(w, "settings.html", map[string]any{
		"Title": "Settings", "Nav": "settings",
		"Contacts": contacts, "Groups": groups,
		"Campaigns": campaigns, "Templates": templates,
		"Conn": conn,
	})
}

// AdminSettings renders workspace administration (plan, team, keys,
// webhooks) inside the admin layout. Admin-only (members:manage).
func (h *Web) AdminSettings(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, store.PermMembersManage)
	if !ok {
		return
	}
	h.Views.RenderAdmin(w, "admin/settings.html", map[string]any{
		"Title": "Settings", "Nav": "admin-settings",
		"Billing":       h.billingSummary(db, id.OrgID),
		"CanManageKeys": store.Can(id.Role, store.PermKeysManage),
		"Keys":          h.listKeys(db, id.OrgID),
		"Webhooks":      h.listWebhooks(db, id.OrgID),
		"CanManageTeam": store.Can(id.Role, store.PermMembersManage),
		"Team":          h.teamView(db, id.OrgID, id.UserID),
	})
}

// AdminKeys renders API key management (page is perm-gated, so the
// template assumes key management is allowed).
func (h *Web) AdminKeys(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, store.PermKeysManage)
	if !ok {
		return
	}
	h.Views.RenderAdmin(w, "admin/keys.html", map[string]any{
		"Title": "API keys", "Nav": "admin-keys",
		"Keys": h.listKeys(db, id.OrgID),
	})
}

// AdminWebhooks renders webhook endpoint management (same perm as keys).
func (h *Web) AdminWebhooks(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, store.PermKeysManage)
	if !ok {
		return
	}
	h.Views.RenderAdmin(w, "admin/webhooks.html", map[string]any{
		"Title": "Webhooks", "Nav": "admin-webhooks",
		"Webhooks": h.listWebhooks(db, id.OrgID),
	})
}

// AdminBilling renders plan & usage for the org.
func (h *Web) AdminBilling(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, store.PermBillingManage)
	if !ok {
		return
	}
	orgName := "Local workspace"
	if db != nil && db.IsPostgres() {
		if org, err := db.GetOrg(id.OrgID); err == nil {
			orgName = org.Name
		}
	}
	h.Views.RenderAdmin(w, "admin/billing.html", map[string]any{
		"Title": "Plan & usage", "Nav": "admin-billing",
		"OrgName": orgName,
		"Billing": h.billingSummary(db, id.OrgID),
	})
}

// billingSummary builds the plan & usage card model (nil when unavailable).
func (h *Web) billingSummary(db *store.DB, orgID string) map[string]any {
	if db == nil || !db.IsPostgres() {
		return nil
	}
	plan, err := db.EffectivePlan(orgID)
	if err != nil {
		return nil
	}
	usage, err := db.UsageForOrg(orgID)
	if err != nil {
		return nil
	}
	lim := func(n int) any {
		if n < 0 {
			return "∞"
		}
		return n
	}
	return map[string]any{
		"PlanName": plan.Name,
		"PlanCode": plan.Code,
		"Rows": []map[string]any{
			{"Label": "Messages this month", "Used": usage.MsgsThisMonth, "Limit": lim(plan.Limits.MsgsPerMonth)},
			{"Label": "Contacts", "Used": usage.Contacts, "Limit": lim(plan.Limits.Contacts)},
			{"Label": "WhatsApp numbers", "Used": usage.Accounts, "Limit": lim(plan.Limits.Accounts)},
			{"Label": "Members", "Used": usage.Members, "Limit": lim(plan.Limits.Members)},
		},
	}
}

// Admin renders the admin overview inside the admin layout. Admin-only
// (members:manage); legacy SQLite requests without identity pass through
// with zeroed sections.
func (h *Web) Admin(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, store.PermMembersManage)
	if !ok {
		return
	}
	orgName := "Local workspace"
	members, invites, accounts, connected := 0, 0, 0, 0
	var audit []store.AuditEntry
	if db != nil && db.IsPostgres() {
		if org, err := db.GetOrg(id.OrgID); err == nil {
			orgName = org.Name
		}
		if ms, err := db.MembersByOrg(id.OrgID); err == nil {
			members = len(ms)
		}
		if invs, err := db.InvitesByOrg(id.OrgID); err == nil {
			invites = len(invs)
		}
		if as, err := db.AccountsByOrg(id.OrgID); err == nil {
			accounts = len(as)
			if h.WA != nil {
				for _, a := range as {
					if st := h.WA.Status(a.ID); st.Connected && st.LoggedIn {
						connected++
					}
				}
			}
		}
		if entries, _, err := db.ListAudit(id.OrgID, 8, ""); err == nil {
			audit = entries
		}
	}
	contacts, groups, campaigns, queued := 0, 0, 0, 0
	if db != nil {
		contacts, groups, campaigns, queued = db.Stats()
	}
	h.Views.RenderAdmin(w, "admin/overview.html", map[string]any{
		"Title": "Admin", "Nav": "admin",
		"OrgName": orgName,
		"Members": members, "Invites": invites,
		"Accounts": accounts, "Connected": connected,
		"Contacts": contacts, "Groups": groups,
		"Campaigns": campaigns, "Queued": queued,
		"Billing": h.billingSummary(db, id.OrgID),
		"Audit":   audit,
	})
}

// AdminUsers renders full user management (members, invites, grants).
func (h *Web) AdminUsers(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, store.PermMembersManage)
	if !ok {
		return
	}
	orgName := "Local workspace"
	if db != nil && db.IsPostgres() {
		if org, err := db.GetOrg(id.OrgID); err == nil {
			orgName = org.Name
		}
	}
	h.Views.RenderAdmin(w, "admin/users.html", map[string]any{
		"Title": "Users", "Nav": "admin-users",
		"OrgName":       orgName,
		"CanManageTeam": true,
		"Team":          h.teamView(db, id.OrgID, id.UserID),
	})
}

// matrixRow is one permission-matrix row on the roles page.
type matrixRow struct {
	Perm        string
	Description string
	Admin       bool
	User        bool
}

// AdminRoles renders the roles × permissions matrix (read-only; custom
// roles are a future extension of the roles table).
func (h *Web) AdminRoles(w http.ResponseWriter, r *http.Request) {
	_, _, ok := Authorize(h.Store, w, r, store.PermMembersManage)
	if !ok {
		return
	}
	matrix := make([]matrixRow, 0)
	for _, p := range store.AllPermissions() {
		matrix = append(matrix, matrixRow{
			Perm:        p,
			Description: store.PermDescriptions[p],
			Admin:       store.Can(store.RoleAdmin, p),
			User:        store.Can(store.RoleUser, p),
		})
	}
	h.Views.RenderAdmin(w, "admin/roles.html", map[string]any{
		"Title": "Roles", "Nav": "admin-roles",
		"Matrix": matrix,
	})
}

// AdminGrants renders per-number access management (members × accounts).
func (h *Web) AdminGrants(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, store.PermMembersManage)
	if !ok {
		return
	}
	orgName := "Local workspace"
	if db != nil && db.IsPostgres() {
		if org, err := db.GetOrg(id.OrgID); err == nil {
			orgName = org.Name
		}
	}
	team := h.teamView(db, id.OrgID, id.UserID)
	h.Views.RenderAdmin(w, "admin/grants.html", map[string]any{
		"Title": "Grants", "Nav": "admin-grants",
		"OrgName":       orgName,
		"CanManageTeam": true,
		"Team":          team,
		"HasGrants":     hasGrants(team),
	})
}

// hasGrants reports whether any member in a teamView has a grant.
func hasGrants(team map[string]any) bool {
	members, _ := team["Members"].([]teamMember)
	for _, m := range members {
		if len(m.Grants) > 0 {
			return true
		}
	}
	return false
}

// teamMember is one row in the settings Team card.
type teamMember struct {
	store.Membership
	Grants []store.Grant
}

// teamView loads members (with grants), pending invites and accounts for
// the Team card. Errors collapse to an empty view (pages never 500).
func (h *Web) teamView(db *store.DB, orgID, currentUserID string) map[string]any {
	view := map[string]any{"Members": []teamMember{}, "Invites": []store.Invite{}, "Accounts": []store.Account{}, "CurrentUserID": currentUserID, "Show": false, "AccountLabels": map[string]string{}}
	if db == nil || !db.IsPostgres() {
		return view
	}
	view["Show"] = true
	if db == nil || !db.IsPostgres() {
		return view
	}
	members, err := db.MembersByOrg(orgID)
	if err != nil {
		return view
	}
	rows := make([]teamMember, 0, len(members))
	for _, m := range members {
		grants, _ := db.GrantsForUser(m.UserID, orgID)
		rows = append(rows, teamMember{Membership: m, Grants: grants})
	}
	view["Members"] = rows
	if invites, err := db.InvitesByOrg(orgID); err == nil {
		view["Invites"] = invites
	}
	if accounts, err := db.AccountsByOrg(orgID); err == nil {
		view["Accounts"] = accounts
		labels := map[string]string{}
		for _, a := range accounts {
			name := a.Label
			if name == "" {
				name = a.Phone
			}
			if name == "" {
				name = a.ID
			}
			labels[a.ID] = name
		}
		view["AccountLabels"] = labels
	}
	return view
}

// listKeys returns org keys or nil (scoped; errors collapse to nil for pages).
func (h *Web) listKeys(db *store.DB, orgID string) []store.APIKey {
	if db == nil || !db.IsPostgres() {
		return nil
	}
	keys, err := db.ListAPIKeys(orgID)
	if err != nil {
		return nil
	}
	return keys
}

// listWebhooks returns org endpoints or nil (scoped; errors collapse to nil).
func (h *Web) listWebhooks(db *store.DB, orgID string) []store.WebhookEndpoint {
	if db == nil || !db.IsPostgres() {
		return nil
	}
	endpoints, err := db.ListEndpoints(orgID)
	if err != nil {
		return nil
	}
	return endpoints
}

// Analytics shows delivery funnels + timeline (live aggregates).
func (h *Web) Analytics(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	o, err := db.Overview()
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	delivery, read := "—", "—"
	if o.DeliveryRate != nil {
		delivery = percent(*o.DeliveryRate)
	}
	if o.ReadRate != nil {
		read = percent(*o.ReadRate)
	}
	sentTotal := o.Funnel.Sent + o.Funnel.Delivered + o.Funnel.Read + o.Funnel.Replied
	h.Views.RenderApp(w, "analytics.html", map[string]any{
		"Title": "Analytics", "Nav": "analytics",
		"Overview": o, "DeliveryPct": delivery, "ReadPct": read, "SentTotal": sentTotal,
	})
}

func percent(f float64) string {
	return strconv.Itoa(int(f*100+0.5)) + "%"
}
