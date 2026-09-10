package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/devstroop/wam/internal/middleware"
	"github.com/devstroop/wam/internal/store"
	"github.com/devstroop/wam/internal/views"
)

// Campaigns serves broadcast CRUD + lifecycle over SQLite.
// JSON for API; HX-Refresh + toast for htmx posts.
type Campaigns struct {
	Store *store.DB
	Views *views.Views
}

// List returns a page of campaigns with counters.
func (h *Campaigns) List(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	data, next, err := db.ListCampaigns(limit, r.URL.Query().Get("cursor"))
	if err != nil {
		WriteProblem(w, r, http.StatusInternalServerError, "Store error", err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": data, "nextCursor": next})
}

// Create snapshots the audience and returns the campaign (JSON or form).
// bodyTemplate may come directly or via template_id (template body is copied).
func (h *Campaigns) Create(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, store.PermCampaignsManage)
	if !ok {
		return
	}
	var body struct {
		Name         string   `json:"name"`
		BodyTemplate string   `json:"bodyTemplate"`
		TemplateID   string   `json:"template_id"`
		GroupIDs     []string `json:"group_ids"`
		ContactIDs   []string `json:"contact_ids"`
		ScheduledAt  string   `json:"scheduledAt"`
	}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid JSON body")
			return
		}
	} else {
		_ = r.ParseForm()
		body.Name = r.FormValue("name")
		body.BodyTemplate = r.FormValue("bodyTemplate")
		body.TemplateID = r.FormValue("template_id")
		if body.TemplateID == "" {
			body.TemplateID = r.FormValue("templateId")
		}
		body.ScheduledAt = r.FormValue("scheduledAt")
		body.GroupIDs = r.Form["group_ids"]
		body.ContactIDs = r.Form["contact_ids"]
	}
	if strings.TrimSpace(body.BodyTemplate) == "" && strings.TrimSpace(body.TemplateID) != "" {
		t, err := db.GetTemplate(strings.TrimSpace(body.TemplateID))
		if err != nil {
			WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "unknown template_id")
			return
		}
		body.BodyTemplate = t.Body
	}
	c, err := db.CreateCampaign(body.Name, body.BodyTemplate, body.GroupIDs, body.ContactIDs, body.ScheduledAt)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if middleware.IsHTMX(r) {
		w.Header().Set("HX-Trigger", `{"toast":"Campaign created"}`)
		w.Header().Set("HX-Refresh", "true")
	}
	WriteJSON(w, http.StatusCreated, c)
}

// Get returns one campaign.
func (h *Campaigns) Get(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	c, err := db.GetCampaign(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, c)
}

// Update edits a draft/scheduled campaign.
// Accepts template_id as an alias for replacing the message from a template.
func (h *Campaigns) Update(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, store.PermCampaignsManage)
	if !ok {
		return
	}
	var body struct {
		Name         string `json:"name"`
		BodyTemplate string `json:"bodyTemplate"`
		TemplateID   string `json:"template_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid JSON body")
		return
	}
	if strings.TrimSpace(body.BodyTemplate) == "" && strings.TrimSpace(body.TemplateID) != "" {
		t, err := db.GetTemplate(strings.TrimSpace(body.TemplateID))
		if err != nil {
			WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "unknown template_id")
			return
		}
		body.BodyTemplate = t.Body
	}
	c, err := db.UpdateCampaign(r.PathValue("id"), body.Name, body.BodyTemplate)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, c)
}

// Delete removes a campaign with its recipients.
func (h *Campaigns) Delete(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, store.PermCampaignsManage)
	if !ok {
		return
	}
	if err := db.DeleteCampaign(r.PathValue("id")); err != nil {
		writeStoreError(w, r, err)
		return
	}
	if middleware.IsHTMX(r) {
		w.Header().Set("HX-Trigger", `{"toast":"Campaign deleted"}`)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// transition runs a lifecycle change with htmx feedback.
func (h *Campaigns) transition(w http.ResponseWriter, r *http.Request, perm, to, toast string) {
	_, db, ok := Authorize(h.Store, w, r, perm)
	if !ok {
		return
	}
	c, err := db.SetCampaignStatus(r.PathValue("id"), to)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if middleware.IsHTMX(r) {
		w.Header().Set("HX-Trigger", `{"toast":"`+toast+`"}`)
		w.Header().Set("HX-Refresh", "true")
	}
	WriteJSON(w, http.StatusAccepted, c)
}

// Start begins sending (draft/scheduled → sending).
func (h *Campaigns) Start(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, store.PermCampaignsSend, store.CampaignSending, "Campaign started")
}

// Pause halts mid-flight (sending → paused).
func (h *Campaigns) Pause(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, store.PermCampaignsManage, store.CampaignPaused, "Campaign paused")
}

// Resume continues (paused → sending).
func (h *Campaigns) Resume(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, store.PermCampaignsSend, store.CampaignSending, "Campaign resumed")
}

// Cancel stops permanently.
func (h *Campaigns) Cancel(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, store.PermCampaignsManage, store.CampaignCancelled, "Campaign cancelled")
}

// Recipients lists per-contact delivery states.
func (h *Campaigns) Recipients(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	if _, err := db.GetCampaign(r.PathValue("id")); err != nil {
		writeStoreError(w, r, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	data, next, err := db.ListRecipients(r.PathValue("id"), limit, r.URL.Query().Get("cursor"))
	if err != nil {
		WriteProblem(w, r, http.StatusInternalServerError, "Store error", err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": data, "nextCursor": next})
}

// Funnel returns status counters for a campaign.
func (h *Campaigns) Funnel(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	if _, err := db.GetCampaign(r.PathValue("id")); err != nil {
		writeStoreError(w, r, err)
		return
	}
	f, err := db.RecipientFunnel(r.PathValue("id"))
	if err != nil {
		WriteProblem(w, r, http.StatusInternalServerError, "Store error", err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, f)
}

// Rows renders the campaign table fragment.
func (h *Campaigns) Rows(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	data, _, err := db.ListCampaigns(20, "")
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	h.Views.RenderPartial(w, "campaign-rows", map[string]any{"Campaigns": data})
}

// AudienceContacts renders a searchable list of contacts with checkboxes for the campaign audience picker.
func (h *Campaigns) AudienceContacts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	cursor := r.URL.Query().Get("cursor")
	_, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	data, next, err := db.ListContacts(q, "", 20, cursor)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	h.Views.RenderPartial(w, "audience-contacts", map[string]any{"Contacts": data, "NextCursor": next, "Q": q})
}

// RecipientRows renders recipient rows for the detail page (?campaign=id).
func (h *Campaigns) RecipientRows(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("campaign")
	if id == "" {
		http.Error(w, "campaign query param required", http.StatusBadRequest)
		return
	}
	_, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	data, _, err := db.ListRecipients(id, 50, "")
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	f, _ := db.RecipientFunnel(id)
	h.Views.RenderPartial(w, "recipient-rows", map[string]any{"Recipients": data, "Funnel": f})
}
