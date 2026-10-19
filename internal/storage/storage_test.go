package storage

import (
	"strings"
	"testing"
)

func TestNewFromEnvDisabledWithoutBucket(t *testing.T) {
	// S3_BUCKET unset → disabled instance, not a startup error (K7).
	s, err := NewFromEnv()
	if err != nil {
		t.Fatalf("NewFromEnv with empty S3_BUCKET: %v", err)
	}
	if _, _, err := s.Put(t.Context(), "k", strings.NewReader("x"), "text/plain"); err != ErrNotConfigured {
		t.Errorf("Put err = %v, want ErrNotConfigured", err)
	}
	if _, err := s.Get(t.Context(), "k"); err != ErrNotConfigured {
		t.Errorf("Get err = %v, want ErrNotConfigured", err)
	}
	if err := s.Delete(t.Context(), "k"); err != ErrNotConfigured {
		t.Errorf("Delete err = %v, want ErrNotConfigured", err)
	}
}

func TestNewFromEnvParsing(t *testing.T) {
	t.Setenv("S3_ENDPOINT", "http://localhost:9000")
	t.Setenv("S3_REGION", "ap-southeast-1")
	t.Setenv("S3_BUCKET", "nexqia-docs")
	t.Setenv("S3_ACCESS_KEY_ID", "nexqia")
	t.Setenv("S3_SECRET_ACCESS_KEY", "nexqia-secret")
	t.Setenv("S3_PATH_STYLE", "false")
	t.Setenv("PATIENT_DOC_MAX_MB", "25")

	s, err := NewFromEnv()
	if err != nil {
		t.Fatalf("NewFromEnv: %v", err)
	}
	s3, ok := s.(*s3Storage)
	if !ok {
		t.Fatalf("NewFromEnv returned %T, want *s3Storage", s)
	}
	if s3.cfg.region != "ap-southeast-1" || s3.cfg.pathStyle || s3.cfg.maxUpload != 25*1024*1024 {
		t.Errorf("config parsed wrong: %+v", s3.cfg)
	}
	if MaxUploadBytes() != 25*1024*1024 {
		t.Errorf("MaxUploadBytes = %d, want %d", MaxUploadBytes(), 25*1024*1024)
	}
}

func TestNewFromEnvInvalidMaxMB(t *testing.T) {
	t.Setenv("S3_BUCKET", "") // disabled path still validates the size cap
	t.Setenv("PATIENT_DOC_MAX_MB", "zero")
	if _, err := NewFromEnv(); err == nil {
		t.Error("NewFromEnv with non-numeric PATIENT_DOC_MAX_MB should fail hard")
	}
	t.Setenv("PATIENT_DOC_MAX_MB", "0")
	if _, err := NewFromEnv(); err == nil {
		t.Error("NewFromEnv with PATIENT_DOC_MAX_MB=0 should fail hard")
	}
}

func TestMaxUploadBytesDefault(t *testing.T) {
	t.Setenv("PATIENT_DOC_MAX_MB", "")
	if MaxUploadBytes() != 10*1024*1024 {
		t.Errorf("MaxUploadBytes default = %d, want %d", MaxUploadBytes(), 10*1024*1024)
	}
}

// SigV4 canonical-request vectors (hardcoded expected values, spec contract:
// service s3, full-payload SHA-256, x-amz-content-sha256 header).
func sigV4TestStorage(pathStyle bool) *s3Storage {
	return newS3Storage(s3Config{
		endpoint:   "http://localhost:9000",
		region:     "ap-southeast-1",
		bucket:     "nexqia-docs",
		accessKey:  "nexqia",
		secretKey:  "nexqia-secret",
		pathStyle:  pathStyle,
		configured: true,
	})
}

func TestSigV4CanonicalRequestPut(t *testing.T) {
	s := sigV4TestStorage(true)
	// sha256("hello")
	payloadHash := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	got := buildCanonicalRequest(
		"PUT",
		s.objectPath("patients/doc1.pdf"),
		"",
		map[string]string{
			"host":                 "localhost:9000",
			"x-amz-content-sha256": payloadHash,
			"x-amz-date":           "20261017T000000Z",
		},
		[]string{"host", "x-amz-content-sha256", "x-amz-date"},
		payloadHash,
	)
	want := strings.Join([]string{
		"PUT",
		"/nexqia-docs/patients/doc1.pdf",
		"",
		"host:localhost:9000",
		"x-amz-content-sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
		"x-amz-date:20261017T000000Z",
		"",
		"host;x-amz-content-sha256;x-amz-date",
		"2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
	}, "\n")
	if got != want {
		t.Errorf("canonical request PUT:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestSigV4CanonicalRequestGet(t *testing.T) {
	s := sigV4TestStorage(true)
	got := buildCanonicalRequest(
		"GET",
		s.objectPath("patients/doc1.pdf"),
		"",
		map[string]string{
			"host":                 "localhost:9000",
			"x-amz-content-sha256": emptySHA256,
			"x-amz-date":           "20261017T000000Z",
		},
		[]string{"host", "x-amz-content-sha256", "x-amz-date"},
		emptySHA256,
	)
	want := strings.Join([]string{
		"GET",
		"/nexqia-docs/patients/doc1.pdf",
		"",
		"host:localhost:9000",
		"x-amz-content-sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"x-amz-date:20261017T000000Z",
		"",
		"host;x-amz-content-sha256;x-amz-date",
		emptySHA256,
	}, "\n")
	if got != want {
		t.Errorf("canonical request GET:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestObjectPathEncoding(t *testing.T) {
	s := sigV4TestStorage(true)
	if got := s.objectPath("patients/a b/file+1.pdf"); got != "/nexqia-docs/patients/a%20b/file%2B1.pdf" {
		t.Errorf("objectPath = %q", got)
	}
	virtual := sigV4TestStorage(false)
	if got := virtual.objectPath("k"); got != "/k" {
		t.Errorf("virtual-host objectPath = %q, want /k", got)
	}
	if got := virtual.requestHost(); got != "nexqia-docs.localhost:9000" {
		t.Errorf("virtual-host host = %q", got)
	}
}
