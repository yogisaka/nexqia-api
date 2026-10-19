// internal/storage/s3.go
package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// requestTimeout caps every S3 request (spec §7 adapter contract).
const requestTimeout = 60 * time.Second

// s3Storage talks to any S3-compatible store (MinIO, Biznet Gio Neo) using
// AWS Signature V4 implemented on the stdlib HTTP client — no SDK dependency
// (K4). Error messages never include response bodies or object contents,
// only the HTTP status and the object key.
type s3Storage struct {
	cfg  s3Config
	http *http.Client
}

func newS3Storage(cfg s3Config) *s3Storage {
	return &s3Storage{cfg: cfg, http: &http.Client{Timeout: requestTimeout}}
}

// Put uploads r as a single object, returning its size and SHA-256 hex digest
// (payload hash is computed over the full body, as required by SigV4).
func (s *s3Storage) Put(ctx context.Context, key string, r io.Reader, contentType string) (int64, string, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return 0, "", fmt.Errorf("s3 put %s: read body: %w", key, err)
	}
	payloadHash := sha256Hex(body)
	req, err := s.signedRequest(ctx, http.MethodPut, key, payloadHash, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("s3 put %s: %w", key, err)
	}
	defer drainClose(resp.Body)
	if resp.StatusCode/100 != 2 {
		return 0, "", fmt.Errorf("s3 put %s: status %d", key, resp.StatusCode)
	}
	return int64(len(body)), payloadHash, nil
}

// Get streams the object back; the caller owns closing the body.
func (s *s3Storage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	req, err := s.signedRequest(ctx, http.MethodGet, key, emptySHA256, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("s3 get %s: %w", key, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		drainClose(resp.Body)
		return nil, fmt.Errorf("s3 get %s: status 404", key)
	}
	if resp.StatusCode/100 != 2 {
		drainClose(resp.Body)
		return nil, fmt.Errorf("s3 get %s: status %d", key, resp.StatusCode)
	}
	return resp.Body, nil
}

// Delete removes the object.
func (s *s3Storage) Delete(ctx context.Context, key string) error {
	req, err := s.signedRequest(ctx, http.MethodDelete, key, emptySHA256, nil)
	if err != nil {
		return err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("s3 delete %s: %w", key, err)
	}
	defer drainClose(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("s3 delete %s: status %d", key, resp.StatusCode)
	}
	return nil
}

// putBucket creates the bucket (CreateBucket is a plain PUT /<bucket>) — used
// by tests and initial provisioning against MinIO.
func (s *s3Storage) putBucket(ctx context.Context) error {
	t := time.Now().UTC().Format("20060102T150405Z")
	req, err := s.sign(ctx, http.MethodPut, "/"+s.cfg.bucket, "", emptySHA256, t, nil)
	if err != nil {
		return err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("s3 create bucket %s: %w", s.cfg.bucket, err)
	}
	defer drainClose(resp.Body)
	// 409 BucketAlreadyOwnedByYou is fine (bucket exists).
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusConflict {
		return fmt.Errorf("s3 create bucket %s: status %d", s.cfg.bucket, resp.StatusCode)
	}
	return nil
}

// signedRequest builds and signs an object request for the given key.
func (s *s3Storage) signedRequest(ctx context.Context, method, key, payloadHash string, body io.Reader) (*http.Request, error) {
	t := time.Now().UTC().Format("20060102T150405Z")
	return s.sign(ctx, method, s.objectPath(key), "", payloadHash, t, body)
}

// objectPath renders the request path for an object key: path-style
// "/<bucket>/<key>" or virtual-host style "/<key>" (bucket moves to the host).
func (s *s3Storage) objectPath(key string) string {
	key = strings.TrimPrefix(key, "/")
	var p string
	if s.cfg.pathStyle {
		p = "/" + s.cfg.bucket + "/" + key
	} else {
		p = "/" + key
	}
	return canonicalURI(p)
}

// buildCanonicalRequest renders the SigV4 canonical request:
// Method\nCanonicalURI\nCanonicalQuery\nCanonicalHeaders\nSignedHeaders\nPayloadHash.
func buildCanonicalRequest(method, canonicalPath, query string, headers map[string]string, signed []string, payloadHash string) string {
	canonicalHeaders, signedHeaders := canonicalHeadersBlock(headers, signed)
	return strings.Join([]string{
		method,
		canonicalPath,
		query,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")
}

// sign builds an HTTP request with an AWS SigV4 Authorization header
// (service "s3", full-payload SHA-256, x-amz-content-sha256 header).
func (s *s3Storage) sign(ctx context.Context, method, canonicalPath, query, payloadHash, amzDate string, body io.Reader) (*http.Request, error) {
	host := s.requestHost()
	headers := map[string]string{
		"host":                 host,
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           amzDate,
	}
	signed := []string{"host", "x-amz-content-sha256", "x-amz-date"}

	req, err := http.NewRequestWithContext(ctx, method, s.requestURL(canonicalPath), body)
	if err != nil {
		return nil, fmt.Errorf("s3 %s %s: %w", method, canonicalPath, err)
	}
	for _, h := range signed {
		if h != "host" {
			req.Header.Set(h, headers[h])
		}
	}

	cr := buildCanonicalRequest(method, canonicalPath, query, headers, signed, payloadHash)

	scope := amzDate[:8] + "/" + s.cfg.region + "/s3/aws4_request"
	sts := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(cr)),
	}, "\n")

	key := hmacChain([]byte("AWS4"+s.cfg.secretKey), amzDate[:8], s.cfg.region, "s3", "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, []byte(sts)))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.cfg.accessKey, scope, strings.Join(signed, ";"), signature,
	))
	return req, nil
}

// requestHost returns the Host header: for virtual-host style the bucket is
// prefixed to the endpoint host.
func (s *s3Storage) requestHost() string {
	u, err := url.Parse(s.cfg.endpoint)
	if err != nil {
		return s.cfg.endpoint
	}
	host := u.Host
	if !s.cfg.pathStyle && s.cfg.bucket != "" {
		host = s.cfg.bucket + "." + host
	}
	return host
}

func (s *s3Storage) requestURL(canonicalPath string) string {
	base := strings.TrimSuffix(s.cfg.endpoint, "/")
	if !s.cfg.pathStyle {
		// Virtual-host style: bucket already part of the Host header.
		return base + canonicalPath
	}
	return base + canonicalPath
}

// canonicalURI percent-encodes each path segment (S3 SigV4: encode everything
// except unreserved chars, keep "/" separators literal).
func canonicalURI(p string) string {
	if p == "" {
		p = "/"
	}
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = uriEncode(part)
	}
	return strings.Join(parts, "/")
}

// uriEncode implements AWS URI encoding: unreserved characters [A-Za-z0-9-._~]
// stay literal, everything else becomes %XX uppercase.
func uriEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// canonicalHeadersBlock renders the sorted canonical header lines and the
// semicolon-joined SignedHeaders list (input slice must already be sorted
// lowercase).
func canonicalHeadersBlock(headers map[string]string, signed []string) (string, string) {
	var b strings.Builder
	for _, h := range signed {
		b.WriteString(h)
		b.WriteString(":")
		b.WriteString(strings.TrimSpace(headers[h]))
		b.WriteString("\n")
	}
	return b.String(), strings.Join(signed, ";")
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

// hmacChain derives the SigV4 signing key: date → region → service → "aws4_request".
func hmacChain(secret []byte, date, region, service, terminator string) []byte {
	k := hmacSHA256(secret, []byte(date))
	k = hmacSHA256(k, []byte(region))
	k = hmacSHA256(k, []byte(service))
	return hmacSHA256(k, []byte(terminator))
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// emptySHA256 is the well-known hash of an empty payload (GET/DELETE).
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func drainClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 4096))
	_ = body.Close()
}
