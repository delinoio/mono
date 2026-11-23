package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// jwksStartTimeout bounds every wait on the JWKS fixture. It is a failure
// deadline, not a pacing delay: a passing run never waits for it.
const jwksStartTimeout = 10 * time.Second

// waitForJWKSStart waits until the JWKS handler reports its first request,
// the validation finishes early, or the deadline passes. An early result or a
// missing signal is returned as a local diagnostic instead of hanging.
func waitForJWKSStart(started <-chan struct{}, done <-chan error, deadline time.Duration) error {
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	select {
	case <-started:
		return nil
	case err := <-done:
		return fmt.Errorf("validation returned before the JWKS request started (err=%w)", wrapNil(err))
	case <-timer.C:
		return errors.New("JWKS handler never received a request before the test deadline")
	}
}

// wrapNil lets %w carry a nil validation result without a nil-wrap panic.
func wrapNil(err error) error {
	if err == nil {
		return errors.New("<nil>")
	}
	return err
}

func TestWaitForJWKSStartReportsEarlyCompletion(t *testing.T) {
	started := make(chan struct{}, 1)
	done := make(chan error, 1)
	done <- ErrInvalidToken
	err := waitForJWKSStart(started, done, jwksStartTimeout)
	if err == nil || !errors.Is(err, ErrInvalidToken) || !strings.Contains(err.Error(), "before the JWKS request started") {
		t.Fatalf("early completion diagnostic = %v", err)
	}
}

func TestWaitForJWKSStartReportsMissingSignal(t *testing.T) {
	started := make(chan struct{}, 1)
	done := make(chan error, 1)
	err := waitForJWKSStart(started, done, 0)
	if err == nil || !strings.Contains(err.Error(), "never received a request") {
		t.Fatalf("missing signal diagnostic = %v", err)
	}
}

func TestWaitForJWKSStartReturnsOnSignal(t *testing.T) {
	started := make(chan struct{}, 1)
	started <- struct{}{}
	if err := waitForJWKSStart(started, make(chan error, 1), jwksStartTimeout); err != nil {
		t.Fatalf("signaled start returned %v", err)
	}
}

// An unsupported token type fails before any key lookup, so a real validator
// finishes with zero handler requests and the wait must surface that result.
func TestLogtoValidatorEarlyCompletionIsReportedBeforeJWKSStart(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	o := &jwksOutage{
		f:       tokenFactory{key: key, kid: "k1", issuer: "https://tenant.logto.app/oidc"},
		block:   make(chan struct{}),
		started: make(chan struct{}, 1),
	}
	srv := httptest.NewServer(o)
	t.Cleanup(srv.Close)
	now := time.Now()
	v := NewLogtoValidator(LogtoConfig{Issuer: o.f.issuer, Audience: "https://ownsand.api.delino.io", JWKSURL: srv.URL, Now: func() time.Time { return now }})
	token := o.f.sign(t, "JWT", outageClaims(o.f, now))
	done := make(chan error, 1)
	go func() {
		_, err := v.Validate(t.Context(), token)
		done <- err
	}()
	err := waitForJWKSStart(o.started, done, jwksStartTimeout)
	if err == nil || !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("early completion diagnostic = %v", err)
	}
	if got := o.fetches.Load(); got != 0 {
		t.Fatalf("fetches = %d, want 0", got)
	}
}
