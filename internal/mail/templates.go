package mail

import (
	"bytes"
	"embed"
	htmltemplate "html/template"
	texttemplate "text/template"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

var (
	textTemplates = texttemplate.Must(texttemplate.ParseFS(templateFS, "templates/*.txt.tmpl"))
	htmlTemplates = htmltemplate.Must(htmltemplate.ParseFS(templateFS, "templates/*.html.tmpl"))
)

const (
	subjectWelcome          = "Selamat datang di NEXQIA — kode perusahaan Anda"
	subjectNewDeviceLogin   = "Login baru ke akun NEXQIA Anda"
	subjectCredentialExpiry = "Pengingat: masa berlaku STR/SIP/SIPA"
)

// WelcomeData fills the registration email (spec §3.4). Never carries a password.
type WelcomeData struct {
	FullName    string
	CompanyName string
	CompanyCode string
	Username    string
	LoginURL    string
}

// NewDeviceData fills the new-device login alert (spec §3.4).
type NewDeviceData struct {
	Username    string
	When        string
	Device      string
	IP          string
	SecurityURL string
}

// RenderWelcome renders the registration email; the caller sets To.
func RenderWelcome(d WelcomeData) (Message, error) {
	return render("welcome", subjectWelcome, d)
}

// RenderNewDeviceLogin renders the new-device login alert; the caller sets To.
func RenderNewDeviceLogin(d NewDeviceData) (Message, error) {
	return render("new_device_login", subjectNewDeviceLogin, d)
}

// CredentialExpiryItem fills one license line of the expiry reminder.
type CredentialExpiryItem struct {
	StaffName      string
	CredentialKind string // str | sip | sipa
	LicenseNumber  string
	ValidUntil     string // YYYY-MM-DD
	DaysRemaining  int    // negatif = sudah lewat
}

// CredentialExpiryData fills the periodic credential expiry reminder
// (spec 2026-10-20-staff-credential-expiry-design §4.3). IsAdmin = true for
// the merchant admin digest, false for the license owner's own summary.
type CredentialExpiryData struct {
	RecipientName string
	MerchantName  string
	IsAdmin       bool
	Items         []CredentialExpiryItem
}

// RenderCredentialExpiry renders the credential expiry reminder; the caller sets To.
func RenderCredentialExpiry(d CredentialExpiryData) (Message, error) {
	return render("staff_credential_expiry", subjectCredentialExpiry, d)
}

func render(name, subject string, data any) (Message, error) {
	var text, html bytes.Buffer
	if err := textTemplates.ExecuteTemplate(&text, name+".txt.tmpl", data); err != nil {
		return Message{}, err
	}
	if err := htmlTemplates.ExecuteTemplate(&html, name+".html.tmpl", data); err != nil {
		return Message{}, err
	}
	return Message{Subject: subject, Text: text.String(), HTML: html.String()}, nil
}
