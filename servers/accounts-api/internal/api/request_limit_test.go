// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/protos/delino/accounts/v1/accountsv1connect"
	"github.com/delinoio/mono/servers/accounts-api/internal/auth"
	"github.com/delinoio/mono/servers/accounts-api/internal/billing"
	"github.com/delinoio/mono/servers/accounts-api/internal/testenv"
)

const requestLimit = 1 << 20

// procedures returns the Connect procedure path of every RPC of every
// accounts-api service, read from the registered protobuf descriptors so that
// a new RPC is covered without a test change.
func procedures(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, name := range []protoreflect.FullName{
		"delino.accounts.v1.BillingAccountService",
		"delino.accounts.v1.CreditService",
		"delino.accounts.v1.BillingUsageService",
		"delino.accounts.v1.UsageService",
	} {
		desc, err := protoregistry.GlobalFiles.FindDescriptorByName(name)
		if err != nil {
			t.Fatalf("find %s: %v", name, err)
		}
		methods := desc.(protoreflect.ServiceDescriptor).Methods()
		for i := 0; i < methods.Len(); i++ {
			out = append(out, fmt.Sprintf("/%s/%s", name, methods.Get(i).Name()))
		}
	}
	if len(out) < 30 {
		t.Fatalf("found %d procedures, want every RPC of the four services", len(out))
	}
	return out
}

// bearerInterceptor adds an Authorization header to every request.
func bearerInterceptor(token string) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	})
}

func gzipped(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// post sends body to a procedure without an Authorization header and returns
// the HTTP status and the response body. A negative contentLength sends the
// body chunked.
func post(t *testing.T, env *testenv.Env, procedure, contentType, encoding string, body []byte, contentLength int64) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, env.URL+procedure, io.NopCloser(bytes.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = contentLength
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Connect-Protocol-Version", "1")
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", procedure, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response of %s: %v", procedure, err)
	}
	return resp.StatusCode, string(data)
}

func wantResourceExhausted(t *testing.T, label string, status int, body string) {
	t.Helper()
	if status != http.StatusTooManyRequests || !strings.Contains(body, `"code":"resource_exhausted"`) {
		t.Errorf("%s: status %d body %q, want 429 resource_exhausted", label, status, body)
	}
}

// TestRequestSizeLimit checks that every procedure rejects an oversized
// request, also a compressed one that inflates over the limit, with
// resource_exhausted before the authentication interceptor sees it.
func TestRequestSizeLimit(t *testing.T) {
	env := testenv.New(t)
	oversized := make([]byte, 2*requestLimit)
	// 4 MiB of empty `reports` entries: field 1, length 0.
	emptyReports := bytes.Repeat([]byte{0x0a, 0x00}, 2*requestLimit)

	for _, procedure := range procedures(t) {
		status, body := post(t, env, procedure, "application/proto", "", oversized, int64(len(oversized)))
		wantResourceExhausted(t, procedure+" raw", status, body)
		status, body = post(t, env, procedure, "application/proto", "", oversized, -1)
		wantResourceExhausted(t, procedure+" chunked", status, body)
		status, body = post(t, env, procedure, "application/proto", "", emptyReports, int64(len(emptyReports)))
		wantResourceExhausted(t, procedure+" empty reports", status, body)
		status, body = post(t, env, procedure, "application/json", "", append([]byte(`{"x":"`), append(oversized, '"', '}')...), -1)
		wantResourceExhausted(t, procedure+" json", status, body)
		// A small compressed body that inflates to 4 MiB.
		bomb := gzipped(t, emptyReports)
		if len(bomb) > requestLimit/8 {
			t.Fatalf("test body: bomb has %d bytes, want a small compressed body", len(bomb))
		}
		status, body = post(t, env, procedure, "application/proto", "gzip", bomb, int64(len(bomb)))
		wantResourceExhausted(t, procedure+" gzip bomb", status, body)
	}

	// The requests were rejected before authentication: no credential check
	// ran, so nothing was logged as an authentication rejection.
	if logs := env.Logs.String(); strings.Contains(logs, "auth_rejected") {
		t.Errorf("an oversized request reached the authentication interceptor:\n%s", logs)
	}
}

// TestRequestSizeLimitKeepsSmallRequests checks that the limit leaves small
// requests alone: an unauthenticated one still fails with unauthenticated.
func TestRequestSizeLimitKeepsSmallRequests(t *testing.T) {
	env := testenv.New(t)
	small, err := proto.Marshal(&accountsv1.ListBillingStatusChangesRequest{Service: ownsand})
	if err != nil {
		t.Fatal(err)
	}
	status, body := post(t, env, "/delino.accounts.v1.UsageService/ListBillingStatusChanges", "application/proto", "", small, int64(len(small)))
	if status != http.StatusUnauthorized || !strings.Contains(body, `"code":"unauthenticated"`) {
		t.Errorf("small unauthenticated request: status %d body %q, want 401 unauthenticated", status, body)
	}
	// A request just under the limit passes the size check and is rejected by
	// the authentication interceptor instead.
	nearLimit := bytes.Repeat([]byte{0x0a, 0x00}, (requestLimit-1024)/2)
	status, body = post(t, env, "/delino.accounts.v1.UsageService/ReportUsage", "application/proto", "", nearLimit, int64(len(nearLimit)))
	if status != http.StatusUnauthorized || !strings.Contains(body, `"code":"unauthenticated"`) {
		t.Errorf("request under the limit: status %d body %q, want 401 unauthenticated", status, body)
	}
}

// TestReportUsageMaximumRequest checks that the largest valid ReportUsage
// request (500 reports, 128-byte organization IDs, large int64 values) is
// far under the limit and still succeeds over the binary and JSON encodings.
func TestReportUsageMaximumRequest(t *testing.T) {
	env := testenv.New(t)
	token := env.Issuer.MachineToken(t, testenv.UsageClientID, auth.ScopeReportUsage)
	start := window(env.Clock.Now()).Add(-billing.WindowLength)
	build := func(prefix string) *accountsv1.ReportUsageRequest {
		reports := make([]*accountsv1.UsageReport, 500)
		for i := range reports {
			org := fmt.Sprintf("%s-%03d-", prefix, i)
			org += strings.Repeat("o", 128-len(org))
			reports[i] = report(org, meterEgress, start, math.MaxInt64-int64(i), math.MaxInt64-int64(i))
		}
		return &accountsv1.ReportUsageRequest{Reports: reports}
	}
	// The largest request is about 81 KiB in binary and 163 KiB in protojson.
	jsonBytes, err := protojson.Marshal(build("j"))
	if err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int{"binary": proto.Size(build("b")), "protojson": len(jsonBytes)} {
		if size < 64<<10 || size > requestLimit/4 {
			t.Errorf("%s request size = %d bytes, want far under the %d byte limit", name, size, requestLimit)
		}
	}

	for _, tc := range []struct {
		name string
		opts []connect.ClientOption
	}{
		{"binary", nil},
		{"protojson", []connect.ClientOption{connect.WithProtoJSON()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := accountsv1connect.NewUsageServiceClient(http.DefaultClient, env.URL, append(tc.opts,
				connect.WithInterceptors(bearerInterceptor(token)))...)
			resp, err := client.ReportUsage(ctx(), connect.NewRequest(build(tc.name)))
			if err != nil {
				t.Fatalf("ReportUsage with 500 reports: %v", err)
			}
			if got := len(resp.Msg.GetResults()); got != 500 {
				t.Errorf("results = %d, want 500", got)
			}
		})
	}
}

// TestRequestSizeLimitKeepsWebhookLimit checks that the Paddle webhook keeps
// its own 1 MiB body limit, which answers 413 and not a Connect error.
func TestRequestSizeLimitKeepsWebhookLimit(t *testing.T) {
	env := testenv.New(t)
	if status := env.PostWebhook(make([]byte, requestLimit+1), ""); status != http.StatusRequestEntityTooLarge {
		t.Errorf("webhook over 1 MiB: status %d, want 413", status)
	}
}
