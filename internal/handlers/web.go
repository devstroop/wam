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
	var billing map[string]any
	if db != nil && db.IsPostgres() {
		if plan, err := db.EffectivePlan(id.OrgID); err == nil {
			if usage, err := db.UsageForOrg(id.OrgID); err == nil {
				lim := func(n int) any {
					if n < 0 {
						return "∞"
					}
					return n
				}
				billing = map[string]any{
					"PlanName": plan.Name,
					"Rows": []map[string]any{
						{"Label": "Messages this month", "Used": usage.MsgsThisMonth, "Limit": lim(plan.Limits.MsgsPerMonth)},
						{"Label": "Contacts", "Used": usage.Contacts, "Limit": lim(plan.Limits.Contacts)},
						{"Label": "WhatsApp numbers", "Used": usage.Accounts, "Limit": lim(plan.Limits.Accounts)},
						{"Label": "Members", "Used": usage.Members, "Limit": lim(plan.Limits.Members)},
					},
				}
			}
		}
	}
	h.Views.RenderApp(w, "settings.html", map[string]any{
		"Title": "Settings", "Nav": "settings",
		"Contacts": contacts, "Groups": groups,
		"Campaigns": campaigns, "Templates": templates,
		"Conn": conn, "Billing": billing,
		"CanManageKeys": store.Can(id.Role, store.PermKeysManage),
		"Keys":          h.listKeys(db, id.OrgID),
		"Webhooks":      h.listWebhooks(db, id.OrgID),
		"CanManageTeam": store.Can(id.Role, store.PermMembersManage),
		"Team":          h.teamView(db, id.OrgID, id.UserID),
	})
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
