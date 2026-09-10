package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"

	"golang.org/x/crypto/bcrypt"
)

// HashPassword bcrypts a plaintext password (DB users only — never store raw).
func HashPassword(pw string) (string, error) {
	if len(pw) < 8 {
		return "", fmt.Errorf("password must be at least 8 characters")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// CheckPasswordHash compares plaintext against a stored bcrypt hash.
func CheckPasswordHash(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// NewToken returns a URL-safe random token (emailed) plus its SHA256 hex
// digest (stored). Raw tokens never touch the database.
func NewToken() (raw, digest string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, HashToken(raw), nil
}

// HashToken digests a token/cookie value for storage lookup.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%x", sum)
}

// CookieValue extracts the raw session cookie for row lookup.
func (s *Session) CookieValue(r *http.Request) (string, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	return c.Value, true
}
