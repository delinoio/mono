// SPDX-License-Identifier: FSL-1.1-ALv2

package agent

import (
	"context"
	"errors"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/r2"
	"github.com/delinoio/mono/servers/ownsand-api/internal/store"
)

// Signed URL limits.
const (
	SignedURLLifetime  = 15 * time.Minute
	MaxPartsPerRequest = 100
	MaxPartNumber      = 10000
)

// R2SignedURLs issues part URLs with R2 presigning.
type R2SignedURLs struct {
	Store *store.Store
	R2    *r2.Client
}

// Grant implements SignedURLs. Upload URLs go only to the device that runs
// the export; download URLs only to a device of the organization with the
// snapshot's OS and architecture.
func (s *R2SignedURLs) Grant(ctx context.Context, deviceID string, req *ownsandv1.SignedUrlRequest) *ownsandv1.SignedUrlGrant {
	g := &ownsandv1.SignedUrlGrant{RequestId: req.GetRequestId(), SnapshotId: req.GetSnapshotId()}
	fail := func(code connect.Code) *ownsandv1.SignedUrlGrant {
		g.ErrorCode = code.String()
		g.Urls = nil
		return g
	}
	parts := req.GetPartNumbers()
	if len(parts) == 0 || len(parts) > MaxPartsPerRequest {
		return fail(connect.CodeInvalidArgument)
	}
	for _, p := range parts {
		if p < 1 || p > MaxPartNumber {
			return fail(connect.CodeInvalidArgument)
		}
	}
	purpose, method := store.SignedURLDownload, http.MethodGet
	if req.GetUpload() {
		purpose, method = store.SignedURLUpload, http.MethodPut
	}
	exp := time.Now().Add(SignedURLLifetime)
	org, err := s.Store.AuthorizeSignedURLs(ctx, deviceID, req.GetSnapshotId(), purpose, exp)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fail(connect.CodeNotFound)
	case errors.Is(err, store.ErrConflict):
		return fail(connect.CodeFailedPrecondition)
	case err != nil:
		return fail(connect.CodeInternal)
	}
	prefix := store.SnapshotPrefix(org, req.GetSnapshotId())
	g.Urls = map[int32]string{}
	for _, p := range parts {
		u, err := s.R2.Presign(method, r2.PartKey(prefix, p), SignedURLLifetime)
		if err != nil {
			return fail(connect.CodeInternal)
		}
		g.Urls[p] = u
	}
	g.ExpireTime = timestamppb.New(exp)
	return g
}
