// Package mail sends transactional email (verify, reset, invites).
// v1 ships LogMailer (dev: links in structured logs); SMTP lands with
// production hardening. Handlers depend on the Mailer interface only.
package mail

import (
	"fmt"
	"log/slog"
)

// Mailer sends one message.
type Mailer interface {
	Send(to, subject, body string) error
}

// LogMailer logs instead of sending (local dev).
type LogMailer struct{ Log *slog.Logger }

func (m LogMailer) Send(to, subject, body string) error {
	log := m.Log
	if log == nil {
		log = slog.Default()
	}
	log.Info("mail (logged, not sent)", "to", to, "subject", subject, "body", body)
	return nil
}

// VerifyBody builds the verification message.
func VerifyBody(baseURL, token string) string {
	return fmt.Sprintf("Verify your WAM account:\n\n%s/verify?token=%s\n\nLink expires in 24 hours.", baseURL, token)
}

// ResetBody builds the password-reset message.
func ResetBody(baseURL, token string) string {
	return fmt.Sprintf("Reset your WAM password:\n\n%s/reset?token=%s\n\nLink expires in 1 hour. Ignore if you didn't ask.", baseURL, token)
}

// InviteBody builds the invitation message.
func InviteBody(baseURL, orgName, token string) string {
	return fmt.Sprintf("You've been invited to %s on WAM:\n\n%s/invite/accept?token=%s\n\nLink expires in 7 days.", orgName, baseURL, token)
}
