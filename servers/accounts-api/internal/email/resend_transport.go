// SPDX-License-Identifier: FSL-1.1-ALv2

package email

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
)

const resendPayloadConflictCode = "invalid_idempotent_request"

type resendConflictKey struct{}

type resendTransport struct {
	http.RoundTripper
}

// RoundTrip captures the exact conflict code for this send. The current
// Resend SDK discards the code on HTTP 409. Remove this wrapper once the SDK
// exposes the status and code together in a typed error.
func (t resendTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.RoundTripper.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil || resp.StatusCode != http.StatusConflict {
		return resp, err
	}
	conflict, ok := req.Context().Value(resendConflictKey{}).(*bool)
	if !ok {
		return resp, nil
	}

	// Bound the inspection and leave the body available to the SDK.
	const maxErrorBody = 4096
	body := bufio.NewReaderSize(resp.Body, maxErrorBody+1)
	resp.Body = struct {
		io.Reader
		io.Closer
	}{body, resp.Body}
	data, peekErr := body.Peek(maxErrorBody + 1)
	if peekErr == io.EOF {
		var response struct {
			Name string `json:"name"`
		}
		*conflict = json.Unmarshal(data, &response) == nil && response.Name == resendPayloadConflictCode
	}
	return resp, nil
}
