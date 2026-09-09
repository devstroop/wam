package handlers

import (
	"net/http"
	"strconv"

	"github.com/devstroop/wam/internal/store"
	"github.com/devstroop/wam/internal/views"
	"github.com/devstroop/wam/internal/wa"
)

// Web holds HTML/htmx handlers.
type Web struct {
	Views *views.Views
	Store *store.DB
	WA    *wa.Service
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
	var contacts, groups, campaigns, queued int
	if h.Store != nil {
		contacts, groups, campaigns, queued = h.Store.Stats()
	}
	var funnel store.Funnel
	if h.Store != nil {
		if o, err := h.Store.Overview(); err == nil {
			funnel = o.Funnel
		}
	}
	conn := map[string]any{"Connected": false, "LoggedIn": false}
	if h.WA != nil {
		st := h.WA.Status()
		conn = map[string]any{"Connected": st.Connected, "LoggedIn": st.LoggedIn, "Phone": st.Phone}
	}
	h.Views.RenderApp(w, "dashboard.html", map[string]any{
		"Title": "Dashboard", "Nav": "dashboard",
		"Contacts": contacts, "Groups": groups,
		"Campaigns": campaigns, "Queued": queued,
		"Funnel": funnel, "Conn": conn,
	})
}

// Connect shows WhatsApp pairing status (QR/phone).
func (h *Web) Connect(w http.ResponseWriter, r *http.Request) {
	isConnected := false
	if h.WA != nil {
		st := h.WA.Status()
		isConnected = st.Connected && st.LoggedIn
	}
	h.Views.RenderApp(w, "connect.html", map[string]any{"Title": "Connect", "Nav": "connect", "IsConnected": isConnected})
}

// Contacts lists audiences (live table + group filter options).
func (h *Web) Contacts(w http.ResponseWriter, r *http.Request) {
	var groups []store.Group
	if h.Store != nil {
		groups, _ = h.Store.ListGroups()
	}
	h.Views.RenderApp(w, "contacts.html", map[string]any{"Title": "Contacts", "Nav": "contacts", "Groups": groups})
}

// Groups redirects to Contacts — groups are managed from the drawer there.
func (h *Web) Groups(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/contacts", http.StatusSeeOther)
}

// Campaigns lists broadcasts + new-campaign form (group + template options included).
func (h *Web) Campaigns(w http.ResponseWriter, r *http.Request) {
	var groups []store.Group
	var templates []store.Template
	if h.Store != nil {
		groups, _ = h.Store.ListGroups()
		templates, _ = h.Store.ListTemplates()
	}
	h.Views.RenderApp(w, "campaigns.html", map[string]any{"Title": "Campaigns", "Nav": "campaigns", "Groups": groups, "Templates": templates})
}

// CampaignDetail shows one campaign funnel + recipients (live poll).
func (h *Web) CampaignDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, err := h.Store.GetCampaign(id)
	if err != nil {
		http.Error(w, "campaign not found", http.StatusNotFound)
		return
	}
	h.Views.RenderApp(w, "campaign-detail.html", map[string]any{"Title": c.Name, "Nav": "campaigns", "ID": id, "Campaign": c})
}

// Settings shows channel, app and data shortcuts.
func (h *Web) Settings(w http.ResponseWriter, r *http.Request) {
	var contacts, groups, campaigns, _ int
	var templates int
	if h.Store != nil {
		contacts, groups, campaigns, _ = h.Store.Stats()
		if ts, err := h.Store.ListTemplates(); err == nil {
			templates = len(ts)
		}
	}
	conn := map[string]any{"Connected": false, "LoggedIn": false}
	if h.WA != nil {
		st := h.WA.Status()
		conn = map[string]any{"Connected": st.Connected, "LoggedIn": st.LoggedIn, "Phone": st.Phone}
	}
	h.Views.RenderApp(w, "settings.html", map[string]any{
		"Title": "Settings", "Nav": "settings",
		"Contacts": contacts, "Groups": groups,
		"Campaigns": campaigns, "Templates": templates,
		"Conn": conn,
	})
}

// Analytics shows delivery funnels + timeline (live aggregates).
func (h *Web) Analytics(w http.ResponseWriter, r *http.Request) {
	o, err := h.Store.Overview()
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
