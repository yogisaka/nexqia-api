package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	netmail "net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

const (
	smtpDialTimeout    = 15 * time.Second
	smtpSessionTimeout = 60 * time.Second
)

// smtpSender talks to any STARTTLS relay with PLAIN auth (Gmail App Password
// first; SES/Brevo/Mailgun only need different SMTP_* values).
type smtpSender struct {
	host     string
	port     int
	username string
	password string
	from     string
	fromName string
}

func (s smtpSender) Send(ctx context.Context, m Message) error {
	body, err := buildMIME(s.from, s.fromName, m)
	if err != nil {
		return err
	}

	dialCtx, cancel := context.WithTimeout(ctx, smtpDialTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", net.JoinHostPort(s.host, strconv.Itoa(s.port)))
	if err != nil {
		return fmt.Errorf("smtp dial: %w", err)
	}
	// One deadline for the whole SMTP session: a stalled server must not hang the worker.
	if err := conn.SetDeadline(time.Now().Add(smtpSessionTimeout)); err != nil {
		conn.Close()
		return fmt.Errorf("smtp deadline: %w", err)
	}
	c, err := smtp.NewClient(conn, s.host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp hello: %w", err)
	}
	defer c.Close()

	if ok, _ := c.Extension("STARTTLS"); !ok {
		return errors.New("smtp: server does not support STARTTLS")
	}
	if err := c.StartTLS(&tls.Config{ServerName: s.host}); err != nil {
		return fmt.Errorf("smtp starttls: %w", err)
	}
	if err := c.Auth(smtp.PlainAuth("", s.username, s.password, s.host)); err != nil {
		return fmt.Errorf("smtp auth: %w", err)
	}
	if err := c.Mail(s.from); err != nil {
		return fmt.Errorf("smtp mail from: %w", err)
	}
	if err := c.Rcpt(m.To); err != nil {
		return fmt.Errorf("smtp rcpt: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp data end: %w", err)
	}
	return c.Quit()
}

// buildMIME renders a multipart/alternative (text + HTML) message with CRLF line
// endings. Header values containing CR/LF are refused: addresses are validated
// at registration/profile already, this is the second layer against header
// injection.
func buildMIME(from, fromName string, m Message) ([]byte, error) {
	for _, v := range []string{from, fromName, m.To, m.Subject} {
		if strings.ContainsAny(v, "\r\n") {
			return nil, errors.New("mail: header value contains a line break")
		}
	}

	var parts bytes.Buffer
	mw := multipart.NewWriter(&parts)
	for _, p := range []struct{ contentType, body string }{
		{"text/plain; charset=utf-8", m.Text},
		{"text/html; charset=utf-8", m.HTML},
	} {
		pw, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {p.contentType},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return nil, err
		}
		qp := quotedprintable.NewWriter(pw)
		if _, err := qp.Write([]byte(p.body)); err != nil {
			return nil, err
		}
		if err := qp.Close(); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	msgID, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	domain := "localhost"
	if at := strings.LastIndex(from, "@"); at >= 0 && at < len(from)-1 {
		domain = from[at+1:]
	}

	var b bytes.Buffer
	header := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	// net/mail quotes the display name and Q-encodes it when it is not ASCII.
	header("From", (&netmail.Address{Name: fromName, Address: from}).String())
	header("To", m.To)
	header("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	header("Date", time.Now().Format(time.RFC1123Z))
	header("Message-ID", "<"+msgID+"@"+domain+">")
	header("MIME-Version", "1.0")
	header("Content-Type", "multipart/alternative; boundary="+mw.Boundary())
	b.WriteString("\r\n")
	b.Write(parts.Bytes())
	return b.Bytes(), nil
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
