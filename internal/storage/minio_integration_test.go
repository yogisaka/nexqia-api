//go:build integration

package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// newTestMinio starts a MinIO container (same generic-container pattern as
// newTestRedisClient in internal/server/server_test.go — no dedicated module
// dependency) and returns the object storage pointed at it, with the bucket
// already created via a signed CreateBucket request.
func newTestMinio(t *testing.T, ctx context.Context) ObjectStorage {
	t.Helper()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: "minio/minio",
			Cmd:   []string{"server", "/data"},
			// MinIO menolak overlayfs (/data bawaan container) dengan
			// "file access denied" saat rename — pakai tmpfs agar stabil.
			Tmpfs:        map[string]string{"/data": ""},
			Env:          map[string]string{"MINIO_ROOT_USER": "nexqia", "MINIO_ROOT_PASSWORD": "nexqia-secret"},
			ExposedPorts: []string{"9000/tcp"},
			WaitingFor:   wait.ForListeningPort("9000/tcp"),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("failed to start minio container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("failed to get host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("failed to get port: %v", err)
	}

	t.Setenv("S3_ENDPOINT", "http://"+host+":"+port.Port())
	t.Setenv("S3_REGION", "us-east-1")
	t.Setenv("S3_BUCKET", "nexqia-docs")
	t.Setenv("S3_ACCESS_KEY_ID", "nexqia")
	t.Setenv("S3_SECRET_ACCESS_KEY", "nexqia-secret")
	t.Setenv("S3_PATH_STYLE", "true")
	t.Setenv("PATIENT_DOC_MAX_MB", "10")

	s, err := NewFromEnv()
	if err != nil {
		t.Fatalf("NewFromEnv: %v", err)
	}
	s3, ok := s.(*s3Storage)
	if !ok {
		t.Fatalf("NewFromEnv returned %T, want *s3Storage", s)
	}
	if err := s3.putBucket(ctx); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	return s
}

// TestS3ObjectStorageMinIO exercises the full Put → Get → Delete round trip
// against a real SigV4-signed MinIO (spec 2026-10-17 §11, test A).
func TestS3ObjectStorageMinIO(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	s := newTestMinio(t, ctx)

	// Payload ≥ 1 KB of random bytes (not patient data — test fixture).
	payload := make([]byte, 4096)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	const key = "patients/test-doc-integration.bin"
	const contentType = "application/pdf"

	size, shaHex, err := s.Put(ctx, key, bytes.NewReader(payload), contentType)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if size != int64(len(payload)) {
		t.Errorf("Put size = %d, want %d", size, len(payload))
	}
	if len(shaHex) != 64 {
		t.Errorf("Put sha256Hex = %q, want 64 hex chars", shaHex)
	}

	rc, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("Get returned %d bytes, content differs from uploaded %d bytes", len(got), len(payload))
	}

	// Content-Type terjaga: signed raw GET and inspect the response header.
	s3, ok := s.(*s3Storage)
	if !ok {
		t.Fatalf("storage is %T, want *s3Storage", s)
	}
	req, err := s3.signedRequest(ctx, "GET", key, emptySHA256, nil)
	if err != nil {
		t.Fatalf("signed GET: %v", err)
	}
	resp, err := s3.http.Do(req)
	if err != nil {
		t.Fatalf("raw GET: %v", err)
	}
	gotCT := resp.Header.Get("Content-Type")
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if !strings.HasPrefix(gotCT, contentType) {
		t.Errorf("stored Content-Type = %q, want %q", gotCT, contentType)
	}

	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, key); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("Get after Delete err = %v, want status 404", err)
	}
}
