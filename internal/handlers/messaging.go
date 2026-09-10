package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/devstroop/wam/internal/store"
	"github.com/devstroop/wam/internal/wa"
	"github.com/google/uuid"
)

// Messaging serves number checks + one-off sends against the live client.
// 503 when WhatsApp isn't connected.
type Messaging struct {
	WA *wa.Service
}

// Check reports WhatsApp registration per phone number.
func (h *Messaging) Check(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := Authorize(nil, w, r, store.PermCampaignsSend); !ok {
		return
	}
	var body struct {
		Phones []string `json:"phones"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Phones) == 0 {
		WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "phones[] is required")
		return
	}
	if len(body.Phones) > 50 {
		WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "max 50 phones per check")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	type result struct {
		Phone      string `json:"phone"`
		OnWhatsApp bool   `json:"onWhatsApp"`
		JID        string `json:"jid,omitempty"`
	}
	out := make([]result, 0, len(body.Phones))
	for _, p := range body.Phones {
		jid, err := h.WA.ResolvePhone(ctx, p)
		if err != nil {
			out = append(out, result{Phone: p})
			continue
		}
		out = append(out, result{Phone: p, OnWhatsApp: true, JID: jid})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"results": out})
}

// Send delivers a one-off text (resolves E.164 → JID first).
func (h *Messaging) Send(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := Authorize(nil, w, r, store.PermCampaignsSend); !ok {
		return
	}
	var body struct {
		To   string `json:"to"`
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.To == "" || body.Body == "" {
		WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "to and body are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	jid, err := h.WA.ResolvePhone(ctx, body.To)
	if err != nil {
		WriteProblem(w, r, http.StatusBadGateway, "Recipient unreachable", err.Error())
		return
	}
	id, err := h.WA.SendText(ctx, jid, body.Body)
	if err != nil {
		WriteProblem(w, r, http.StatusBadGateway, "Send failed", err.Error())
		return
	}
	_ = id
	WriteJSON(w, http.StatusAccepted, map[string]any{"messageId": uuid.NewString(), "status": "queued"})
}
