// SPDX-License-Identifier: FSL-1.1-ALv2

package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// MaxHeaderBytes is the request header limit of the production http.Server.
// The request head observer derives its own limit from it, so the observer
// never accepts a head that net/http would reject and never gives up on a head
// that net/http accepts.
const MaxHeaderBytes = http.DefaultMaxHeaderBytes

const (
	// maxObservedHeadBytes matches the limit net/http applies to the first
	// request head of a connection: MaxHeaderBytes plus 4096 bytes for the
	// request line and slack.
	maxObservedHeadBytes = MaxHeaderBytes + 4096
	// maxObservedFollowUpHeadBytes is the limit for a head that follows an
	// earlier head on the same connection. net/http does not charge to a
	// keep-alive request the bytes that its Peek(4) wait after the previous
	// response already buffered, which can be up to the 4096 bytes of its
	// connection buffer, so it accepts a head that large beyond
	// maxObservedHeadBytes. The observer cannot see how many bytes were
	// buffered, so it allows the upper bound. It assumes the 4096-byte
	// connection buffer of net/http: review this value when that size changes.
	maxObservedFollowUpHeadBytes = maxObservedHeadBytes + 4096
	// maxPendingRequests bounds the parsed requests that net/http has read from
	// the connection but not yet dispatched to the handler. net/http reads
	// through a 4 KiB buffer, so a connection cannot lead the handler by more
	// than a few hundred minimal requests; the cap only guards that assumption.
	maxPendingRequests = 1024
	// maxChunkLineBytes and maxTrailerBytes bound the scratch space used to
	// skip a chunked request body. They match the limits net/http applies to a
	// chunk-size line and to the trailer section.
	maxChunkLineBytes = 4096
	maxTrailerBytes   = 4096
	// maxLeadingCRorLF is the number of leading CR or LF bytes that net/http
	// discards before a request line after a POST (numLeadingCRorLF in its
	// server). The observer skips the same number.
	maxLeadingCRorLF = 4
	// peekedHeadBytes is the number of bytes of the next request that net/http
	// waits for under IdleTimeout after a response (it calls Peek(4) on the
	// connection reader). Only once it has them does it arm ReadHeaderTimeout, so
	// fewer bytes than this after a request are still the keep-alive wait.
	peekedHeadBytes = 4
	// retainedHeadCap is the largest head buffer kept between requests, so an
	// idle keep-alive connection does not hold a large buffer.
	retainedHeadCap = 4096
)

// Request head observer
//
// net/http answers a request whose Expect header names an unsupported
// expectation with "417 Expectation Failed" and closes the connection before it
// calls Server.Handler, and it offers no hook for that rejection. The request
// logger inside the handler therefore never sees such a request.
//
// The observer closes that gap without changing how net/http answers. The
// listener returned by ObserveListener wraps every accepted connection:
//
//   - Inbound bytes are fed, synchronously and before net/http sees them, to a
//     bounded incremental parser that splits the stream into request heads,
//     parses each head with http.ReadRequest (the parser net/http uses) and
//     skips the request body by its declared framing. The result is an ordered
//     queue of pending requests (method, path, whether the Expect header is
//     unsupported). The parser keeps at most one request head in memory.
//   - The request logger reports each handler call through the connection
//     state that ConnContext stores in the request context. A handler call
//     dequeues the oldest pending request. Every request that net/http does not
//     answer itself reaches the handler, and a rejection closes the connection,
//     so the oldest pending request is the one net/http handles next. This
//     holds only when http.Server.DisableGeneralOptionsHandler is true:
//     otherwise net/http answers a server-wide "OPTIONS *" request with its
//     built-in handler, the entry of that request stays queued at the front
//     and the pairing is offset for the rest of the connection.
//   - When net/http writes the 417 response while no handler call or response
//     of a handler is in progress and the oldest pending request has an
//     unsupported Expect header, that request is the rejected one and the
//     observer logs its http_request record exactly once, before the response
//     is written, as the request logger does.
//
// The observer never answers, delays or alters traffic and starts no goroutine,
// so a connection that net/http closes releases everything with it. Whenever the
// stream does not parse as net/http would parse it (a malformed or oversized
// head, an invalid chunked body, a full queue) the observer stops parsing and
// the connection continues unobserved: it may then miss the record of a later
// rejected request, but it never logs a request that was not rejected.
//
// net/http discards up to four leading CR or LF bytes before a request line
// when the previous request on the connection was a POST, so the parser skips
// them in that case too and keeps observing the connection. After any other
// method, and at the start of the connection, an empty first line is not a
// request and stops the parsing, as does a fifth leading CR or LF byte.
//
// Other rejections happen before net/http has a request at all, so they have no
// method and path and are logged as http_request_rejected records instead:
//
//   - net/http answers a malformed request (400), a head over the limit (431),
//     an unsupported Transfer-Encoding (501) and an unsupported protocol
//     version (505) with one write of a fixed response shape on the raw
//     connection (see parseDirectErrorResponse). The observer recognizes that
//     write once the response of the last handler call is complete (the
//     ConnState hook reports the connection idle) and logs it once, before the
//     write.
//   - When ReadHeaderTimeout expires, net/http either closes the connection
//     without any response or answers a malformed partial line with the 400 above
//     and closes it. The observer sees the timed-out read of the connection when
//     the read was waiting for a request head. With nothing buffered after the
//     last line break it logs a header_read_timeout record at once. With a
//     partial line buffered it waits for what net/http does: the 400 is logged
//     as the malformed request it is, and a close without a response is logged
//     as header_read_timeout when the connection closes (see ConnState). After
//     an earlier request, net/http arms ReadHeaderTimeout only once four bytes of
//     the next request arrived, so a stall with fewer bytes is the keep-alive
//     wait. A timeout of the keep-alive wait between requests (IdleTimeout), of
//     a request body (ReadTimeout) and the read interruptions that net/http
//     causes itself are not logged; see claimReadTimeout.
//   - A connection that a handler hijacked is no longer net/http's, so the
//     observer never logs for it (see ConnState).

// observerPhase is the position of the incremental parser in the byte stream.
type observerPhase int

const (
	// phaseHead accumulates a request head up to its terminating empty line.
	phaseHead observerPhase = iota
	// phaseBody skips the remaining bytes of a Content-Length body.
	phaseBody
	// phaseChunkSize accumulates a chunk-size line of a chunked body.
	phaseChunkSize
	// phaseChunkData skips the remaining data bytes of a chunk.
	phaseChunkData
	// phaseChunkDataEnd expects the CRLF that follows chunk data.
	phaseChunkDataEnd
	// phaseTrailer skips the trailer section after the last chunk.
	phaseTrailer
	// phaseStopped means the parser gave up or the connection ends after the
	// queued requests. Pending requests stay valid for pairing.
	phaseStopped
)

// observedRequest is the data of a parsed request head that the 417 record
// needs. It never holds header values or bodies.
type observedRequest struct {
	method string
	path   string
	// expectFailed reports that net/http rejects the request with 417.
	expectFailed bool
	// parsedAt is when the head was complete.
	parsedAt time.Time
}

// connObserver is the per-connection parser and pairing state. All methods are
// safe for concurrent use: net/http may read the connection from its background
// reader goroutine, and Server.Shutdown closes connections from another one.
type connObserver struct {
	logger *slog.Logger
	now    func() time.Time

	mu sync.Mutex

	phase     observerPhase
	head      []byte
	lineStart int
	lines     int
	// lastMethod is the method of the last request head that parsed. net/http
	// keeps the same value (conn.lastMethod) and skips leading CR and LF bytes
	// of the next head only after a POST.
	lastMethod string
	// leadingSkipped counts the leading CR and LF bytes skipped before the
	// current head; it is reset when the head parses.
	leadingSkipped int

	// remaining counts the body or chunk data bytes still to skip.
	remaining uint64
	// line accumulates a chunk-size line or the current trailer line.
	line         []byte
	trailerBytes int

	pending []observedRequest
	// responding is set when a handler call starts and cleared when net/http
	// reports the connection idle (see responseFinished). It covers the handler
	// and the rest of its response: net/http writes the buffered tail of the
	// response, which a handler controls, after the handler returned, so the end
	// of the handler call alone does not end the writes that belong to it.
	responding bool
	lastDone   time.Time
	// rejected is set once the connection logged a rejection, so every
	// connection logs at most one, as net/http closes it after each rejection.
	rejected bool
	// hijacked is set when a handler took the connection over.
	hijacked bool
	// timeoutPending is set when a read deadline expired while a partial line of
	// a request head was buffered. net/http answers such a head with a 400 or
	// closes the connection without a response, depending on the partial line,
	// and the observer does not know which until the write or the close, so the
	// header read timeout record waits (see claimReadTimeout). timeoutPendingStart
	// is the start of the request that the record measures.
	timeoutPending      bool
	timeoutPendingStart time.Time

	// acceptedAt is when the connection was accepted.
	acceptedAt time.Time
	// requestStart is when the first request head byte arrived that no handler
	// has finished since; it is zero until the next head byte arrives. Body bytes
	// never set it (see feedHead). It is the start of the request that a
	// rejection record measures.
	requestStart time.Time
	// parsedHeads counts the request heads that parsed. A read timeout on a
	// connection whose parser is waiting for the first byte of a head is the
	// header read timeout only while this is zero; later it is the idle wait.
	parsedHeads int

	// readDeadline is the last read deadline net/http set, and deadlineSetAt is
	// when it set it; see deadlineExpired.
	readDeadline  time.Time
	deadlineSetAt time.Time
}

func newConnObserver(logger *slog.Logger) *connObserver {
	o := &connObserver{logger: logger, now: time.Now}
	o.acceptedAt = o.now()
	return o
}

// feed consumes inbound bytes in stream order.
func (o *connObserver) feed(p []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for len(p) > 0 && o.phase != phaseStopped {
		switch o.phase {
		case phaseHead:
			p = o.feedHead(p)
		case phaseBody, phaseChunkData:
			n := uint64(len(p))
			if n > o.remaining {
				n = o.remaining
			}
			p = p[n:]
			o.remaining -= n
			if o.remaining == 0 {
				if o.phase == phaseBody {
					o.phase = phaseHead
				} else {
					o.phase = phaseChunkDataEnd
					o.line = o.line[:0]
				}
			}
		case phaseChunkDataEnd:
			p = o.feedChunkDataEnd(p)
		case phaseChunkSize:
			p = o.feedChunkSize(p)
		case phaseTrailer:
			p = o.feedTrailer(p)
		}
	}
}

func (o *connObserver) stop() {
	o.phase = phaseStopped
	o.head = nil
	o.line = nil
}

// feedHead appends bytes to the current head until its empty line and returns
// the unconsumed rest.
func (o *connObserver) feedHead(p []byte) []byte {
	// Only head bytes can start the request that a rejection record measures.
	// Body bytes belong to a request that already started: net/http drains the
	// unread body of a request after its handler returned, and those bytes must
	// not become the start of the next request. A read that carries the end of a
	// body and the start of the next head is stamped here, at that read's time.
	if len(p) > 0 && o.requestStart.IsZero() {
		o.requestStart = o.now()
	}
	headLimit := maxObservedHeadBytes
	if o.parsedHeads > 0 {
		headLimit = maxObservedFollowUpHeadBytes
	}
	for i, b := range p {
		// net/http discards up to maxLeadingCRorLF CR or LF bytes before a request
		// line when the previous request on the connection was a POST (a tolerance
		// for clients that send an extra CRLF after a body). Skip the same bytes,
		// wherever the reads split them. Any other byte, and every further CR or
		// LF byte, is parsed as part of the head, so a fifth one still ends the
		// parsing as net/http answers it with 400.
		if len(o.head) == 0 && (b == '\r' || b == '\n') && o.lastMethod == http.MethodPost && o.leadingSkipped < maxLeadingCRorLF {
			o.leadingSkipped++
			continue
		}
		o.head = append(o.head, b)
		if len(o.head) > headLimit {
			o.stop()
			return nil
		}
		if b != '\n' {
			continue
		}
		// textproto accepts a bare LF as a line end and ends the head at the
		// first empty line after the request line.
		line := bytes.TrimSuffix(o.head[o.lineStart:len(o.head)-1], []byte("\r"))
		if len(line) == 0 {
			if o.lines == 0 {
				o.stop()
				return nil
			}
			o.finishHead()
			return p[i+1:]
		}
		o.lineStart = len(o.head)
		o.lines++
	}
	return nil
}

// finishHead parses the completed head and chooses how to skip its body.
func (o *connObserver) finishHead() {
	head := o.head
	o.lineStart, o.lines = 0, 0
	o.leadingSkipped = 0
	req, ok := parseObservedHead(head)
	if cap(o.head) > retainedHeadCap {
		o.head = nil
	} else {
		o.head = o.head[:0]
	}
	if !ok || len(o.pending) >= maxPendingRequests {
		o.stop()
		return
	}
	o.parsedHeads++
	o.lastMethod = req.Method
	observed := observedRequest{
		method:       req.Method,
		path:         req.URL.Path,
		expectFailed: unsupportedExpect(req.Header),
		parsedAt:     o.now(),
	}
	o.pending = append(o.pending, observed)

	switch {
	case req.Close || observed.expectFailed:
		// net/http closes the connection after this request, or after the 417.
		o.stop()
	case len(req.TransferEncoding) == 1 && req.TransferEncoding[0] == "chunked":
		o.phase = phaseChunkSize
		o.line = o.line[:0]
	case req.ContentLength > 0:
		o.phase = phaseBody
		o.remaining = uint64(req.ContentLength)
	case req.ContentLength == 0:
		o.phase = phaseHead
	default:
		o.stop()
	}
}

// parseObservedHead parses a complete request head with the parser net/http
// uses. It reports false unless the parser accepts the head and consumes
// exactly all of its bytes, which is how a disagreement about where the head
// ends shows up.
func parseObservedHead(head []byte) (*http.Request, bool) {
	rd := bytes.NewReader(head)
	br := bufio.NewReaderSize(rd, len(head))
	req, err := http.ReadRequest(br)
	if err != nil || rd.Len()+br.Buffered() != 0 {
		return nil, false
	}
	return req, true
}

// feedChunkSize accumulates and interprets a chunk-size line.
func (o *connObserver) feedChunkSize(p []byte) []byte {
	for i, b := range p {
		o.line = append(o.line, b)
		if len(o.line) > maxChunkLineBytes {
			o.stop()
			return nil
		}
		if b != '\n' {
			continue
		}
		size, ok := parseChunkSize(o.line)
		o.line = o.line[:0]
		if !ok {
			o.stop()
			return nil
		}
		if size == 0 {
			o.phase = phaseTrailer
			o.trailerBytes = 0
		} else {
			o.phase = phaseChunkData
			o.remaining = size
		}
		return p[i+1:]
	}
	return nil
}

// feedChunkDataEnd expects the CRLF after chunk data, byte by byte.
func (o *connObserver) feedChunkDataEnd(p []byte) []byte {
	want := "\r\n"
	for i, b := range p {
		if b != want[len(o.line)] {
			o.stop()
			return nil
		}
		o.line = append(o.line, b)
		if len(o.line) == len(want) {
			o.line = o.line[:0]
			o.phase = phaseChunkSize
			return p[i+1:]
		}
	}
	return nil
}

// feedTrailer skips the trailer section: lines up to the first empty line.
func (o *connObserver) feedTrailer(p []byte) []byte {
	for i, b := range p {
		o.trailerBytes++
		if o.trailerBytes > maxTrailerBytes {
			o.stop()
			return nil
		}
		if b != '\n' {
			o.line = append(o.line, b)
			continue
		}
		empty := len(bytes.TrimSuffix(o.line, []byte("\r"))) == 0
		o.line = o.line[:0]
		if empty {
			o.phase = phaseHead
			return p[i+1:]
		}
	}
	return nil
}

// parseChunkSize parses a chunk-size line the way net/http does. The line,
// which includes its final LF, must end in exactly one CRLF with no other CR.
// Trailing spaces and tabs and a chunk extension are dropped and the rest must
// be a hex number of at most 16 digits.
func parseChunkSize(line []byte) (uint64, bool) {
	// net/http's readChunkLine rejects a bare LF and any CR before the line
	// end, so check this before trimming anything.
	if i := bytes.IndexByte(line, '\r'); i < 0 || i != len(line)-2 {
		return 0, false
	}
	// After it removes the CRLF, net/http trims only spaces and tabs.
	line = bytes.TrimRight(line[:len(line)-2], " \t")
	if i := bytes.IndexByte(line, ';'); i >= 0 {
		line = line[:i]
	}
	if len(line) == 0 || len(line) > 16 {
		return 0, false
	}
	// With base 16 ParseUint accepts only hex digits: no sign, prefix or
	// underscore.
	size, err := strconv.ParseUint(string(line), 16, 64)
	return size, err == nil
}

// unsupportedExpect reports whether net/http rejects a request with this header
// set with 417: a non-empty Expect header that does not contain the
// 100-continue token. It mirrors the check in net/http's connection loop
// (request.expectsContinue); a test compares it with the real server.
func unsupportedExpect(h http.Header) bool {
	var value string
	if values := h["Expect"]; len(values) > 0 {
		value = values[0]
	}
	return value != "" && !hasToken(value, "100-continue")
}

// hasToken reports whether v contains token as a comma, space or tab delimited,
// case-insensitive element, like the unexported helper of net/http.
func hasToken(v, token string) bool {
	if len(token) > len(v) || token == "" {
		return false
	}
	isBoundary := func(b byte) bool { return b == ' ' || b == ',' || b == '\t' }
	for start := 0; start <= len(v)-len(token); start++ {
		if start > 0 && !isBoundary(v[start-1]) {
			continue
		}
		end := start + len(token)
		if end != len(v) && !isBoundary(v[end]) {
			continue
		}
		if equalFoldASCII(v[start:end], token) {
			return true
		}
	}
	return false
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// handlerStarted records that the request logger runs for the oldest pending
// request of the connection.
func (o *connObserver) handlerStarted() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.responding = true
	if len(o.pending) > 0 {
		o.pending[0] = observedRequest{}
		o.pending = o.pending[1:]
		if len(o.pending) == 0 {
			o.pending = nil
		}
	}
}

// handlerFinished records that a handler call returned or panicked. The next
// pending request cannot start before that, which bounds its logged duration.
// It does not end the response: net/http still flushes the buffered rest of it
// afterwards (see responseFinished).
func (o *connObserver) handlerFinished() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.lastDone = o.now()
	if len(o.head) > 0 || len(o.pending) > 0 || o.leadingSkipped > 0 {
		// Bytes of a later request already arrived, for example the start of the
		// next head in the same read as the request that this handler answered.
		// Skipped leading CR and LF bytes after a POST count too: they are in
		// neither head nor pending, but they belong to the next request.
		// Keep their arrival time: a later byte of that request must not restart
		// the measured duration. rejectionStart bounds it by lastDone, because
		// net/http reads the request only after the handler is done.
		return
	}
	// The next request starts with the next byte.
	o.requestStart = time.Time{}
}

// responseFinished records that net/http finished the response of the last
// handler call and is waiting for, or reading, the next request. It runs from
// the StateIdle hook, which net/http calls after it flushed the response and
// before it reads the next request. Every outbound write until then belongs to
// the response of a handler, whatever bytes it has.
func (o *connObserver) responseFinished() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.responding = false
}

// expectationFailedStatus is the status line of every 417 response of net/http
// after the protocol version, which net/http copies from the request (HTTP/1.0
// or HTTP/1.1). net/http adds "Connection: close" to the response, which a
// handler answering 417 itself does not do unless it closes the connection too.
const expectationFailedStatus = " 417 Expectation Failed\r\n"

// isExpectationFailedResponse reports whether p starts the response that
// net/http writes for an unsupported expectation.
func isExpectationFailedResponse(p []byte) bool {
	const versionLen = len("HTTP/1.1")
	if len(p) < versionLen || !bytes.HasPrefix(p, []byte("HTTP/1.")) || (p[versionLen-1] != '0' && p[versionLen-1] != '1') {
		return false
	}
	if !bytes.HasPrefix(p[versionLen:], []byte(expectationFailedStatus)) {
		return false
	}
	// Start at the CRLF that ends the status line so that the first header line
	// is matched like every other one.
	headers := p[versionLen+len(expectationFailedStatus)-2:]
	if end := bytes.Index(headers, []byte("\r\n\r\n")); end >= 0 {
		headers = headers[:end+2]
	}
	return bytes.Contains(headers, []byte("\r\nConnection: close\r\n"))
}

// claimRejection reports the request that this outbound write rejects, if any,
// and claims it so it is logged exactly once.
func (o *connObserver) claimRejection(p []byte) (observedRequest, time.Time, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.rejected || o.hijacked || o.responding || len(o.pending) == 0 || !o.pending[0].expectFailed || !isExpectationFailedResponse(p) {
		return observedRequest{}, time.Time{}, false
	}
	o.rejected = true
	request := o.pending[0]
	o.pending = nil
	// The request waited behind earlier pipelined requests on the connection, so
	// it started when the last handler finished if that was later.
	start := request.parsedAt
	if o.lastDone.After(start) {
		start = o.lastDone
	}
	return request, start, true
}

// logRejection writes the http_request record of a request that net/http
// rejected before the handler ran. The response carries no body.
func (o *connObserver) logRejection(request observedRequest, start time.Time) {
	logRequest(context.Background(), o.logger, request.method, request.path, http.StatusExpectationFailed, o.now().Sub(start), 0)
}

// RejectionReason identifies why net/http rejected a request before the
// handler ran. The values are the stable `reason` field of the
// http_request_rejected record.
type RejectionReason string

const (
	// RejectionMalformedRequest is a request net/http cannot parse or accept:
	// a malformed request line or header (400).
	RejectionMalformedRequest RejectionReason = "malformed_request"
	// RejectionHeaderTooLarge is a request head over MaxHeaderBytes (431).
	RejectionHeaderTooLarge RejectionReason = "request_header_too_large"
	// RejectionUnsupportedTransferEncoding is a Transfer-Encoding other than
	// chunked (501).
	RejectionUnsupportedTransferEncoding RejectionReason = "unsupported_transfer_encoding"
	// RejectionUnsupportedProtocolVersion is a request of an HTTP version that
	// net/http does not serve (505).
	RejectionUnsupportedProtocolVersion RejectionReason = "unsupported_protocol_version"
	// RejectionHeaderReadTimeout is a client that did not send a complete
	// request head within ReadHeaderTimeout and that net/http closed without a
	// response. A partial line that net/http answers with a 400 is a
	// RejectionMalformedRequest instead.
	RejectionHeaderReadTimeout RejectionReason = "header_read_timeout"
	// RejectionOther is any other status that net/http answers directly.
	RejectionOther RejectionReason = "rejected"
)

// directErrorHeaders are the headers of every response that net/http's
// connection loop writes itself for a request it rejects, in this order and
// with nothing else. net/http writes the status line, these headers and the
// body in one write to the raw connection and never adds Date or
// Content-Length. A response of a handler, or of a request that reached the
// handler, always carries Date, so it never has this shape.
const directErrorHeaders = "\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\n\r\n"

// parseDirectErrorResponse recognizes the response that net/http's connection
// loop writes when it rejects a request before it exists as a request, and
// returns its status. The status line is "HTTP/1.1 <code> <status text>",
// optionally followed by ": <detail>" (net/http adds the detail for a missing
// Host header and the like), and always says HTTP/1.1 whatever the client
// sent.
func parseDirectErrorResponse(p []byte) (int, bool) {
	const prefix = "HTTP/1.1 "
	if !bytes.HasPrefix(p, []byte(prefix)) || len(p) < len(prefix)+3 {
		return 0, false
	}
	code, err := strconv.Atoi(string(p[len(prefix) : len(prefix)+3]))
	if err != nil || code < 400 || code > 599 {
		return 0, false
	}
	text := http.StatusText(code)
	if text == "" {
		return 0, false
	}
	rest := p[len(prefix)+3:]
	if !bytes.HasPrefix(rest, []byte(" "+text)) {
		return 0, false
	}
	rest = rest[1+len(text):]
	if bytes.HasPrefix(rest, []byte(": ")) {
		end := bytes.Index(rest, []byte("\r\n"))
		if end < 0 {
			return 0, false
		}
		rest = rest[end:]
	}
	if !bytes.HasPrefix(rest, []byte(directErrorHeaders)) {
		return 0, false
	}
	return code, true
}

// rejectionReasonFor maps the status of a direct net/http error response to its
// reason.
func rejectionReasonFor(status int) RejectionReason {
	switch status {
	case http.StatusBadRequest:
		return RejectionMalformedRequest
	case http.StatusRequestHeaderFieldsTooLarge:
		return RejectionHeaderTooLarge
	case http.StatusNotImplemented:
		return RejectionUnsupportedTransferEncoding
	case http.StatusHTTPVersionNotSupported:
		return RejectionUnsupportedProtocolVersion
	default:
		return RejectionOther
	}
}

// rejectionStart returns when the request that is being rejected started: with
// its first byte, or with the end of the last handler call when its bytes
// arrived while that handler was still running, or with the accepted
// connection. The caller holds o.mu.
func (o *connObserver) rejectionStart() time.Time {
	start := o.acceptedAt
	if !o.requestStart.IsZero() {
		start = o.requestStart
	}
	if o.lastDone.After(start) {
		start = o.lastDone
	}
	return start
}

// claimDirectRejection reports whether this outbound write is a response that
// net/http wrote for a request it rejected before the handler, and claims it so
// it is logged exactly once.
//
// A response is only claimed once the response of the last handler call is
// complete (net/http reports the connection idle), so it cannot be bytes of a
// handler's response, and never on a hijacked connection. The shape check does
// not depend on the parser, so it also works after the parser stopped on the
// malformed or oversized head that caused the response.
func (o *connObserver) claimDirectRejection(p []byte) (RejectionReason, int, time.Time, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.rejected || o.hijacked || o.responding {
		return "", 0, time.Time{}, false
	}
	status, ok := parseDirectErrorResponse(p)
	if !ok {
		return "", 0, time.Time{}, false
	}
	o.rejected = true
	// The 400 that net/http writes after a header timeout inside a malformed
	// partial line is the record of that request; the timeout is not logged too.
	o.timeoutPending = false
	return rejectionReasonFor(status), status, o.rejectionStart(), true
}

// setReadDeadline records the read deadline that net/http sets on the
// connection. net/http also sets a deadline that already passed to interrupt a
// pending background read; such an interruption is told apart from a timer
// expiry by deadlineExpired.
func (o *connObserver) setReadDeadline(t time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.readDeadline = t
	o.deadlineSetAt = o.now()
}

// deadlineExpired reports whether a read deadline timer expired, as opposed to
// net/http interrupting a read with a deadline in the past (it does so to stop
// the background read of a finished request, and a handler may do the same).
// A deadline that was already in the past when it was set cannot have
// expired. The caller holds o.mu.
func (o *connObserver) deadlineExpired() bool {
	return !o.readDeadline.IsZero() && o.readDeadline.After(o.deadlineSetAt)
}

// claimReadTimeout reports whether a read of the connection that failed with a
// timeout was waiting for a request head that did not arrive in time, and
// claims it so it is logged exactly once as a header read timeout without a
// response.
//
// Only a deadline timer that expired while the parser waited for (more of) a
// request head counts, with no handler call or response in progress:
//
//   - While no request head ever parsed on the connection, any partly received
//     head counts, and so does an empty one: a connection that connects and
//     stalls is logged, because the contract covers every header read timeout,
//     and so is one that sends nothing for ReadHeaderTimeout.
//   - After a request, net/http waits for the first four bytes of the next one
//     under IdleTimeout and arms ReadHeaderTimeout only once it has them (see
//     peekedHeadBytes). A stall with fewer than four bytes of the next request is
//     therefore the keep-alive wait, a normal close that is not logged. From four
//     bytes on, the stall is a header read timeout, whatever deadline the
//     observer sees. Leading CR and LF bytes that the parser skipped after a POST
//     count as received bytes, because net/http has received them too.
//   - A timeout while the parser skips a body (ReadTimeout), or after it
//     stopped, is not a header timeout.
//
// What net/http does after the timeout depends on the bytes after the last line
// break of the head. With none, it closes the connection without a response and
// the timeout is claimed here. With a partial line, net/http parses that line as
// if it were complete: it answers the 400 of a malformed request when the line
// is invalid at its position, and closes the connection without a response when
// it is a valid start of a line (and always during a shutdown). The observer
// does not copy that validation. It leaves the timeout pending instead:
// claimDirectRejection claims the 400 as a malformed request, and
// claimPendingTimeout claims the timeout when the connection closes without one.
func (o *connObserver) claimReadTimeout() (time.Time, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.rejected || o.hijacked || o.responding || o.phase != phaseHead || !o.deadlineExpired() {
		return time.Time{}, false
	}
	// The observer cannot see which deadline net/http set, but it sees every byte
	// first, so the bytes of the next request that it received (the head buffer
	// plus the leading CR and LF bytes skipped after a POST, which net/http has
	// received too) are a lower bound of what net/http has buffered. Below
	// peekedHeadBytes net/http is still in the keep-alive wait under IdleTimeout.
	if o.parsedHeads > 0 && len(o.head)+o.leadingSkipped < peekedHeadBytes {
		return time.Time{}, false
	}
	if len(o.head) > o.lineStart {
		if !o.timeoutPending {
			o.timeoutPending = true
			o.timeoutPendingStart = o.rejectionStart()
		}
		return time.Time{}, false
	}
	o.rejected = true
	return o.rejectionStart(), true
}

// claimPendingTimeout reports the start of a header read timeout that is still
// pending when the connection closes, and claims it so it is logged exactly
// once. net/http closed the connection without writing the 400 that would have
// replaced the record, so the connection closed without a response.
func (o *connObserver) claimPendingTimeout() (time.Time, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.timeoutPending || o.rejected || o.hijacked {
		return time.Time{}, false
	}
	o.timeoutPending = false
	o.rejected = true
	return o.timeoutPendingStart, true
}

// markHijacked records that a handler took the connection over. net/http no
// longer reads or writes it, so nothing on it is a rejection.
func (o *connObserver) markHijacked() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.hijacked = true
	o.stop()
}

// logRejected writes the http_request_rejected record of a request that net/http
// rejected before it had a request to hand to the handler. The record has no
// method, path or byte count because none of them is reliable at that point.
// status is the status net/http sent to the client, or 0 when it sent no
// response (the header read timeout).
func (o *connObserver) logRejected(reason RejectionReason, status int, start time.Time) {
	o.logger.Info("http_request_rejected",
		slog.String("reason", string(reason)),
		slog.Int("status", status),
		slog.Int64("duration_ms", o.now().Sub(start).Milliseconds()),
	)
}

// observedConn tees a connection into its connObserver. It keeps the
// net.Conn behavior net/http relies on, including CloseWrite.
type observedConn struct {
	net.Conn
	obs *connObserver
}

func (c *observedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.obs.feed(p[:n])
	}
	var netErr net.Error
	if err != nil && errors.As(err, &netErr) && netErr.Timeout() {
		// net/http closes the connection without a response after this error,
		// except that it answers a malformed partial line with a 400 first. Such a
		// timeout is left pending (see claimReadTimeout) and the 400 write claims it.
		if start, ok := c.obs.claimReadTimeout(); ok {
			c.obs.logRejected(RejectionHeaderReadTimeout, 0, start)
		}
	}
	return n, err
}

func (c *observedConn) Write(p []byte) (int, error) {
	// Log before writing, like the request logger, which records the request
	// before net/http flushes the response.
	if request, start, ok := c.obs.claimRejection(p); ok {
		c.obs.logRejection(request, start)
	} else if reason, status, start, ok := c.obs.claimDirectRejection(p); ok {
		c.obs.logRejected(reason, status, start)
	}
	return c.Conn.Write(p)
}

func (c *observedConn) SetReadDeadline(t time.Time) error {
	c.obs.setReadDeadline(t)
	return c.Conn.SetReadDeadline(t)
}

func (c *observedConn) SetDeadline(t time.Time) error {
	c.obs.setReadDeadline(t)
	return c.Conn.SetDeadline(t)
}

// CloseWrite is used by net/http to half-close a connection before it closes
// it; embedding net.Conn alone would hide it.
func (c *observedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

type observedListener struct {
	net.Listener
	logger *slog.Logger
}

// ObserveListener wraps listener so that requests that net/http rejects before
// the handler runs are logged: a 417 for an unsupported Expect header as an
// http_request record, and a malformed request (400), an oversized head (431),
// an unsupported Transfer-Encoding (501) or protocol version (505) and a
// header read timeout as http_request_rejected records. The server that serves
// the listener must set ConnContext to this package's ConnContext, ConnState to
// this package's ConnState, MaxHeaderBytes to MaxHeaderBytes and
// DisableGeneralOptionsHandler to true, and must serve NewHandler so that
// net/http does not answer a server-wide "OPTIONS *" request itself. Otherwise
// that request is not logged, a later 417 on the same connection is not logged,
// and the duration_ms of later http_request_rejected records on that connection
// is measured from the "OPTIONS *" request. It does not change any response.
func ObserveListener(listener net.Listener, logger *slog.Logger) net.Listener {
	return observedListener{Listener: listener, logger: logger}
}

func (l observedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &observedConn{Conn: conn, obs: newConnObserver(l.logger)}, nil
}

type observerKey struct{}

// ConnContext is the http.Server.ConnContext hook that hands the connection's
// observer to the request logger. Connections that ObserveListener did not wrap
// are left alone.
func ConnContext(ctx context.Context, conn net.Conn) context.Context {
	if observed, ok := conn.(*observedConn); ok {
		return context.WithValue(ctx, observerKey{}, observed.obs)
	}
	return ctx
}

// ConnState is the http.Server.ConnState hook that tells the connection's
// observer when a handler hijacks the connection, so that the observer does not
// treat the owner's own reads and writes as net/http's, and when net/http
// finished a response, so that the observer does not treat the buffered end of a
// handler's response body as a response of net/http, and when a connection
// closed, so that a header read timeout that net/http answered with no response
// is logged. Connections that ObserveListener did not wrap are left alone.
func ConnState(conn net.Conn, state http.ConnState) {
	observed, ok := conn.(*observedConn)
	if !ok {
		return
	}
	switch state {
	case http.StateHijacked:
		observed.obs.markHijacked()
	case http.StateIdle:
		observed.obs.responseFinished()
	case http.StateClosed:
		// net/http reports StateClosed with the connection that the listener
		// returned, which is the observedConn. A header read timeout inside a
		// partial line is still pending here when net/http wrote no 400.
		if start, ok := observed.obs.claimPendingTimeout(); ok {
			observed.obs.logRejected(RejectionHeaderReadTimeout, 0, start)
		}
	}
}

func observerFrom(ctx context.Context) *connObserver {
	obs, _ := ctx.Value(observerKey{}).(*connObserver)
	return obs
}
