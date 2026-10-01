// Package mail renders transactional emails, stores them in core.email_outbox and
// delivers them from a background worker (spec 2026-10-01-email-outbox-design).
package mail

import (
	"context"
	"log/slog"
	"strings"

	"github.com/yogisaka/nexqia-api/internal/config"
)

// Message is one rendered email. To is filled in by the caller; Render* leave it empty.
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
}

// Sender delivers one message. Errors must never carry credentials: the worker
// stores them in core.email_outbox.last_error and the server log.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// NewSender picks the driver from cfg.MailDriver (validated in config.Load).
func NewSender(cfg config.Config) Sender {
	if cfg.MailDriver == "smtp" {
		return smtpSender{
			host:     cfg.SMTPHost,
			port:     cfg.SMTPPort,
			username: cfg.SMTPUsername,
			password: cfg.SMTPPassword,
			from:     cfg.SMTPFrom,
			fromName: cfg.SMTPFromName,
		}
	}
	return logSender{}
}

// logSender is the dev/test driver: nothing leaves the machine, the email only
// shows up as one masked log line and is then marked sent.
type logSender struct{}

func (logSender) Send(_ context.Context, m Message) error {
	slog.Info("email ready", "to", maskAddress(m.To), "subject", m.Subject)
	return nil
}

// maskAddress keeps the first character of the local part and the domain
// ("andi@gmail.com" → "a***@gmail.com") so logs never hold a full address.
func maskAddress(addr string) string {
	at := strings.LastIndex(addr, "@")
	if at <= 0 {
		return "***"
	}
	return addr[:1] + "***" + addr[at:]
}
