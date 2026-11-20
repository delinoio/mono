// SPDX-License-Identifier: FSL-1.1-ALv2

// Package r2 is a minimal Cloudflare R2 (S3-compatible) client built on the
// Go standard library: AWS Signature Version 4 presigned URLs for the agent's
// part uploads and downloads, and signed list and delete requests for the
// clean-up jobs. Exported snapshots are stored as one object per part
// (organizations/<organization_id>/snapshots/<snapshot_id>/part-<n>), so the
// control plane uses no multipart uploads and has none to abort.
package r2

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
	"strconv"
	"strings"
	"time"
)

// Region is the SigV4 region of R2.
const Region = "auto"

// Client signs requests for one bucket.
type Client struct {
	// Endpoint is the account endpoint, for example
	// https://<account_id>.r2.cloudflarestorage.com. Requests use path-style
	// URLs: <endpoint>/<bucket>/<key>.
	Endpoint        string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string // Secret: never logged.
	Region          string
	HTTP            *http.Client
	Now             func() time.Time
}

func (c *Client) region() string {
	if c.Region == "" {
		return Region
	}
	return c.Region
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

// PartKey returns the object key of one exported part (1-based).
func PartKey(prefix string, part int32) string {
	return fmt.Sprintf("%spart-%05d", prefix, part)
}

// uriEncode encodes like the SigV4 canonical URI and query rules.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' || (c == '/' && !encodeSlash) {
			b.WriteByte(c)
		} else {
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
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (c *Client) signingKey(date string) []byte {
	k := hmacSHA256([]byte("AWS4"+c.SecretAccessKey), date)
	k = hmacSHA256(k, c.region())
	k = hmacSHA256(k, "s3")
	return hmacSHA256(k, "aws4_request")
}

// signature computes the SigV4 signature of a canonical request.
func (c *Client) signature(method, path string, query url.Values, headers map[string]string, payloadHash string, t time.Time) (signed string, sig string) {
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, strings.ToLower(k))
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, n := range names {
		for k, v := range headers {
			if strings.ToLower(k) == n {
				canonHeaders.WriteString(n + ":" + strings.TrimSpace(v) + "\n")
			}
		}
	}
	signed = strings.Join(names, ";")
	canonical := strings.Join([]string{method, uriEncode(path, false), canonicalQuery(query), canonHeaders.String(), signed, payloadHash}, "\n")
	date := t.Format("20060102")
	scope := date + "/" + c.region() + "/s3/aws4_request"
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", t.Format("20060102T150405Z"), scope, sha256Hex([]byte(canonical))}, "\n")
	return signed, hex.EncodeToString(hmacSHA256(c.signingKey(date), toSign))
}

func (c *Client) objectURL(key string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSuffix(c.Endpoint, "/"))
	if err != nil {
		return nil, err
	}
	u.Path = "/" + c.Bucket
	if key != "" {
		u.Path += "/" + key
	}
	return u, nil
}

// Presign returns a presigned URL for method (GET or PUT) on key that expires
// after expires.
func (c *Client) Presign(method, key string, expires time.Duration) (string, error) {
	u, err := c.objectURL(key)
	if err != nil {
		return "", err
	}
	return c.presign(method, u.Host, u.Scheme, u.Path, expires, c.now()), nil
}

func (c *Client) presign(method, host, scheme, path string, expires time.Duration, t time.Time) string {
	q := url.Values{}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", c.AccessKeyID+"/"+t.Format("20060102")+"/"+c.region()+"/s3/aws4_request")
	q.Set("X-Amz-Date", t.Format("20060102T150405Z"))
	q.Set("X-Amz-Expires", strconv.Itoa(int(expires/time.Second)))
	q.Set("X-Amz-SignedHeaders", "host")
	_, sig := c.signature(method, path, q, map[string]string{"host": host}, "UNSIGNED-PAYLOAD", t)
	return scheme + "://" + host + uriEncode(path, false) + "?" + canonicalQuery(q) + "&X-Amz-Signature=" + sig
}

// do sends a header-signed request with an empty body.
func (c *Client) do(ctx context.Context, method string, u *url.URL, query url.Values) (*http.Response, error) {
	t := c.now()
	emptyHash := sha256Hex(nil)
	headers := map[string]string{"host": u.Host, "x-amz-content-sha256": emptyHash, "x-amz-date": t.Format("20060102T150405Z")}
	signed, sig := c.signature(method, u.Path, query, headers, emptyHash, t)
	full := *u
	full.RawQuery = canonicalQuery(query)
	req, err := http.NewRequestWithContext(ctx, method, full.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-amz-content-sha256", emptyHash)
	req.Header.Set("x-amz-date", headers["x-amz-date"])
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s/%s/s3/aws4_request, SignedHeaders=%s, Signature=%s",
		c.AccessKeyID, t.Format("20060102"), c.region(), signed, sig))
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return hc.Do(req)
}

type listResult struct {
	Contents []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

// DeletePrefix deletes every object under prefix.
func (c *Client) DeletePrefix(ctx context.Context, prefix string) error {
	if prefix == "" || !strings.HasSuffix(prefix, "/") {
		return errors.New("refusing to delete a prefix that does not end with /")
	}
	bucketURL, err := c.objectURL("")
	if err != nil {
		return err
	}
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := c.do(ctx, http.MethodGet, bucketURL, q)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("list objects: status %d", resp.StatusCode)
		}
		var res listResult
		if err := xml.Unmarshal(body, &res); err != nil {
			return fmt.Errorf("list objects: %w", err)
		}
		for _, obj := range res.Contents {
			if !strings.HasPrefix(obj.Key, prefix) {
				continue
			}
			u, err := c.objectURL(obj.Key)
			if err != nil {
				return err
			}
			resp, err := c.do(ctx, http.MethodDelete, u, url.Values{})
			if err != nil {
				return err
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
				return fmt.Errorf("delete object: status %d", resp.StatusCode)
			}
		}
		if !res.IsTruncated || res.NextContinuationToken == "" {
			return nil
		}
		token = res.NextContinuationToken
	}
}
