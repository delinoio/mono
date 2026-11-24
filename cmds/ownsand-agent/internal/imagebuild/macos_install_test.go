// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"testing/synctest"
)

type fakeMacOSInstallation struct {
	done             chan error
	acceptsCancel    bool
	cancelRequests   int
	progressCanceled bool
	closed           bool
	stopVM           bool
}

func (i *fakeMacOSInstallation) completion() <-chan error   { return i.done }
func (i *fakeMacOSInstallation) fractionCompleted() float64 { return 0.5 }

func (i *fakeMacOSInstallation) cancel() {
	i.cancelRequests++
	i.progressCanceled = i.acceptsCancel
}

func (i *fakeMacOSInstallation) close(stopVM bool) {
	i.closed = true
	i.stopVM = stopVM
}

func TestMacOSInstallCanceledBeforeStart(t *testing.T) {
	t.Run("before setup", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := runMacOSInstall(ctx, func(context.Context) (macOSInstallation, error) {
			t.Fatal("an already canceled installation started")
			return nil, nil
		}, slog.New(slog.DiscardHandler))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("installation error = %v, want context.Canceled", err)
		}
	})
	t.Run("during setup", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		err := runMacOSInstall(ctx, func(ctx context.Context) (macOSInstallation, error) {
			cancel()
			return nil, ctx.Err()
		}, slog.New(slog.DiscardHandler))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("installation error = %v, want context.Canceled", err)
		}
	})
}

func TestMacOSInstallCancellationWaitsForCompletion(t *testing.T) {
	for _, tc := range []struct {
		name          string
		acceptsCancel bool
	}{
		{name: "cancellable", acceptsCancel: true},
		{name: "uncancellable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				installation := &fakeMacOSInstallation{
					done:          make(chan error, 1),
					acceptsCancel: tc.acceptsCancel,
				}
				result := make(chan error, 1)
				go func() {
					result <- runMacOSInstall(ctx, func(context.Context) (macOSInstallation, error) {
						return installation, nil
					}, slog.New(slog.DiscardHandler))
				}()
				synctest.Wait()
				cancel()
				synctest.Wait()
				if installation.cancelRequests != 1 || installation.progressCanceled != tc.acceptsCancel {
					t.Errorf("cancellation requests = %d, progress canceled = %t", installation.cancelRequests, installation.progressCanceled)
				}
				if installation.closed || len(result) != 0 {
					t.Error("installation returned or closed before the callback")
				}
				var nativeErr error
				if tc.acceptsCancel {
					nativeErr = errors.New("native installation canceled")
				}
				installation.done <- nativeErr
				if err := <-result; !errors.Is(err, context.Canceled) {
					t.Errorf("installation error = %v, want context.Canceled", err)
				}
				if !installation.closed || installation.stopVM != (nativeErr == nil) {
					t.Errorf("closed = %t, stop VM = %t", installation.closed, installation.stopVM)
				}
			})
		})
	}
}

func TestMacOSInstallReturnsNativeResult(t *testing.T) {
	nativeErr := errors.New("restore failed")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "success"},
		{name: "native error", err: nativeErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installation := &fakeMacOSInstallation{done: make(chan error, 1)}
			installation.done <- tc.err
			err := runMacOSInstall(t.Context(), func(context.Context) (macOSInstallation, error) {
				return installation, nil
			}, slog.New(slog.DiscardHandler))
			if !errors.Is(err, tc.err) {
				t.Errorf("installation error = %v, want %v", err, tc.err)
			}
			if !installation.closed || installation.stopVM != (tc.err == nil) || installation.cancelRequests != 0 {
				t.Errorf("closed = %t, stop VM = %t, cancellation requests = %d",
					installation.closed, installation.stopVM, installation.cancelRequests)
			}
		})
	}
}
