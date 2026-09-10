package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/devstroop/wam/internal/middleware"
	"github.com/devstroop/wam/internal/store"
	"github.com/devstroop/wam/internal/views"
)

// Groups serves label/segment CRUD + membership over SQLite.
type Groups struct {
	Store *store.DB
	Views *views.Views
}

// List returns all groups with member counts.
func (h *Groups) List(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	data, err := db.ListGroups()
	if err != nil {
		WriteProblem(w, r, http.StatusInternalServerError, "Store error", err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": data})
}

// Rows renders the group list fragment.
func (h *Groups) Rows(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	data, err := db.ListGroups()
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	h.Views.RenderPartial(w, "group-rows", map[string]any{"Groups": data})
}

// Create adds a group.
func (h *Groups) Create(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, store.PermContactsManage)
	if !ok {
		return
	}
	var body struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	// Accept JSON or htmx form posts.
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid JSON body")
			return
		}
	} else {
		_ = r.ParseForm()
		body.Name = r.FormValue("name")
		body.Color = r.FormValue("color")
	}
	g, err := db.CreateGroup(body.Name, body.Color)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if middleware.IsHTMX(r) {
		// No HX-Refresh: callers refresh in place (drawer list) and
		// reload on close to sync filter dropdowns.
		w.Header().Set("HX-Trigger", `{"toast":"Group created"}`)
	}
	WriteJSON(w, http.StatusCreated, g)
}

// Get returns one group.
func (h *Groups) Get(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	g, err := db.GetGroup(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, g)
}

// Update renames/recolors.
func (h *Groups) Update(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, store.PermContactsManage)
	if !ok {
		return
	}
	var body struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid JSON body")
		return
	}
	g, err := db.UpdateGroup(r.PathValue("id"), body.Name, body.Color)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if middleware.IsHTMX(r) {
		w.Header().Set("HX-Trigger", `{"toast":"Group updated"}`)
		w.Header().Set("HX-Refresh", "true")
	}
	WriteJSON(w, http.StatusOK, g)
}

// Delete removes a group.
func (h *Groups) Delete(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, store.PermContactsManage)
	if !ok {
		return
	}
	if err := db.DeleteGroup(r.PathValue("id")); err != nil {
		writeStoreError(w, r, err)
		return
	}
	if middleware.IsHTMX(r) {
		w.Header().Set("HX-Trigger", `{"toast":"Group deleted"}`)
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SetMembers replaces membership.
func (h *Groups) SetMembers(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ContactIDs []string `json:"contact_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid JSON body")
		return
	}
	_, db, ok := Authorize(h.Store, w, r, store.PermContactsManage)
	if !ok {
		return
	}
	if err := db.SetGroupMembers(r.PathValue("id"), body.ContactIDs); err != nil {
		writeStoreError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
