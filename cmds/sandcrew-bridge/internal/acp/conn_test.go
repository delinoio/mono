// SPDX-License-Identifier: Apache-2.0

package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingHandler struct {
	mu            sync.Mutex
	notifications []string
	malformed     []error
}

func (h *recordingHandler) HandleNotification(method string, _ json.RawMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notifications = append(h.notifications, method)
}

func (h *recordingHandler) HandleRequest(_ context.Context, method string, _ json.RawMessage) (any, error) {
	if method == "fs/read_text_file" {
		return nil, &RPCError{Code: CodeMethodNotFound, Message: "method not found"}
	}
	return map[string]string{"echo": method}, nil
}

func (h *recordingHandler) HandleMalformed(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.malformed = append(h.malformed, err)
}

func TestConn(t *testing.T) {
	agentOut, connIn := io.Pipe()
	connOut, agentIn := io.Pipe()
	handler := &recordingHandler{}
	conn := NewConn(agentOut, agentIn, handler)
	fromConn := bufio.NewScanner(connOut)
	send := func(line string) {
		t.Helper()
		if _, err := connIn.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	read := func() map[string]any {
		t.Helper()
		if !fromConn.Scan() {
			t.Fatal("no message from the connection")
		}
		var m map[string]any
		if err := json.Unmarshal(fromConn.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	// A call and its response, with a string ID echo.
	result := make(chan error, 1)
	var got struct{ OK bool }
	go func() { result <- conn.Call(context.Background(), "session/new", map[string]any{}, &got) }()
	request := read()
	if request["method"] != "session/new" || request["jsonrpc"] != "2.0" {
		t.Fatalf("request %v", request)
	}
	send(`{"jsonrpc":"2.0","id":"` + strings.TrimSpace(jsonText(request["id"])) + `","result":{"OK":true}}`)
	if err := <-result; err != nil || !got.OK {
		t.Fatalf("call: %v %v", err, got)
	}

	// An error response.
	go func() { result <- conn.Call(context.Background(), "session/set_mode", map[string]any{}, nil) }()
	request = read()
	send(`{"jsonrpc":"2.0","id":` + jsonText(request["id"]) + `,"error":{"code":-32602,"message":"bad"}}`)
	var rpcErr *RPCError
	if err := <-result; !errors.As(err, &rpcErr) || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("error %v", err)
	}

	// Requests of the agent are answered; unsupported ones with an error.
	send(`{"jsonrpc":"2.0","id":7,"method":"fs/read_text_file","params":{}}`)
	if reply := read(); reply["error"].(map[string]any)["code"] != float64(CodeMethodNotFound) || reply["id"] != float64(7) {
		t.Fatalf("reply %v", reply)
	}
	send(`{"jsonrpc":"2.0","id":"x","method":"session/request_permission","params":{}}`)
	if reply := read(); reply["result"].(map[string]any)["echo"] != "session/request_permission" {
		t.Fatalf("reply %v", reply)
	}

	// Notifications, malformed lines and unknown responses.
	send(`{"jsonrpc":"2.0","method":"session/update","params":{}}`)
	send(`not json`)
	send(`{"jsonrpc":"1.0","method":"x"}`)
	send(`{"jsonrpc":"2.0","id":999,"result":{}}`)
	deadline := time.Now().Add(5 * time.Second)
	for {
		handler.mu.Lock()
		done := len(handler.notifications) == 1 && len(handler.malformed) == 3
		handler.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("notifications %v, malformed %v", handler.notifications, handler.malformed)
		}
		time.Sleep(time.Millisecond)
	}

	// A closed reader fails pending and later calls.
	go func() { result <- conn.Call(context.Background(), "session/prompt", map[string]any{}, nil) }()
	read()
	connIn.Close()
	if err := <-result; !errors.Is(err, ErrClosed) {
		t.Fatalf("pending call after close: %v", err)
	}
	<-conn.Done()
	if err := conn.Call(context.Background(), "x", nil, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("call after close: %v", err)
	}
}

func jsonText(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}
