package mail

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	netmail "net/mail"
	"strings"
	"testing"
)

func TestMaskAddress(t *testing.T) {
	for in, want := range map[string]string{
		"andi@gmail.com": "a***@gmail.com",
		"b@x.id":         "b***@x.id",
		"no-at-sign":     "***",
		"@nolocal.id":    "***",
		"":               "***",
	} {
		if got := maskAddress(in); got != want {
			t.Errorf("maskAddress(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildMIME_RejectsLineBreaksInHeaders(t *testing.T) {
	base := Message{To: "andi@gmail.com", Subject: "Halo", Text: "t", HTML: "<p>h</p>"}
	cases := map[string]func() (string, string, Message){
		"to": func() (string, string, Message) {
			m := base
			m.To = "andi@gmail.com\r\nBcc: x@evil.test"
			return "a@b.id", "NEXQIA", m
		},
		"subject": func() (string, string, Message) {
			m := base
			m.Subject = "Halo\nBcc: x@evil.test"
			return "a@b.id", "NEXQIA", m
		},
		"from":      func() (string, string, Message) { return "a@b.id\r\n", "NEXQIA", base },
		"from name": func() (string, string, Message) { return "a@b.id", "NEX\nQIA", base },
	}
	for name, c := range cases {
		from, fromName, m := c()
		if _, err := buildMIME(from, fromName, m); err == nil {
			t.Errorf("%s with a line break must be rejected", name)
		}
	}
}

func TestBuildMIME_TwoPartsAndEncodedSubject(t *testing.T) {
	raw, err := buildMIME("noreply@gmail.com", "NEXQIA Klinik", Message{
		To: "andi@gmail.com", Subject: "Selamat datang — kode", Text: "Halo Andi\nbaris dua", HTML: "<p>Halo Andi</p>",
	})
	if err != nil {
		t.Fatalf("buildMIME: %v", err)
	}
	head := string(raw[:bytes.Index(raw, []byte("\r\n\r\n"))])
	for _, line := range strings.Split(head, "\r\n") {
		if strings.ContainsRune(line, '\n') {
			t.Fatalf("header block must use CRLF only: %q", line)
		}
	}

	msg, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse message: %v", err)
	}
	rawSubject := msg.Header.Get("Subject")
	if !strings.HasPrefix(rawSubject, "=?utf-8?q?") {
		t.Errorf("non-ASCII subject must be Q-encoded, got %q", rawSubject)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(rawSubject)
	if err != nil || subject != "Selamat datang — kode" {
		t.Errorf("decoded subject = %q (%v)", subject, err)
	}
	from, err := msg.Header.AddressList("From")
	if err != nil || len(from) != 1 || from[0].Name != "NEXQIA Klinik" || from[0].Address != "noreply@gmail.com" {
		t.Errorf("unexpected From %v (%v)", from, err)
	}
	if msg.Header.Get("To") != "andi@gmail.com" || msg.Header.Get("MIME-Version") != "1.0" ||
		msg.Header.Get("Date") == "" || !strings.HasSuffix(msg.Header.Get("Message-ID"), "@gmail.com>") {
		t.Errorf("missing/incorrect headers: %v", msg.Header)
	}

	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/alternative" {
		t.Fatalf("content type %q (%v)", mediaType, err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	var types, bodies []string
	for {
		p, err := mr.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("next part: %v", err)
		}
		if p.Header.Get("Content-Transfer-Encoding") != "quoted-printable" {
			t.Errorf("part must be quoted-printable, got %q", p.Header.Get("Content-Transfer-Encoding"))
		}
		body, err := io.ReadAll(quotedprintable.NewReader(p))
		if err != nil {
			t.Fatalf("read part: %v", err)
		}
		types = append(types, p.Header.Get("Content-Type"))
		bodies = append(bodies, string(body))
	}
	if len(types) != 2 || types[0] != "text/plain; charset=utf-8" || types[1] != "text/html; charset=utf-8" {
		t.Fatalf("expected text then html parts, got %v", types)
	}
	if !strings.Contains(bodies[0], "baris dua") || !strings.Contains(bodies[1], "<p>Halo Andi</p>") {
		t.Errorf("unexpected part bodies: %q", bodies)
	}
}

func TestRenderWelcome(t *testing.T) {
	m, err := RenderWelcome(WelcomeData{
		FullName: "Andi <script>alert(1)</script>", CompanyName: "Klinik Sehat", CompanyCode: "KS2026",
		Username: "andi", LoginURL: "https://app.nexqia.id/login",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if m.To != "" {
		t.Errorf("render must leave To empty, got %q", m.To)
	}
	if m.Subject != "Selamat datang di NEXQIA — kode perusahaan Anda" {
		t.Errorf("subject = %q", m.Subject)
	}
	for _, body := range []string{m.Text, m.HTML} {
		if !strings.Contains(body, "KS2026") || !strings.Contains(body, "https://app.nexqia.id/login") || !strings.Contains(body, "Klinik Sehat") {
			t.Errorf("body must carry company code, company name and login URL: %q", body)
		}
	}
	if strings.Contains(m.HTML, "<script>") || !strings.Contains(m.HTML, "&lt;script&gt;") {
		t.Errorf("HTML must escape the name, got %q", m.HTML)
	}
	if strings.Contains(m.Text, "<p") || strings.Contains(m.Text, "<a ") || strings.Contains(m.Text, "<table") {
		t.Errorf("text part must carry no HTML tags: %q", m.Text)
	}
}

func TestRenderNewDeviceLogin(t *testing.T) {
	m, err := RenderNewDeviceLogin(NewDeviceData{
		Username: "andi", When: "01 Oct 2026 09:15 WIB", Device: "Chrome di Windows", IP: "203.0.113.7",
		SecurityURL: "https://app.nexqia.id/account/security",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if m.Subject != "Login baru ke akun NEXQIA Anda" {
		t.Errorf("subject = %q", m.Subject)
	}
	for _, want := range []string{"andi", "01 Oct 2026 09:15 WIB", "Chrome di Windows", "203.0.113.7", "https://app.nexqia.id/account/security"} {
		if !strings.Contains(m.Text, want) || !strings.Contains(m.HTML, want) {
			t.Errorf("both parts must contain %q", want)
		}
	}
	if strings.Contains(m.Text, "<p") || strings.Contains(m.Text, "<a ") {
		t.Errorf("text part must carry no HTML tags: %q", m.Text)
	}
}

// A relay without STARTTLS must be refused before AUTH: the App Password would
// otherwise cross the network in clear text.
func TestSMTPSender_RequiresStartTLS(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		fmt.Fprint(conn, "220 fake ESMTP\r\n")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			switch {
			case strings.HasPrefix(line, "EHLO"):
				fmt.Fprint(conn, "250-fake\r\n250 AUTH PLAIN\r\n")
			case strings.HasPrefix(line, "QUIT"):
				fmt.Fprint(conn, "221 bye\r\n")
				return
			default:
				fmt.Fprint(conn, "502 not implemented\r\n")
			}
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)
	s := smtpSender{host: "127.0.0.1", port: addr.Port, username: "u", password: "secret-pass", from: "a@b.id", fromName: "NEXQIA"}
	err = s.Send(context.Background(), Message{To: "andi@gmail.com", Subject: "x", Text: "t", HTML: "h"})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("expected STARTTLS error, got %v", err)
	}
	if strings.Contains(err.Error(), "secret-pass") {
		t.Fatalf("error must not carry the password: %v", err)
	}
}
