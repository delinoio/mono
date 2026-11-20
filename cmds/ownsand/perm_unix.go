//go:build !windows

package main

import "errors"

// errChmodUnsupported is never returned on Unix; Chmod works there.
var errChmodUnsupported = errors.New("chmod unsupported")

// restrictToUser is a no-op on Unix: the file mode 0600 and the directory
// mode 0700 already restrict access to the user.
func restrictToUser(string, bool) error { return nil }
