// SPDX-License-Identifier: Apache-2.0

package logto

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type pollResponseBody struct {
	io.Reader
	onClose func()
}

func (b pollResponseBody) Close() error {
	b.onClose()
	return nil
}

func TestPollTokenChecksContextAfterResponse(t *testing.T) {
	for _, response := range []struct {
		name   string
		status int
		body   string
		err    error
	}{
		{"success", http.StatusOK, `{"access_token":"access","refresh_token":"refresh"}`, nil},
		{"denied", http.StatusBadRequest, `{"error":"access_denied"}`, ErrAccessDenied},
	} {
		t.Run(response.name, func(t *testing.T) {
			for _, stop := range []struct {
				name string
				err  error
			}{
				{"active", nil},
				{"caller canceled", context.Canceled},
				{"caller deadline", context.DeadlineExceeded},
				{"device expired", ErrExpired},
				{"caller canceled at device expiry", context.Canceled},
			} {
				t.Run(stop.name, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						ctx, cancel := context.WithCancel(context.Background())
						defer cancel()
						if stop.name == "caller deadline" {
							var stopDeadline context.CancelFunc
							ctx, stopDeadline = context.WithTimeout(ctx, time.Second)
							defer stopDeadline()
						}
						da := &DeviceAuthorization{DeviceCode: "device", ExpiresIn: 2}
						requests := 0
						transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
							requests++
							return &http.Response{
								StatusCode: response.status,
								Body: pollResponseBody{
									Reader: strings.NewReader(response.body),
									onClose: func() {
										switch stop.name {
										case "caller canceled":
											cancel()
										case "caller deadline", "device expired":
											<-req.Context().Done()
										case "caller canceled at device expiry":
											<-req.Context().Done()
											cancel()
										}
									},
								},
							}, nil
						})
						cfg := Config{Endpoint: "https://issuer.example", AppID: "app", HTTPClient: &http.Client{Transport: transport}}
						tok, err := cfg.PollToken(ctx, da, time.Millisecond)
						if requests != 1 {
							t.Fatalf("made %d requests, want 1", requests)
						}
						want := stop.err
						if want == nil {
							want = response.err
						}
						if err != want {
							t.Fatalf("got %v, want %v", err, want)
						}
						if want == nil {
							if tok == nil || tok.RefreshToken != "refresh" || ctx.Err() != nil {
								t.Fatalf("got token %v and caller context %v, want active success", tok, ctx.Err())
							}
						} else if tok != nil {
							t.Fatal("a rejected poll returned a token")
						}
					})
				})
			}
		})
	}
}
