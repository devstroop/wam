package handlers

import (
	"net/http"
)

// Healthz is the liveness probe. Keep it dependency-free.
func Healthz(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "wam"})
}

// Readyz is the readiness probe. Wire DB/Redis/WhatsApp checks here later.
func Readyz(w http.ResponseWriter, _ *http.Request) {
	// TODO: check dependencies (db, cache, gateway) before reporting ready.
	WriteJSON(w, http.StatusOK, map[string]any{"status": "ready", "service": "wam"})
}
