// Package auth implements single-admin authentication for WAM.
//
// Design: password from WAM_ADMIN_PASSWORD (env-only). On POST /login the
// password is compared with bcrypt (or plaintext compare when the env holds
// a plaintext secret — hashed with bcrypt at boot is recommended but optional).
// Success issues an HMAC-signed session cookie (stdlib only, no deps).
// Empty AdminPassword disables auth (local dev convenience).
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const cookieName = "wam_session"

// Session issues and validates cookies. Secret should be 32+ bytes.
type Session struct {
	secret        []byte
	passwordHash  []byte
	plainFallback string
	enabled       bool
}

// New builds a Session. Empty adminPassword disables auth (all allowed).
func New(adminPassword, secret string) *Session {
	if strings.TrimSpace(adminPassword) == "" {
		return &Session{enabled: false}
	}
	s := &Session{secret: []byte(secret), enabled: true}
	if hash, err := bcrypt.GenerateFromPassword([]byte(adminPassword), bcrypt.DefaultCost); err == nil {
		// NOTE: bcrypt includes a random salt, so we keep the hash for this
		// process and compare against the original password. Restarting the
		// server re-hashes; clients must log in again (acceptable v1).
		s.passwordHash = hash
		s.plainFallback = adminPassword
	} else {
		s.plainFallback = adminPassword
	}
	return s
}

// Enabled reports whether auth is enforced.
func (s *Session) Enabled() bool { return s.enabled }

// CheckPassword verifies a login attempt.
func (s *Session) CheckPassword(pw string) bool {
	if !s.enabled {
		return true
	}
	if len(s.passwordHash) > 0 {
		if bcrypt.CompareHashAndPassword(s.passwordHash, []byte(pw)) == nil {
			return true
		}
	}
	return subtle.ConstantTimeCompare([]byte(pw), []byte(s.plainFallback)) == 1
}

// Issue sets the session cookie (valid 30 days, HttpOnly, SameSite=Lax)
// and returns the raw cookie value (hash it for server-side row lookup).
func (s *Session) Issue(w http.ResponseWriter, r *http.Request) string {
	exp := time.Now().Add(30 * 24 * time.Hour).Unix()
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	payload := "v1." + strconv.FormatInt(exp, 10) + "." + base64.RawURLEncoding.EncodeToString(nonce)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	raw := payload + "." + sig
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    raw,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		Expires:  time.Unix(exp, 0),
	})
	return raw
}

// Clear removes the session cookie.
func (s *Session) Clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1, HttpOnly: true})
}

// Valid reports whether the request carries a live session.
func (s *Session) Valid(r *http.Request) bool {
	if !s.enabled {
		return true
	}
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 4 || parts[0] != "v1" {
		return false
	}
	payload := strings.Join(parts[:3], ".")
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(parts[3]), []byte(want)) != 1 {
		return false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return true
}
