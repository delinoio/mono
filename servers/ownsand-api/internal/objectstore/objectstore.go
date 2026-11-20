// SPDX-License-Identifier: FSL-1.1-ALv2

// Package objectstore signs URLs for snapshot export parts and deletes
// object prefixes in Cloudflare R2 through its S3-compatible API. The
// signer is pluggable: tests use Memory. The owning contract is the Storage
// section of docs/servers-ownsand-api-foundation.md.
package objectstore

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store issues signed URLs and deletes prefixes.
type Store interface {
	// PresignPut returns a signed upload URL of key that expires after ttl.
	PresignPut(key string, ttl time.Duration) (string, time.Time, error)
	// PresignGet returns a signed download URL of key.
	PresignGet(key string, ttl time.Duration) (string, time.Time, error)
	// DeletePrefix deletes every object whose key starts with prefix.
	DeletePrefix(ctx context.Context, prefix string) error
}

// R2 is an S3 SigV4 client of one bucket. Signed URLs use path-style
// addressing: <endpoint>/<bucket>/<key>.
type R2 struct {
	Endpoint        string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	// Region is `auto` for R2.
	Region string
	Client *http.Client
	Now    func() time.Time
}

const (
	service       = "s3"
	algorithm     = "AWS4-HMAC-SHA256"
	unsignedBody  = "UNSIGNED-PAYLOAD"
	amzDateFormat = "20060102T150405Z"
	maxPresignTTL = 7 * 24 * time.Hour
)

func (r *R2) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *R2) region() string {
	if r.Region == "" {
		return "auto"
	}
	return r.Region
}

// uriEncode encodes a string as SigV4 requires: unreserved characters stay,
// everything else is percent-encoded; slashes stay in paths.
func uriEncode(s string, path bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && path:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, uriEncode(k, false)+"="+uriEncode(v, false))
		}
	}
	return strings.Join(parts, "&")
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (r *R2) signingKey(date string) []byte {
	k := hmacSHA256([]byte("AWS4"+r.SecretAccessKey), date)
	k = hmacSHA256(k, r.region())
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

func (r *R2) objectURL(key string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSuffix(r.Endpoint, "/"))
	if err != nil {
		return nil, err
	}
	// An empty bucket means a virtual-hosted endpoint whose host names the
	// bucket.
	if r.Bucket == "" {
		u.Path = "/" + key
	} else {
		u.Path = "/" + r.Bucket + "/" + key
	}
	u.RawPath = uriEncode(u.Path, true)
	return u, nil
}

// presign builds a query-string signed URL (SigV4 with UNSIGNED-PAYLOAD and
// only the host header signed).
func (r *R2) presign(method, key string, ttl time.Duration, now time.Time) (string, time.Time, error) {
	if ttl <= 0 || ttl > maxPresignTTL {
		return "", time.Time{}, errors.New("objectstore: invalid signed URL lifetime")
	}
	u, err := r.objectURL(key)
	if err != nil {
		return "", time.Time{}, err
	}
	amzDate := now.Format(amzDateFormat)
	date := amzDate[:8]
	scope := date + "/" + r.region() + "/" + service + "/aws4_request"
	q := url.Values{}
	q.Set("X-Amz-Algorithm", algorithm)
	q.Set("X-Amz-Credential", r.AccessKeyID+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", fmt.Sprint(int64(ttl/time.Second)))
	q.Set("X-Amz-SignedHeaders", "host")
	canonical := strings.Join([]string{method, u.RawPath, canonicalQuery(q), "host:" + u.Host + "\n", "host", unsignedBody}, "\n")
	toSign := strings.Join([]string{algorithm, amzDate, scope, sha256Hex([]byte(canonical))}, "\n")
	q.Set("X-Amz-Signature", hex.EncodeToString(hmacSHA256(r.signingKey(date), toSign)))
	u.RawQuery = canonicalQuery(q)
	return u.String(), now.Add(ttl), nil
}

// PresignPut implements Store.
func (r *R2) PresignPut(key string, ttl time.Duration) (string, time.Time, error) {
	return r.presign(http.MethodPut, key, ttl, r.now())
}

// PresignGet implements Store.
func (r *R2) PresignGet(key string, ttl time.Duration) (string, time.Time, error) {
	return r.presign(http.MethodGet, key, ttl, r.now())
}

// do sends a header-signed request without a body.
func (r *R2) do(ctx context.Context, method string, u *url.URL) (*http.Response, error) {
	now := r.now()
	amzDate := now.Format(amzDateFormat)
	date := amzDate[:8]
	scope := date + "/" + r.region() + "/" + service + "/aws4_request"
	payloadHash := sha256Hex(nil)
	if u.RawPath == "" {
		u.RawPath = uriEncode(u.Path, true)
	}
	canonicalHeaders := "host:" + u.Host + "\nx-amz-content-sha256:" + payloadHash + "\nx-amz-date:" + amzDate + "\n"
	signed := "host;x-amz-content-sha256;x-amz-date"
	canonical := strings.Join([]string{method, u.RawPath, canonicalQuery(u.Query()), canonicalHeaders, signed, payloadHash}, "\n")
	toSign := strings.Join([]string{algorithm, amzDate, scope, sha256Hex([]byte(canonical))}, "\n")
	signature := hex.EncodeToString(hmacSHA256(r.signingKey(date), toSign))
	u.RawQuery = canonicalQuery(u.Query())
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s", algorithm, r.AccessKeyID, scope, signed, signature))
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return client.Do(req)
}

type listResult struct {
	Contents []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

// DeletePrefix lists the objects under prefix (ListObjectsV2) and deletes
// them one by one. It returns the first error; the caller retries.
func (r *R2) DeletePrefix(ctx context.Context, prefix string) error {
	token := ""
	for {
		u, err := url.Parse(strings.TrimSuffix(r.Endpoint, "/") + "/" + r.Bucket)
		if err != nil {
			return err
		}
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		u.RawQuery = q.Encode()
		resp, err := r.do(ctx, http.MethodGet, u)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("objectstore: list status %d", resp.StatusCode)
		}
		var list listResult
		if err := xml.Unmarshal(body, &list); err != nil {
			return err
		}
		for _, obj := range list.Contents {
			ou, err := r.objectURL(obj.Key)
			if err != nil {
				return err
			}
			resp, err := r.do(ctx, http.MethodDelete, ou)
			if err != nil {
				return err
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
				return fmt.Errorf("objectstore: delete status %d", resp.StatusCode)
			}
		}
		if !list.IsTruncated || list.NextContinuationToken == "" {
			return nil
		}
		token = list.NextContinuationToken
	}
}

// Memory is an in-memory Store for tests. Signed URLs have the form
// memory://<method>/<key>?expires=<unix>.
type Memory struct {
	mu      sync.Mutex
	Objects map[string][]byte
	// Deleted records every DeletePrefix call.
	Deleted []string
	// Fail makes DeletePrefix fail while it is set.
	Fail error
	Now  func() time.Time
}

// NewMemory returns an empty store.
func NewMemory(now func() time.Time) *Memory {
	if now == nil {
		now = time.Now
	}
	return &Memory{Objects: map[string][]byte{}, Now: now}
}

func (m *Memory) sign(method, key string, ttl time.Duration) (string, time.Time, error) {
	exp := m.Now().Add(ttl)
	return fmt.Sprintf("memory://%s/%s?expires=%d", method, key, exp.Unix()), exp, nil
}

// PresignPut implements Store.
func (m *Memory) PresignPut(key string, ttl time.Duration) (string, time.Time, error) {
	return m.sign(http.MethodPut, key, ttl)
}

// PresignGet implements Store.
func (m *Memory) PresignGet(key string, ttl time.Duration) (string, time.Time, error) {
	return m.sign(http.MethodGet, key, ttl)
}

// DeletePrefix implements Store.
func (m *Memory) DeletePrefix(_ context.Context, prefix string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	m.Deleted = append(m.Deleted, prefix)
	for k := range m.Objects {
		if strings.HasPrefix(k, prefix) {
			delete(m.Objects, k)
		}
	}
	return nil
}

// DeletedPrefixes returns a copy of the recorded DeletePrefix calls.
func (m *Memory) DeletedPrefixes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.Deleted...)
}

// SetFail sets or clears the DeletePrefix failure.
func (m *Memory) SetFail(err error) {
	m.mu.Lock()
	m.Fail = err
	m.mu.Unlock()
}
