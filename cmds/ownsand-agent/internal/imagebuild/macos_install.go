// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

type macOSInstallation interface {
	completion() <-chan error
	cancel()
	fractionCompleted() float64
	close(stopVM bool) error
}

// A successful start must register a completion callback. After that point,
// the installer owns the staged files until the callback fires.
func runMacOSInstall(ctx context.Context, start func(context.Context) (macOSInstallation, error), logger *slog.Logger) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	startedAt := time.Now()
	installation, err := start(ctx)
	if err != nil {
		return err
	}
	stopVM := false
	defer func() {
		if closeErr := installation.close(stopVM); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	logger.Info("installing macOS")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	completion := installation.completion()
	canceled := ctx.Done()
	for {
		select {
		case <-canceled:
			logger.Info("canceling macOS installation and waiting for completion")
			installation.cancel()
			canceled = nil
		case err := <-completion:
			stopVM = err == nil
			logger.Info("macOS installer completed", "install_ms", time.Since(startedAt).Milliseconds())
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		case <-ticker.C:
			logger.Info("macOS installation", "fraction_completed", installation.fractionCompleted())
		}
	}
}
