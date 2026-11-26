// SPDX-License-Identifier: FSL-1.1-ALv2

// Package vendorhttp is the HTTP client that the agent vendor adapters
// (Cursor, Devin, and later Claude Code and OpenAI Codex) share: JSON
// requests and responses, Server-Sent Event streams, a base URL per vendor
// (the production origin, or an httptest server in tests) and typed status
// errors.
//
// Logging (docs/servers-sandrun-api-foundation.md, Logging): every vendor API
// answer that is not 2xx is logged as vendor_api_error with the vendor, the
// operation name and the HTTP status. The client never logs the
// credential, a request or response body, or query values: request and
// response bodies can carry credentials, session tokens or connect tokens.
// Only the `code` and `error` fields of an error body are kept, cut to a
// short length, because the vendors put their stable error titles there.
package vendorhttp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
)

// Limits of the client.
const (
	// maxBody bounds a JSON response body.
	maxBody = 4 << 20
	// maxEventLine bounds one line of an event stream.
	maxEventLine = 1 << 20
	// maxErrorText bounds the error text that the client keeps.
	maxErrorText = 200
	// DefaultCallTimeout bounds one unary call when the caller's context has
	// no earlier deadline.
	DefaultCallTimeout = 30 * time.Second
)

// Auth sets the credential header of a request.
type Auth func(h http.Header)

// Bearer returns an Auth that sends `Authorization: Bearer <token>`.
func Bearer(token string) Auth {
	return func(h http.Header) { h.Set("Authorization", "Bearer "+token) }
}

// Client is one vendor API with one credential.
type Client struct {
	// Vendor is the vendor label in logs (CURSOR, DEVIN).
	Vendor string
	// BaseURL is the API origin, without a trailing slash.
	BaseURL string
	// HTTP sends the requests; nil selects http.DefaultClient. It must not
	// have a timeout, because event streams stay open.
	HTTP *http.Client
	// Auth sets the credential.
	Auth Auth
	// Logger receives vendor_api_error records.
	Logger *slog.Logger
}

// StatusError is a vendor API answer that is not 2xx.
type StatusError struct {
	Vendor string
	Op     string
	Status int
	// Code and Text are the `code` and `error` (or `message`) fields of the
	// error body, cut to a short length.
	Code string
	Text string
	// RetryAfter is the Retry-After header in seconds, when present.
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("%s %s: HTTP %d", e.Vendor, e.Op, e.Status)
	if e.Code != "" {
		msg += " " + e.Code
	}
	if e.Text != "" {
		msg += ": " + e.Text
	}
	return msg
}

// Status returns the HTTP status of a StatusError in err, or 0.
func Status(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status
	}
	return 0
}

// AsStatus returns the StatusError in err.
func AsStatus(err error) (*StatusError, bool) {
	var se *StatusError
	ok := errors.As(err, &se)
	return se, ok
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

func (c *Client) newRequest(ctx context.Context, method, path string, query url.Values, body any) (*http.Request, error) {
	u := strings.TrimSuffix(c.BaseURL, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("%s: encode request: %w", c.Vendor, err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", c.Vendor, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "sandrun-api")
	if c.Auth != nil {
		c.Auth(req.Header)
	}
	return req, nil
}

// statusError reads the error body of a non-2xx response and logs it.
func (c *Client) statusError(ctx context.Context, op string, resp *http.Response) *StatusError {
	se := &StatusError{Vendor: c.Vendor, Op: op, Status: resp.StatusCode}
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			se.RetryAfter = time.Duration(n) * time.Second
		}
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var body struct {
		Code    any    `json:"code"`
		Error   any    `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &body) == nil {
		se.Code = cut(textOf(body.Code))
		se.Text = cut(textOf(body.Error))
		if se.Text == "" {
			se.Text = cut(body.Message)
		}
	}
	c.logger().LogAttrs(ctx, slog.LevelWarn, "vendor_api_error", slog.String("vendor", c.Vendor), slog.String("op", op),
		slog.Int("status", se.Status), slog.String("code", se.Code), slog.String("error", se.Text))
	return se
}

// textOf returns a string field, or the `message` of an object field.
func textOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		if s, ok := t["message"].(string); ok {
			return s
		}
		if s, ok := t["code"].(string); ok {
			return s
		}
	}
	return ""
}

func cut(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > maxErrorText {
		s = s[:maxErrorText]
	}
	return s
}

// Do sends one JSON request and decodes a 2xx JSON answer into out (nil
// ignores the body). op names the operation in errors and logs (for
// example "claim"); it never contains IDs or credentials. A non-2xx
// answer returns a *StatusError.
func (c *Client) Do(ctx context.Context, op, method, path string, query url.Values, body, out any) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultCallTimeout)
		defer cancel()
	}
	req, err := c.newRequest(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if err := api.CheckTeardown(ctx); err != nil {
		return err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", c.Vendor, op, redact(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return c.statusError(ctx, op, resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("%s %s: read response: %w", c.Vendor, op, err)
	}
	if len(b) > maxBody {
		return fmt.Errorf("%s %s: response larger than %d bytes", c.Vendor, op, maxBody)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", c.Vendor, op, err)
	}
	return nil
}

// redact drops the URL of a transport error, because a URL can carry query
// values.
func redact(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// Event is one Server-Sent Event.
type Event struct {
	// ID is the `id:` field of the event (the resume cursor), or empty.
	ID string
	// Name is the `event:` field, or "message".
	Name string
	// Data is the `data:` lines joined with newlines.
	Data []byte
}

// Stream opens an event stream with GET and calls fn for each event until
// the stream ends (nil), ctx ends (ctx.Err()), fn returns an error (that
// error) or the connection fails. A non-2xx answer returns a *StatusError.
// header adds request headers, for example Last-Event-ID.
func (c *Client) Stream(ctx context.Context, op, path string, query url.Values, header http.Header, fn func(Event) error) error {
	req, err := c.newRequest(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s %s: %w", c.Vendor, op, redact(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return c.statusError(ctx, op, resp)
	}
	err = ReadEvents(resp.Body, fn)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("%s %s: %w", c.Vendor, op, redact(err))
	}
	return nil
}

// ReadEvents parses a text/event-stream body and calls fn for each event,
// following the WHATWG event stream rules. It skips one leading UTF-8 BOM
// and accepts LF, CRLF, and CR line endings. Comment lines start with `:`.
// A blank line dispatches an event with data, joining its `data:` lines with `\n`.
func ReadEvents(r io.Reader, fn func(Event) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxEventLine)
	sc.Split(splitEventLines())
	var ev Event
	var data [][]byte
	hasData := false
	firstLine := true
	for sc.Scan() {
		line := sc.Bytes()
		if firstLine {
			line = bytes.TrimPrefix(line, []byte("\uFEFF"))
			firstLine = false
		}
		if len(line) == 0 {
			if hasData {
				ev.Data = bytes.Join(data, []byte("\n"))
				if ev.Name == "" {
					ev.Name = "message"
				}
				if err := fn(ev); err != nil {
					return err
				}
			}
			ev, data, hasData = Event{ID: ev.ID}, nil, false
			continue
		}
		if line[0] == ':' {
			continue
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			ev.Name = string(value)
		case "data":
			data = append(data, append([]byte(nil), value...))
			hasData = true
		case "id":
			if !bytes.ContainsRune(value, 0) {
				ev.ID = string(value)
			}
		}
	}
	return sc.Err()
}

func splitEventLines() bufio.SplitFunc {
	skipLF := false
	return func(data []byte, atEOF bool) (int, []byte, error) {
		start := 0
		if skipLF && len(data) > 0 {
			skipLF = false
			if data[0] == '\n' {
				start = 1
			}
		}
		line := data[start:]
		if end := bytes.IndexAny(line, "\r\n"); end >= 0 {
			// Emit the line at CR so a blank line can dispatch before the next read.
			// Consume a following LF on the next split, even across reads.
			skipLF = line[end] == '\r'
			return start + end + 1, line[:end], nil
		}
		if atEOF && len(line) > 0 {
			return len(data), line, nil
		}
		return start, nil, nil
	}
}
