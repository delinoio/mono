// SPDX-License-Identifier: Apache-2.0

package logto

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

const maxDeviceTimingSeconds int64 = 9_223_372_036

func TestStartDeviceAuthorizationTimingBounds(t *testing.T) {
	for _, tc := range []struct {
		name      string
		expiresIn string
		interval  string
		wantField string
	}{
		{"largest whole-second durations", "9223372036", "9223372036", ""},
		{"lifetime exceeds duration", "9223372037", "5", "expires_in"},
		{"interval exceeds duration", "60", "9223372037", "interval"},
		{"lifetime exceeds JSON integer storage", "9223372036854775808", "5", "expires_in"},
		{"interval exceeds JSON integer storage", "60", "9223372036854775808", "interval"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wantField == "" && strconv.IntSize < 64 {
				t.Skip("the maximum whole-second duration requires a 64-bit int")
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"device_code":"device-marker","user_code":"user-marker","verification_uri":"https://issuer.example/verify","expires_in":%s,"interval":%s}`, tc.expiresIn, tc.interval)
			}))
			defer srv.Close()
			cfg := Config{Endpoint: srv.URL, AppID: "app"}
			before := time.Now()
			da, err := cfg.StartDeviceAuthorization(context.Background())
			after := time.Now()
			if tc.wantField != "" {
				wantError := "logto: invalid device authorization response: " + tc.wantField + " must be an integer in the supported duration range"
				if da != nil || err == nil || err.Error() != wantError {
					t.Fatalf("got (%v, %v), want nil authorization and %q", da, err, wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			lifetime := time.Duration(maxDeviceTimingSeconds) * time.Second
			if da.ExpiresAt.Before(before.Add(lifetime)) || da.ExpiresAt.After(after.Add(lifetime)) {
				t.Fatalf("ExpiresAt %v, want between %v and %v", da.ExpiresAt, before.Add(lifetime), after.Add(lifetime))
			}
		})
	}
}

func TestPollTokenRejectsOverflowingTimingWithoutRequest(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("an overflowing duration in whole seconds requires a 64-bit int")
	}
	overflowSeconds := maxDeviceTimingSeconds + 1
	cfg := Config{Endpoint: "https://issuer.example", AppID: "app", HTTPClient: &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Error("sent a token request for invalid timing")
			return nil, fmt.Errorf("unexpected request to %s", req.URL.Path)
		}),
	}}
	for field, da := range map[string]DeviceAuthorization{
		"expires_in": {DeviceCode: "device-marker", ExpiresIn: int(overflowSeconds), Interval: 1},
		"interval":   {DeviceCode: "device-marker", ExpiresIn: 60, Interval: int(overflowSeconds)},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		token, err := cfg.PollToken(ctx, &da, time.Millisecond)
		cancel()
		wantError := "logto: invalid device authorization response: " + field + " must be an integer in the supported duration range"
		if token != nil || err == nil || err.Error() != wantError {
			t.Errorf("got (%v, %v), want nil token and %q", token, err, wantError)
		}
	}
}

func TestPollTokenPreservesIntervalAndExplicitDeadline(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval int
		wait     time.Duration
	}{
		{"zero interval", 0, 5 * time.Second},
		{"negative interval", -1, 5 * time.Second},
		{"positive interval", 6, 6 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				da := &DeviceAuthorization{
					DeviceCode: "device",
					ExpiresIn:  1,
					Interval:   tc.interval,
					ExpiresAt:  start.Add(10 * time.Second),
				}
				cfg := Config{Endpoint: "https://issuer.example", AppID: "app", HTTPClient: &http.Client{
					Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
						if elapsed := time.Since(start); elapsed != tc.wait {
							t.Errorf("sent a token request after %v, want %v", elapsed, tc.wait)
						}
						return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"refresh_token":"refresh"}`))}, nil
					}),
				}}
				token, err := cfg.PollToken(context.Background(), da, 0)
				if err != nil || token == nil || token.RefreshToken != "refresh" {
					t.Fatalf("got (%v, %v), want the token before the explicit deadline", token, err)
				}
			})
		})
	}
}
