// SPDX-License-Identifier: Apache-2.0

// Package acp is the client side of the Agent Client Protocol (ACP): a
// JSON-RPC 2.0 connection over newline-delimited JSON on the stdio of the
// agent process, and the ACP messages that the bridge uses. See
// https://agentclientprotocol.com/protocol/schema.
package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// maxMessageSize limits one JSON-RPC message from the agent. Tool output
// and file contents can make single updates large.
const maxMessageSize = 64 << 20

// JSON-RPC error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("acp: JSON-RPC error %d: %s", e.Code, e.Message)
}

// ErrClosed is returned by calls when the connection is closed.
var ErrClosed = errors.New("acp: connection closed")

// Handler receives the messages that the agent sends to the client.
type Handler interface {
	// HandleNotification is called on the reader goroutine, in message
	// order. It must not block for long.
	HandleNotification(method string, params json.RawMessage)
	// HandleRequest is called on its own goroutine. It returns the result
	// or an error; an *RPCError is sent unchanged.
	HandleRequest(ctx context.Context, method string, params json.RawMessage) (any, error)
	// HandleMalformed is called for a message that is not valid JSON-RPC.
	// The connection continues.
	HandleMalformed(err error)
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type response struct {
	result json.RawMessage
	err    error
}

// outgoing is one message for the writer goroutine.
type outgoing struct {
	data   []byte
	result chan error
}

// Conn is one JSON-RPC connection.
type Conn struct {
	w       io.Writer
	handler Handler

	// out passes messages to the writer goroutine. A write to an agent
	// that stops reading blocks in the pipe; only the writer goroutine
	// waits there, and senders give up when their context ends.
	out chan outgoing

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan response
	closed  bool
	done    chan struct{}
	err     error

	ctx    context.Context
	cancel context.CancelFunc
}

// NewConn starts reading r and returns the connection. Close it, or let r
// end, to stop it.
func NewConn(r io.Reader, w io.Writer, handler Handler) *Conn {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Conn{
		w:       w,
		handler: handler,
		out:     make(chan outgoing),
		pending: make(map[string]chan response),
		done:    make(chan struct{}),
		ctx:     ctx,
		cancel:  cancel,
	}
	go c.read(r)
	go c.write()
	return c
}

// Done is closed when the reader ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err returns the reason why the reader ended, after Done is closed.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Call sends a request and waits for its response. When ctx ends first,
// also while the request waits to be written, Call returns the context
// error and drops the late response.
func (c *Conn) Call(ctx context.Context, method string, params any, result any) error {
	return c.CallSent(ctx, method, params, result, nil)
}

// CallSent is Call that calls sent once the request is written or cannot
// be written any more, before it waits for the response. A message that
// the caller sends after sent is therefore written after the request.
func (c *Conn) CallSent(ctx context.Context, method string, params any, result any, sent func()) error {
	var once sync.Once
	markSent := func() {
		if sent != nil {
			once.Do(sent)
		}
	}
	// An early return (a closed connection or a failed write) also counts
	// as sent: nothing will be written for this request.
	defer markSent()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.nextID++
	id := strconv.FormatInt(c.nextID, 10)
	ch := make(chan response, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	raw, err := json.Marshal(params)
	if err != nil {
		c.forget(id)
		return err
	}
	if err := c.send(ctx, message{JSONRPC: "2.0", ID: json.RawMessage(id), Method: method, Params: raw}); err != nil {
		c.forget(id)
		return err
	}
	markSent()
	select {
	case <-ctx.Done():
		c.forget(id)
		return ctx.Err()
	case resp := <-ch:
		if resp.err != nil {
			return resp.err
		}
		if result == nil || len(resp.result) == 0 {
			return nil
		}
		if err := json.Unmarshal(resp.result, result); err != nil {
			return fmt.Errorf("acp: decode %s result: %w", method, err)
		}
		return nil
	}
}

// Notify sends a notification. It returns the context error when ctx ends
// before the notification is written.
func (c *Conn) Notify(ctx context.Context, method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.send(ctx, message{JSONRPC: "2.0", Method: method, Params: raw})
}

// Close stops request handlers, the writer goroutine and pending calls.
// It does not close the reader or the writer; the owner of the process
// does. A write that is blocked in w ends when the owner closes w.
func (c *Conn) Close() {
	c.cancel()
	c.failPending(ErrClosed)
}

func (c *Conn) forget(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// send hands m to the writer goroutine and waits for the write. It returns
// when ctx ends or the connection closes first; a message that the writer
// already took is still written whole, so the stream stays valid.
func (c *Conn) send(ctx context.Context, m message) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	req := outgoing{data: append(data, '\n'), result: make(chan error, 1)}
	select {
	case c.out <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return ErrClosed
	}
	select {
	case err := <-req.result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// write writes the messages of send in order until the connection closes.
func (c *Conn) write() {
	for {
		select {
		case req := <-c.out:
			_, err := c.w.Write(req.data)
			req.result <- err
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Conn) failPending(err error) {
	c.mu.Lock()
	c.closed = true
	pending := c.pending
	c.pending = make(map[string]chan response)
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- response{err: err}
	}
}

func (c *Conn) read(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxMessageSize)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		c.dispatch(append([]byte(nil), line...))
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
	c.failPending(ErrClosed)
	c.cancel()
	close(c.done)
}

func (c *Conn) dispatch(line []byte) {
	var m message
	if err := json.Unmarshal(line, &m); err != nil {
		c.handler.HandleMalformed(fmt.Errorf("acp: parse message: %w", err))
		return
	}
	if m.JSONRPC != "2.0" {
		c.handler.HandleMalformed(errors.New("acp: message without jsonrpc 2.0"))
		return
	}
	hasID := len(m.ID) > 0 && string(m.ID) != "null"
	switch {
	case m.Method != "" && hasID:
		go c.serve(m)
	case m.Method != "":
		c.handler.HandleNotification(m.Method, m.Params)
	case hasID:
		c.resolve(m)
	default:
		c.handler.HandleMalformed(errors.New("acp: message without method or id"))
	}
}

func (c *Conn) resolve(m message) {
	id := string(m.ID)
	// The bridge sends numeric IDs; accept an agent that echoes them as
	// strings.
	if unquoted, err := strconv.Unquote(id); err == nil {
		id = unquoted
	}
	c.mu.Lock()
	ch, ok := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if !ok {
		c.handler.HandleMalformed(fmt.Errorf("acp: response to unknown request %s", id))
		return
	}
	if m.Error != nil {
		ch <- response{err: m.Error}
		return
	}
	ch <- response{result: m.Result}
}

func (c *Conn) serve(m message) {
	result, err := c.handler.HandleRequest(c.ctx, m.Method, m.Params)
	reply := message{JSONRPC: "2.0", ID: m.ID}
	if err != nil {
		var rpcErr *RPCError
		if !errors.As(err, &rpcErr) {
			rpcErr = &RPCError{Code: CodeInternalError, Message: err.Error()}
		}
		reply.Error = rpcErr
	} else {
		raw, merr := json.Marshal(result)
		if merr != nil {
			reply.Error = &RPCError{Code: CodeInternalError, Message: "encode result"}
		} else {
			reply.Result = raw
		}
	}
	// A failed write means that the agent is gone; its exit ends the
	// session.
	_ = c.send(c.ctx, reply)
}
