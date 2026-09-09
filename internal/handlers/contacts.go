package handlers

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/devstroop/wam/internal/middleware"
	"github.com/devstroop/wam/internal/store"
	"github.com/devstroop/wam/internal/views"
)

// Contacts serves audience CRUD + CSV import over SQLite.
// JSON for API; HX-Refresh + toast for htmx form posts.
type Contacts struct {
	Store *store.DB
	Views *views.Views
}

func contactParams(r *http.Request) (q, groupID string, limit int, cursor string) {
	q = strings.TrimSpace(r.URL.Query().Get("q"))
	groupID = strings.TrimSpace(r.URL.Query().Get("group_id"))
	limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 || limit > 100 {
		limit = 20
	}
	return q, groupID, limit, r.URL.Query().Get("cursor")
}

// List returns a page of contacts.
func (h *Contacts) List(w http.ResponseWriter, r *http.Request) {
	q, groupID, limit, cursor := contactParams(r)
	data, next, err := h.Store.ListContacts(q, groupID, limit, cursor)
	if err != nil {
		WriteProblem(w, r, http.StatusInternalServerError, "Store error", err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": data, "nextCursor": next})
}

// Rows renders table rows (htmx fragment, honors q/group_id filters).
func (h *Contacts) Rows(w http.ResponseWriter, r *http.Request) {
	q, groupID, limit, _ := contactParams(r)
	data, _, err := h.Store.ListContacts(q, groupID, limit, "")
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	h.Views.RenderPartial(w, "contact-rows", map[string]any{"Contacts": data})
}

// Create adds a contact (JSON or htmx form).
func (h *Contacts) Create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone    string   `json:"phone"`
		Name     string   `json:"name"`
		GroupIDs []string `json:"group_ids"`
	}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid JSON body")
			return
		}
	} else {
		_ = r.ParseForm()
		body.Phone = r.FormValue("phone")
		body.Name = r.FormValue("name")
		if gid := r.FormValue("group_id"); gid != "" {
			body.GroupIDs = []string{gid}
		}
	}
	c, err := h.Store.CreateContact(body.Phone, body.Name, body.GroupIDs)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if middleware.IsHTMX(r) {
		w.Header().Set("HX-Trigger", `{"toast":"Contact added"}`)
		w.Header().Set("HX-Refresh", "true")
	}
	WriteJSON(w, http.StatusCreated, c)
}

// Get returns one contact.
func (h *Contacts) Get(w http.ResponseWriter, r *http.Request) {
	c, err := h.Store.GetContact(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, c)
}

// Update patches phone/name and optionally membership.
func (h *Contacts) Update(w http.ResponseWriter, r *http.Request) {
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid JSON body")
		return
	}
	var phone, name string
	if v, ok := raw["phone"]; ok {
		_ = json.Unmarshal(v, &phone)
	}
	if v, ok := raw["name"]; ok {
		_ = json.Unmarshal(v, &name)
	}
	var groupIDs []string
	changeGroups := false
	if v, ok := raw["group_ids"]; ok {
		changeGroups = true
		_ = json.Unmarshal(v, &groupIDs)
	}
	c, err := h.Store.UpdateContact(r.PathValue("id"), phone, name, groupIDs, changeGroups)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if middleware.IsHTMX(r) {
		w.Header().Set("HX-Trigger", `{"toast":"Contact updated"}`)
		w.Header().Set("HX-Refresh", "true")
	}
	WriteJSON(w, http.StatusOK, c)
}

// Delete removes a contact.
func (h *Contacts) Delete(w http.ResponseWriter, r *http.Request) {
	if err := h.Store.DeleteContact(r.PathValue("id")); err != nil {
		writeStoreError(w, r, err)
		return
	}
	if middleware.IsHTMX(r) {
		w.Header().Set("HX-Trigger", `{"toast":"Contact deleted"}`)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Import accepts multipart CSV (phone,name columns, header optional).
// Returns 202 {accepted, skipped}; HTML receipt for htmx.
func (h *Contacts) Import(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 5<<20)
	if err := r.ParseMultipartForm(5<<20 + 1024); err != nil {
		WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "multipart file required (max 5MB)")
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "field 'file' is required")
		return
	}
	defer f.Close()
	items, skipped := parseContactsCSV(f)
	accepted, skipped2 := h.Store.ImportContacts(items)
	skipped += skipped2
	if middleware.IsHTMX(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<div class="partial"><span class="badge badge-success">Imported ` +
			strconv.Itoa(accepted) + `</span> <span class="muted">Skipped ` + strconv.Itoa(skipped) + `</span></div>`))
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{"accepted": accepted, "skipped": skipped})
}

func parseContactsCSV(r io.Reader) ([]struct {
	Phone string
	Name  string
}, int) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	records, err := cr.ReadAll()
	if err != nil {
		return nil, 0
	}
	var result []struct {
		Phone string
		Name  string
	}
	skipped := 0
	start := 0
	if len(records) > 0 && looksLikeHeader(records[0]) {
		start = 1
	}
	for i, rec := range records[start:] {
		if i >= 5000 {
			break
		}
		if len(rec) == 0 {
			skipped++
			continue
		}
		phone := strings.TrimSpace(rec[0])
		name := ""
		if len(rec) > 1 {
			name = strings.TrimSpace(rec[1])
		}
		if phone == "" {
			skipped++
			continue
		}
		result = append(result, struct {
			Phone string
			Name  string
		}{Phone: phone, Name: name})
	}
	return result, skipped
}

func looksLikeHeader(rec []string) bool {
	if len(rec) == 0 {
		return false
	}
	first := strings.ToLower(strings.TrimSpace(rec[0]))
	return first == "phone" || first == "phone_number" || first == "msisdn" || first == "number"
}

func writeStoreError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		WriteProblem(w, r, http.StatusNotFound, "Not Found", "resource not found")
	case strings.Contains(err.Error(), "already exists"):
		WriteProblem(w, r, http.StatusConflict, "Conflict", err.Error())
	case strings.Contains(err.Error(), "invalid") ||
		strings.Contains(err.Error(), "cannot transition") ||
		strings.Contains(err.Error(), "only draft"):
		WriteProblem(w, r, http.StatusBadRequest, "Bad Request", err.Error())
	default:
		WriteProblem(w, r, http.StatusInternalServerError, "Store error", err.Error())
	}
}
