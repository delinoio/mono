// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !unix && !windows

package guest

import (
	"errors"
	"os"
)

// fileIdentity is not implemented on this platform. An identity is required
// to create a recoverable staging file. Implement it with the first guest
// image for this platform.
type fileIdentity struct {
	Device uint64 `json:"device"`
	Index  uint64 `json:"index"`
}

func identityOfFile(*os.File) (fileIdentity, error) { return fileIdentity{}, errors.ErrUnsupported }

func identityOfPath(string) (fileIdentity, error) { return fileIdentity{}, errors.ErrUnsupported }
