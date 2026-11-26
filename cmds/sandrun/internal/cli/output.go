// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

func (a *app) paint(code, s string) string {
	if !a.color || s == "" {
		return s
	}
	return code + s + ansiReset
}

func (a *app) bold(s string) string { return a.paint(ansiBold, s) }

// safeText escapes control characters, nonprintable Unicode and invalid UTF-8
// bytes so a text field cannot change terminal state or add a line or column.
func safeText(s string) string {
	clean := utf8.ValidString(s)
	for _, r := range s {
		if !plainRune(r) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, "\\x%02x", s[i])
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			fmt.Fprintf(&b, "\\x%02x", r)
		case !plainRune(r):
			if r > 0xffff {
				fmt.Fprintf(&b, "\\U%08x", r)
			} else {
				fmt.Fprintf(&b, "\\u%04x", r)
			}
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

func plainRune(r rune) bool {
	return unicode.IsPrint(r) || unicode.Is(unicode.Zs, r)
}

var jsonOpts = protojson.MarshalOptions{UseProtoNames: true}

// writeJSON writes one JSON value and a newline.
func writeJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}

// protoValue returns m as a generic JSON value. Numbers stay json.Number so
// that they keep the exact decimal text of the protobuf JSON mapping: a
// float64 would print as an exponent (for example 1e+06) in text mode.
func protoValue(m proto.Message) (map[string]any, error) {
	b, err := jsonOpts.Marshal(m)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	out := map[string]any{}
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// printMessage prints m as JSON, or as key-value lines in text mode.
func (a *app) printMessage(m proto.Message) error {
	if a.fmt == FormatJSON {
		b, err := jsonOpts.Marshal(m)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(a.env.Stdout, "%s\n", b)
		return err
	}
	return a.printFields(a.env.Stdout, m)
}

// printFields prints the populated fields of m as "key: value" lines.
func (a *app) printFields(w io.Writer, m proto.Message) error {
	v, err := protoValue(m)
	if err != nil {
		return err
	}
	var lines [][2]string
	flatten("", v, &lines)
	sort.SliceStable(lines, func(i, j int) bool { return lines[i][0] < lines[j][0] })
	for _, l := range lines {
		if _, err := fmt.Fprintf(w, "%s: %s\n", a.paint(ansiCyan, safeText(l[0])), safeText(l[1])); err != nil {
			return err
		}
	}
	return nil
}

// stickyWriter wraps a writer for a sequence of writes whose error is checked
// once at the end: it remembers the first error and drops every later write.
type stickyWriter struct {
	w   io.Writer
	err error
}

func (s *stickyWriter) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	n, err := s.w.Write(p)
	s.err = err
	return n, err
}

func flatten(prefix string, v any, out *[][2]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			flatten(key, x, out)
		}
	case []any:
		parts := make([]string, 0, len(t))
		simple := true
		for _, x := range t {
			switch x.(type) {
			case map[string]any, []any:
				simple = false
			}
			parts = append(parts, scalarText(x))
		}
		if simple {
			*out = append(*out, [2]string{prefix, strings.Join(parts, ", ")})
			return
		}
		for i, x := range t {
			flatten(fmt.Sprintf("%s[%d]", prefix, i), x, out)
		}
	default:
		*out = append(*out, [2]string{prefix, scalarText(t)})
	}
}

// scalarText formats a JSON scalar of protoValue for text mode. A json.Number
// prints its literal, so an integer never takes the exponent form.
func scalarText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	}
	return fmt.Sprint(v)
}

// enumName returns the lowercase value name of an enum without its prefix.
func enumName(e protoreflect.Enum) string {
	d := e.Descriptor().Values().ByNumber(e.Number())
	if d == nil {
		return fmt.Sprint(e.Number())
	}
	name := string(d.Name())
	prefix := strings.ToUpper(camelToSnake(string(e.Descriptor().Name()))) + "_"
	name = strings.TrimPrefix(name, prefix)
	return strings.ReplaceAll(strings.ToLower(name), "_", "-")
}

func camelToSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func ts(t *timestamppb.Timestamp) string {
	if t == nil {
		return ""
	}
	return t.AsTime().UTC().Format(time.RFC3339)
}

// printTable prints a table in text mode and returns the first error of w.
// The tabwriter buffers its output, and a write can flush early, so an error
// can come from a row write as well as from the final flush.
func (a *app) printTable(w io.Writer, headers []string, rows [][]string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for i, h := range headers {
		if i > 0 {
			if _, err := fmt.Fprint(tw, "\t"); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprint(tw, safeText(h)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(tw); err != nil {
		return err
	}
	for _, r := range rows {
		// Escape each cell before inserting the table delimiters.
		cells := make([]string, len(r))
		for i, c := range r {
			cells[i] = safeText(c)
		}
		if _, err := fmt.Fprintln(tw, strings.Join(cells, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// printList prints a list: JSON as the whole message m (a list response
// that holds every item), text as a table.
func (a *app) printList(m proto.Message, headers []string, rows [][]string) error {
	if a.fmt == FormatJSON {
		return a.printMessage(m)
	}
	if a.color {
		for i := range headers {
			headers[i] = strings.ToUpper(headers[i])
		}
	}
	return a.printTable(a.env.Stdout, headers, rows)
}

// printSecret prints a newly issued secret once: in text mode the secret
// alone on standard output and the other fields on standard error; in JSON
// mode one object with both on standard output.
func (a *app) printSecret(meta proto.Message, secretField, secret string) error {
	if a.fmt == FormatJSON {
		v, err := protoValue(meta)
		if err != nil {
			return err
		}
		v[secretField] = secret
		return writeJSON(a.env.Stdout, v)
	}
	// The server issued the secret and shows it only once, so a failure to
	// write the fields on standard error must not stop the secret from
	// reaching standard output. The first error is returned after both writes.
	metaErr := a.printFields(a.env.Stderr, meta)
	if metaErr == nil {
		_, metaErr = fmt.Fprintln(a.env.Stderr, a.paint(ansiYellow, "The secret is shown once; store it now."))
	}
	if _, err := fmt.Fprintln(a.env.Stdout, secret); err != nil {
		return err
	}
	return metaErr
}

// logHandler writes log records as colored text lines.
type logHandler struct {
	mu    *sync.Mutex
	w     io.Writer
	color bool
	level slog.Level
	attrs []slog.Attr
}

func newLogHandler(w io.Writer, color bool, level slog.Level) *logHandler {
	return &logHandler{mu: &sync.Mutex{}, w: w, color: color, level: level}
}

func (h *logHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *logHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	level := r.Level.String()
	if h.color {
		code := ansiDim
		switch {
		case r.Level >= slog.LevelError:
			code = ansiRed
		case r.Level >= slog.LevelWarn:
			code = ansiYellow
		case r.Level >= slog.LevelInfo:
			code = ansiGreen
		}
		level = code + level + ansiReset
	}
	fmt.Fprintf(&b, "%s %s %s", r.Time.Format("15:04:05.000"), level, r.Message)
	write := func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	}
	for _, a := range h.attrs {
		write(a)
	}
	r.Attrs(write)
	b.WriteByte('\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logHandler{mu: h.mu, w: h.w, color: h.color, level: h.level, attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...)}
}

func (h *logHandler) WithGroup(string) slog.Handler { return h }
