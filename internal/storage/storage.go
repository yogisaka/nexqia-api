// internal/storage/storage.go
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
)

// ObjectStorage is the minimal object-store surface the patient-document
// feature needs (spec 2026-10-17-patient-documents-design §7): put, stream
// back, and delete an object by key.
type ObjectStorage interface {
	Put(ctx context.Context, key string, r io.Reader, contentType string) (size int64, sha256Hex string, err error)
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

// ErrNotConfigured is returned by a disabled storage instance (S3_BUCKET
// unset): the API still boots (K7) but any document operation fails with this
// sentinel instead of an opaque transport error.
var ErrNotConfigured = errors.New("object storage is not configured (S3_BUCKET is empty)")

// PATIENT_DOC_MAX_MB default (spec 2026-10-17-patient-documents-design §7).
const defaultMaxUploadMB = 10

// s3Config holds the env-derived settings for the S3-compatible adapter.
type s3Config struct {
	endpoint   string // e.g. http://localhost:9000 (scheme included)
	region     string
	bucket     string
	accessKey  string
	secretKey  string
	pathStyle  bool
	maxUpload  int64
	configured bool // false when S3_BUCKET is empty → disabled instance
}

// NewFromEnv builds the storage adapter from environment variables. An empty
// S3_BUCKET returns a disabled instance (every method returns
// ErrNotConfigured) so the API can start without storage configured (K7);
// a non-numeric or non-positive PATIENT_DOC_MAX_MB fails hard instead.
func NewFromEnv() (ObjectStorage, error) {
	cfg := s3Config{
		endpoint:  os.Getenv("S3_ENDPOINT"),
		region:    os.Getenv("S3_REGION"),
		bucket:    os.Getenv("S3_BUCKET"),
		accessKey: os.Getenv("S3_ACCESS_KEY_ID"),
		secretKey: os.Getenv("S3_SECRET_ACCESS_KEY"),
	}
	cfg.configured = cfg.bucket != ""
	cfg.pathStyle = envDefaultBool("S3_PATH_STYLE", true)

	max, err := maxUploadBytesFromEnv()
	if err != nil {
		return nil, err
	}
	cfg.maxUpload = max

	if cfg.region == "" {
		cfg.region = "us-east-1"
	}
	if cfg.configured {
		if cfg.endpoint == "" {
			return nil, fmt.Errorf("S3_ENDPOINT is required when S3_BUCKET is set")
		}
		if cfg.accessKey == "" || cfg.secretKey == "" {
			return nil, fmt.Errorf("S3_ACCESS_KEY_ID and S3_SECRET_ACCESS_KEY are required when S3_BUCKET is set")
		}
	}
	if !cfg.configured {
		return disabledStorage{maxUpload: cfg.maxUpload}, nil
	}
	return newS3Storage(cfg), nil
}

// MaxUploadBytes reports the PATIENT_DOC_MAX_MB limit in bytes (default 10 MiB).
// A value that failed validation at startup yields 0.
func MaxUploadBytes() int64 {
	n, err := maxUploadBytesFromEnv()
	if err != nil {
		return 0
	}
	return n
}

// maxUploadBytesFromEnv parses PATIENT_DOC_MAX_MB (default 10); a non-numeric
// or non-positive value fails hard at startup rather than accepting uploads
// with a silently wrong size cap.
func maxUploadBytesFromEnv() (int64, error) {
	v := os.Getenv("PATIENT_DOC_MAX_MB")
	if v == "" {
		return defaultMaxUploadMB * 1024 * 1024, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("PATIENT_DOC_MAX_MB must be an integer, got %q", v)
	}
	if n <= 0 {
		return 0, fmt.Errorf("PATIENT_DOC_MAX_MB must be > 0, got %d", n)
	}
	return n * 1024 * 1024, nil
}

func envDefaultBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

// disabledStorage is returned when S3_BUCKET is empty: every operation fails
// with ErrNotConfigured, but the API boots normally (K7).
type disabledStorage struct {
	maxUpload int64
}

func (disabledStorage) Put(context.Context, string, io.Reader, string) (int64, string, error) {
	return 0, "", ErrNotConfigured
}

func (disabledStorage) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, ErrNotConfigured
}

func (disabledStorage) Delete(context.Context, string) error { return ErrNotConfigured }
