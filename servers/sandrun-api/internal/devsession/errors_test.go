// SPDX-License-Identifier: FSL-1.1-ALv2

package devsession

import (
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand"
	"github.com/delinoio/mono/go/ownsand/validate"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient"
)

func TestOwnSandError(t *testing.T) {
	sdk, err := (ownsandclient.SDKFactory{BaseURL: "http://127.0.0.1:1"}).New(api.NewID(), "test-key")
	if err != nil {
		t.Fatal(err)
	}
	_, openErr := sdk.OpenFileWrite(api.NewID(), ownsand.FileWriteParams{WriteID: api.NewID(), Path: "relative.txt"})
	validationErr := &validate.Error{Field: "command", Rule: "is required"}
	for _, tc := range []struct {
		name    string
		err     error
		code    connect.Code
		message string
	}{
		{"validation", validationErr, connect.CodeInvalidArgument, "invalid command: is required"},
		{"wrapped validation", fmt.Errorf("wrapper detail: %w", validationErr), connect.CodeInvalidArgument, "invalid command: is required"},
		{"relative SDK write path", openErr, connect.CodeInvalidArgument, "invalid path: must be an absolute guest path"},
		{"wire invalid argument", connect.NewWireError(connect.CodeInvalidArgument, errors.New("missing command")), connect.CodeInvalidArgument, "OwnSand: missing command"},
		{"unknown", errors.New("unknown failure"), connect.CodeUnavailable, "the OwnSand data path failed; try again"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ownsandError(tc.err)
			var ce *connect.Error
			if !errors.As(got, &ce) || ce.Code() != tc.code || ce.Message() != tc.message {
				t.Fatalf("got %v, want %s: %s", got, tc.code, tc.message)
			}
		})
	}
}
