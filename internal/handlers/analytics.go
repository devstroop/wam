package handlers

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"time"

	"github.com/devstroop/wam/internal/store"
	"github.com/devstroop/wam/internal/views"
)

// Analytics serves the account overview, per-campaign table rows and CSV export.
type Analytics struct {
	Store *store.DB
	Views *views.Views
}

// Overview returns account-wide aggregates + 14-day timeline.
func (h *Analytics) Overview(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	o, err := db.Overview()
	if err != nil {
		WriteProblem(w, r, http.StatusInternalServerError, "Store error", err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, o)
}

// Rows renders one table row per campaign with its funnel.
func (h *Analytics) Rows(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	data, _, err := db.ListCampaigns(50, "")
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	h.Views.RenderPartial(w, "analytics-rows", map[string]any{"Campaigns": data})
}

// Chart renders the 14-day timeline fragment.
func (h *Analytics) Chart(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	o, err := db.Overview()
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	h.Views.RenderPartial(w, "analytics-chart", map[string]any{"Timeline": o.Timeline})
}

// Export downloads all recipients of a campaign as CSV.
func (h *Analytics) Export(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	c, err := db.GetCampaign(id)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	rows, err := db.ExportRows(id)
	if err != nil {
		WriteProblem(w, r, http.StatusInternalServerError, "Store error", err.Error())
		return
	}
	name := fmt.Sprintf("wam-%s-%s.csv", sanitize(c.Name), time.Now().UTC().Format("20060102"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"phone", "name", "status", "wa_msg_id", "error", "sent_at"})
	for _, rec := range rows {
		_ = cw.Write([]string{rec.Phone, rec.Name, rec.Status, rec.WAMsgID, rec.Error, rec.SentAt})
	}
	cw.Flush()
}

func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			out = append(out, r)
		} else if r == ' ' {
			out = append(out, '-')
		}
	}
	if len(out) == 0 {
		return "campaign"
	}
	return string(out)
}
