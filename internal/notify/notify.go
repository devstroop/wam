// Package notify fans org events out to subscribed webhook endpoints.
//
// Design: Emit is fire-and-forget from hot paths (worker sends, receipts).
// Each delivery POSTs HMAC-signed JSON with retries inline in a goroutine
// and records the final outcome; a crash may lose in-flight attempts
// (documented v1 tradeoff — no persistent retry sweeper yet).
// NOTE for the sender-scale rebase: move Emit call sites from the legacy
// loop (sendLegacy/onReceipt/done) into the queue drain equivalents.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/devstroop/wam/internal/store"
)

// Dispatcher delivers events (nil-safe: construct once in server.New).
type Dispatcher struct {
	Store *store.DB // pool handle (app role); Emit scopes per org internally
	HTTP  *http.Client
	Log   *slog.Logger
}

func (d *Dispatcher) http() *http.Client {
	if d != nil && d.HTTP != nil {
		return d.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (d *Dispatcher) log() *slog.Logger {
	if d != nil && d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

// Payload is the signed envelope.
type Payload struct {
	Event string         `json:"event"`
	OrgID string         `json:"orgId"`
	At    string         `json:"at"`
	Data  map[string]any `json:"data"`
}

// Emit queues delivery of event to the org's subscribed endpoints.
// Never blocks the caller; never fails it.
func (d *Dispatcher) Emit(orgID, event string, data map[string]any) {
	if d == nil || d.Store == nil {
		return
	}
	body, err := json.Marshal(Payload{
		Event: event, OrgID: orgID,
		At:   time.Now().UTC().Format(time.RFC3339),
		Data: data,
	})
	if err != nil {
		return
	}
	var targets []store.EndpointWithSecret
	_ = d.Store.WithOrg(context.Background(), orgID, func(odb *store.DB) error {
		endpoints, err := odb.EndpointsForEvent(orgID, event)
		if err != nil {
			return err
		}
		targets = append(targets, endpoints...)
		return nil
	})
	for _, t := range targets {
		t := t
		go d.deliver(orgID, t, event, body)
	}
}

func (d *Dispatcher) deliver(orgID string, t store.EndpointWithSecret, event string, body []byte) {
	const tries = 3
	backoffs := []time.Duration{2 * time.Second, 8 * time.Second}
	var lastErr string
	for attempt := 1; attempt <= tries; attempt++ {
		if attempt > 1 {
			time.Sleep(backoffs[attempt-2])
		}
		if err := d.post(t, event, body); err == nil {
			_ = d.Store.RecordDelivery(orgID, t.ID, event, string(body), "delivered", attempt, "")
			return
		} else {
			lastErr = err.Error()
		}
	}
	d.log().Warn("notify: delivery failed", "org", orgID, "endpoint", t.ID, "event", event, "err", lastErr)
	_ = d.Store.RecordDelivery(orgID, t.ID, event, string(body), "failed", tries, lastErr)
}

func (d *Dispatcher) post(t store.EndpointWithSecret, event string, body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", t.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Wam-Event", event)
	if t.Secret != "" {
		mac := hmac.New(sha256.New, []byte(t.Secret))
		mac.Write(body)
		req.Header.Set("X-Wam-Signature", hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := d.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
