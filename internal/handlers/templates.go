package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/devstroop/wam/internal/middleware"
	"github.com/devstroop/wam/internal/store"
	"github.com/devstroop/wam/internal/views"
)

// Templates serves message template CRUD over SQLite.
type Templates struct {
	Store *store.DB
	Views *views.Views
}

// List returns all templates.
func (h *Templates) List(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	data, err := db.ListTemplates()
	if err != nil {
		WriteProblem(w, r, http.StatusInternalServerError, "Store error", err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": data})
}

// Create adds a template (JSON or form).
func (h *Templates) Create(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, store.PermTemplatesManage)
	if !ok {
		return
	}
	var body struct {
		Name     string `json:"name"`
		Body     string `json:"body"`
		Category string `json:"category"`
		Language string `json:"language"`
	}
	ct := r.Header.Get("Content-Type")
	if len(ct) > 0 && (ct == "application/json" || len(ct) > 16 && ct[:16] == "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid JSON")
			return
		}
	} else {
		_ = r.ParseForm()
		body.Name = r.FormValue("name")
		body.Body = r.FormValue("body")
		body.Category = r.FormValue("category")
		body.Language = r.FormValue("language")
		// hx-post from form may send as urlencoded; if empty try json as fallback (tests use json without header)
		if body.Name == "" && body.Body == "" {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
	}
	t, err := db.CreateTemplate(body.Name, body.Body, body.Category, body.Language)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if middleware.IsHTMX(r) {
		w.Header().Set("HX-Trigger", `{"toast":"Template created"}`)
		w.Header().Set("HX-Refresh", "true")
	}
	WriteJSON(w, http.StatusCreated, t)
}

// Get returns one template.
func (h *Templates) Get(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	t, err := db.GetTemplate(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, t)
}

// Update patches a template.
func (h *Templates) Update(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, store.PermTemplatesManage)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
		Body string `json:"body"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	t, err := db.UpdateTemplate(r.PathValue("id"), body.Name, body.Body)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, t)
}

// Delete removes a template.
func (h *Templates) Delete(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, store.PermTemplatesManage)
	if !ok {
		return
	}
	if err := db.DeleteTemplate(r.PathValue("id")); err != nil {
		writeStoreError(w, r, err)
		return
	}
	if middleware.IsHTMX(r) {
		w.Header().Set("HX-Trigger", `{"toast":"Template deleted"}`)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Duplicate clones a template.
func (h *Templates) Duplicate(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, store.PermTemplatesManage)
	if !ok {
		return
	}
	t, err := db.DuplicateTemplate(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, t)
}

// Rows renders the template table fragment.
func (h *Templates) Rows(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	data, err := db.ListTemplates()
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	h.Views.RenderPartial(w, "template-rows", map[string]any{"Templates": data})
}

// Page renders the templates library.
func (h *Templates) Page(w http.ResponseWriter, r *http.Request) {
	h.Views.RenderApp(w, "templates.html", map[string]any{"Title": "Templates", "Nav": "templates"})
}
