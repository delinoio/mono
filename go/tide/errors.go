// SPDX-License-Identifier: Apache-2.0

package tide

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"connectrpc.com/connect"
)

// PathSize is a local or guest path with its size in bytes.
type PathSize struct {
	Path string
	Size int64
}

// LimitError reports a sync or download that breaks a transfer limit. The
// SDK returns it before the transfer starts, or during a download as soon as
// an archive entry breaks a limit. It is wrapped in a *connect.Error with
// the code failed_precondition, so connect.CodeOf works on it.
type LimitError struct {
	// Operation is "sync" or "download".
	Operation string
	// Paths lists the paths and sizes that break the limit.
	Paths []PathSize
	// FileLimit and TotalLimit are the limits in bytes. Total is the sum
	// of the transfer when the total limit is broken, otherwise zero.
	FileLimit  int64
	TotalLimit int64
	Total      int64
}

func (e *LimitError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s limit exceeded (at most %s per file and %s in total)", e.Operation, formatBytes(e.FileLimit), formatBytes(e.TotalLimit))
	if e.Total > 0 {
		fmt.Fprintf(&b, "; the transfer is %s", formatBytes(e.Total))
	}
	for i, p := range e.Paths {
		if i == 0 {
			b.WriteString(": ")
		} else {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s (%s)", p.Path, formatBytes(p.Size))
	}
	return b.String()
}

// PathRule names a path rule of sync.
type PathRule int

const (
	// PathRuleCaseCollision: two local paths differ only in case, and the
	// guest file system ignores case (Windows and macOS guests).
	PathRuleCaseCollision PathRule = iota + 1
	// PathRuleInvalidWindowsName: the path is not valid on a Windows guest
	// (a reserved name, a reserved character, or a trailing space or dot).
	PathRuleInvalidWindowsName
	// PathRuleInvalidUTF8: a local path contains invalid UTF-8.
	PathRuleInvalidUTF8
)

// String returns the stable identifier of r.
func (r PathRule) String() string {
	switch r {
	case PathRuleCaseCollision:
		return "case_collision"
	case PathRuleInvalidWindowsName:
		return "invalid_windows_name"
	case PathRuleInvalidUTF8:
		return "invalid_utf8"
	}
	return fmt.Sprintf("PathRule(%d)", int(r))
}

// PathRuleError reports local paths that break a path rule of sync. The SDK
// returns it before it takes the sync lock or uploads anything, wrapped in a
// *connect.Error with the code failed_precondition.
type PathRuleError struct {
	Rule PathRule
	// Paths lists the paths. For a case collision, each group lists the
	// paths that differ only in case. For other rules, each group has one path.
	Paths [][]string
}

func (e *PathRuleError) Error() string {
	parts := make([]string, 0, len(e.Paths))
	for _, g := range e.Paths {
		if e.Rule == PathRuleInvalidUTF8 {
			for _, path := range g {
				parts = append(parts, fmt.Sprintf("%q", path))
			}
			continue
		}
		parts = append(parts, strings.Join(g, " and "))
	}
	switch e.Rule {
	case PathRuleCaseCollision:
		return "paths differ only in case, and the guest file system ignores case: " + strings.Join(parts, "; ")
	case PathRuleInvalidWindowsName:
		return "paths are not valid on a Windows guest: " + strings.Join(parts, ", ")
	case PathRuleInvalidUTF8:
		return "paths are not valid UTF-8: " + strings.Join(parts, ", ")
	}
	return "paths break a sync rule: " + strings.Join(parts, "; ")
}

// ErrSyncLocked reports a sync lock that another sync holds or that a
// stopped sync left. It is wrapped in a *connect.Error with the code
// failed_precondition.
var ErrSyncLocked = errors.New("another sync runs in this dev session (the sync lock exists); if no sync runs, sync again with force to remove the lock that a stopped sync left")

// ErrNotGitWorkTree reports a local sync directory that is not in a Git
// working tree. It is wrapped in a *connect.Error with the code
// failed_precondition.
var ErrNotGitWorkTree = errors.New("the local directory is not in a Git working tree")

// localError wraps err in a *connect.Error with code, so that callers can
// read the Connect code of SDK-side failures the same way as server errors.
// errors.As still finds err.
func localError(code connect.Code, err error) error {
	return connect.NewError(code, err)
}

// transient reports whether err is a failure that a retry can fix: a
// network or transport interruption without a server status, or an explicit
// unavailable status. A cancelled caller is never transient.
func transient(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	if connect.IsWireError(err) {
		return connect.CodeOf(err) == connect.CodeUnavailable
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		switch ce.Code() {
		case connect.CodeUnavailable, connect.CodeUnknown, connect.CodeInternal, connect.CodeDeadlineExceeded, connect.CodeCanceled:
			return true
		}
		return false
	}
	return true
}

// backoff doubles a delay from min to max with up to 20% jitter below it.
// It is not safe for concurrent use.
type backoff struct {
	min, max time.Duration
	cur      time.Duration
}

func (b *backoff) next() time.Duration {
	if b.cur == 0 {
		b.cur = b.min
	} else {
		b.cur = min(b.cur*2, b.max)
	}
	if b.cur <= 0 {
		return 0
	}
	return b.cur - time.Duration(rand.Int64N(int64(b.cur)/5+1))
}

func (b *backoff) reset() { b.cur = 0 }

// sleep waits d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// formatBytes formats n with a binary unit.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	s := fmt.Sprintf("%.1f", v)
	s = strings.TrimSuffix(s, ".0")
	return s + " " + string("KMGTPE"[exp]) + "iB"
}
