// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// opError is an operation error that becomes an ERROR frame. Its message
// never contains sandbox content such as paths or file data.
type opError struct {
	code ownsandv1.ErrorCode
	msg  string
}

func (e *opError) Error() string { return e.msg }

func errorf(code ownsandv1.ErrorCode, format string, args ...any) *opError {
	return &opError{code: code, msg: fmt.Sprintf(format, args...)}
}

// detailOf turns err into the ErrorDetail of an ERROR frame.
func detailOf(err error) *ownsandv1.ErrorDetail {
	var op *opError
	if errors.As(err, &op) {
		return &ownsandv1.ErrorDetail{Code: op.code, Message: op.msg}
	}
	return &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, Message: "internal guest helper error"}
}

// fsError maps a file system error to an operation error without naming the
// path.
func fsError(what string, err error) *opError {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return errorf(ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "%s: file does not exist", what)
	case errors.Is(err, fs.ErrPermission):
		return errorf(ownsandv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "%s: permission denied", what)
	case isDiskFull(err):
		return errorf(ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED, "%s: no space left on the guest disk", what)
	case errors.Is(err, syscall.EISDIR), errors.Is(err, syscall.ENOTDIR):
		return errorf(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "%s: path is a directory or has a non-directory parent", what)
	}
	return errorf(ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "%s: file system error", what)
}
