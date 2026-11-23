// SPDX-License-Identifier: FSL-1.1-ALv2

// Package objectstore signs URLs for snapshot export parts and lists and
// deletes object prefixes in Cloudflare R2 through its S3-compatible API. The
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

// Object is one listed object: its key and its size in bytes.
type Object struct {
	Key  string
	Size int64
}

// Store issues signed URLs, lists prefixes and deletes them.
type Store interface {
	// PresignPut returns a signed upload URL of key that expires after ttl.
	PresignPut(key string, ttl time.Duration) (string, time.Time, error)
	// PresignGet returns a signed download URL of key.
	PresignGet(key string, ttl time.Duration) (string, time.Time, error)
	// ListPrefix returns every object whose key starts with prefix, with its
	// size, in key order. It returns an error, never a partial list, when
	// the store cannot deliver the complete listing.
	ListPrefix(ctx context.Context, prefix string) ([]Object, error)
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
// only the host header signed). The returned expiry is derived from the signed
// X-Amz-Date and X-Amz-Expires values, so it matches what the storage service
// enforces even when the clock has a fractional second.
func (r *R2) presign(method, key string, ttl time.Duration, now time.Time) (string, time.Time, error) {
	if ttl <= 0 || ttl > maxPresignTTL {
		return "", time.Time{}, errors.New("objectstore: invalid signed URL lifetime")
	}
	u, err := r.objectURL(key)
	if err != nil {
		return "", time.Time{}, err
	}
	// X-Amz-Date has one-second resolution and X-Amz-Expires counts whole
	// seconds, so truncate both before computing the expiry.
	signedAt := now.UTC().Truncate(time.Second)
	expires := int64(ttl / time.Second)
	amzDate := signedAt.Format(amzDateFormat)
	date := amzDate[:8]
	scope := date + "/" + r.region() + "/" + service + "/aws4_request"
	q := url.Values{}
	q.Set("X-Amz-Algorithm", algorithm)
	q.Set("X-Amz-Credential", r.AccessKeyID+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", fmt.Sprint(expires))
	q.Set("X-Amz-SignedHeaders", "host")
	canonical := strings.Join([]string{method, u.RawPath, canonicalQuery(q), "host:" + u.Host + "\n", "host", unsignedBody}, "\n")
	toSign := strings.Join([]string{algorithm, amzDate, scope, sha256Hex([]byte(canonical))}, "\n")
	q.Set("X-Amz-Signature", hex.EncodeToString(hmacSHA256(r.signingKey(date), toSign)))
	u.RawQuery = canonicalQuery(q)
	return u.String(), signedAt.Add(time.Duration(expires) * time.Second), nil
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
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

// ListPrefix lists the objects under prefix (ListObjectsV2, following the
// continuation tokens) and implements Store. A truncated page without a
// continuation token is an error: the listing would silently miss objects.
func (r *R2) ListPrefix(ctx context.Context, prefix string) ([]Object, error) {
	var objects []Object
	token := ""
	for {
		u, err := url.Parse(strings.TrimSuffix(r.Endpoint, "/") + "/" + r.Bucket)
		if err != nil {
			return nil, err
		}
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		u.RawQuery = q.Encode()
		resp, err := r.do(ctx, http.MethodGet, u)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("objectstore: list status %d", resp.StatusCode)
		}
		var list listResult
		if err := xml.Unmarshal(body, &list); err != nil {
			return nil, err
		}
		for _, obj := range list.Contents {
			objects = append(objects, Object{Key: obj.Key, Size: obj.Size})
		}
		if !list.IsTruncated {
			return objects, nil
		}
		if list.NextContinuationToken == "" {
			return nil, errors.New("objectstore: truncated list without a continuation token")
		}
		token = list.NextContinuationToken
	}
}

// DeletePrefix lists the objects under prefix with ListPrefix and deletes
// them one by one. It returns the first error; the caller retries.
func (r *R2) DeletePrefix(ctx context.Context, prefix string) error {
	objects, err := r.ListPrefix(ctx, prefix)
	if err != nil {
		return err
	}
	for _, obj := range objects {
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
	return nil
}

// Memory is an in-memory Store for tests. Signed URLs have the form
// memory://<method>/<key>?expires=<unix>.
type Memory struct {
	mu      sync.Mutex
	Objects map[string][]byte
	// Deleted records every DeletePrefix call.
	Deleted []string
	// Sizes holds the size of an object that PutSized added without
	// content; Objects holds the others.
	Sizes map[string]int64
	// Fail makes DeletePrefix fail while it is set.
	Fail error
	// ListFail makes ListPrefix fail while it is set.
	ListFail error
	// Lists records every ListPrefix call.
	Lists []string
	Now   func() time.Time
}

// NewMemory returns an empty store.
func NewMemory(now func() time.Time) *Memory {
	if now == nil {
		now = time.Now
	}
	return &Memory{Objects: map[string][]byte{}, Sizes: map[string]int64{}, Now: now}
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
	for k := range m.Sizes {
		if strings.HasPrefix(k, prefix) {
			delete(m.Sizes, k)
		}
	}
	return nil
}

// ListPrefix implements Store.
func (m *Memory) ListPrefix(_ context.Context, prefix string) ([]Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Lists = append(m.Lists, prefix)
	if m.ListFail != nil {
		return nil, m.ListFail
	}
	var out []Object
	for k, v := range m.Objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, Object{Key: k, Size: int64(len(v))})
		}
	}
	for k, size := range m.Sizes {
		if strings.HasPrefix(k, prefix) {
			out = append(out, Object{Key: k, Size: size})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// PutObject adds or replaces the object key with data.
func (m *Memory) PutObject(key string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Sizes, key)
	m.Objects[key] = data
}

// PutSized adds or replaces the object key with an object of size bytes
// that the store does not keep in memory.
func (m *Memory) PutSized(key string, size int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Objects, key)
	m.Sizes[key] = size
}

// RemoveObject deletes the object key, if any.
func (m *Memory) RemoveObject(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Objects, key)
	delete(m.Sizes, key)
}

// ListCalls returns the number of ListPrefix calls so far.
func (m *Memory) ListCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Lists)
}

// SetListFail sets or clears the ListPrefix failure.
func (m *Memory) SetListFail(err error) {
	m.mu.Lock()
	m.ListFail = err
	m.mu.Unlock()
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
