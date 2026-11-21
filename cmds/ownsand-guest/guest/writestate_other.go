// SPDX-License-Identifier: Apache-2.0

//go:build !unix && !windows

package guest

import (
	"errors"
	"os"
)

// fileIdentity is not implemented on this platform. Without an identity the
// helper keeps no write records: staging files of a helper process that
// exited stay until somebody removes them. Implement it with the first guest
// image for this platform.
type fileIdentity struct {
	Device uint64 `json:"device"`
	Index  uint64 `json:"index"`
}

func identityOfFile(*os.File) (fileIdentity, error) { return fileIdentity{}, errors.ErrUnsupported }

func identityOfPath(string) (fileIdentity, error) { return fileIdentity{}, errors.ErrUnsupported }
