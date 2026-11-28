// SPDX-License-Identifier: Apache-2.0

// Package logging builds the bridge logger. Every record goes to a local
// JSON handler (stderr and bridge.log, without ANSI color, because programs
// read them) and through the OpenTelemetry log bridge to a process-local
// Logs SDK provider, as the repository defaults require. The bridge has no
// network connection to SandCrew, so New attaches no exporter by default;
// a caller that has one passes it as a processor. The owning contract is
// docs/cmds-sandcrew-bridge-foundation.md#logging.
package logging

import (
	"context"
	"io"
	"log/slog"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
)

// ServiceName is the OpenTelemetry service and instrumentation scope name.
const ServiceName = "sandcrew-bridge"

// shutdownTimeout bounds the flush of the Logs SDK at exit.
const shutdownTimeout = time.Second

// New returns the bridge logger that writes records of level and above to
// out and to the Logs SDK, and a function that shuts the SDK down.
func New(out io.Writer, level slog.Level, processors ...sdklog.Processor) (*slog.Logger, func()) {
	options := []sdklog.LoggerProviderOption{
		sdklog.WithResource(resource.NewSchemaless(attribute.String("service.name", ServiceName))),
	}
	for _, processor := range processors {
		options = append(options, sdklog.WithProcessor(processor))
	}
	provider := sdklog.NewLoggerProvider(options...)
	handler := slog.NewMultiHandler(
		slog.NewJSONHandler(out, &slog.HandlerOptions{Level: level}),
		&levelHandler{Handler: otelslog.NewHandler(ServiceName, otelslog.WithLoggerProvider(provider)), level: level},
	)
	return slog.New(handler), func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = provider.Shutdown(ctx)
	}
}

// levelHandler applies the bridge log level to the SDK handler, which has
// no level of its own.
type levelHandler struct {
	slog.Handler
	level slog.Level
}

func (h *levelHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.level && h.Handler.Enabled(ctx, level)
}

func (h *levelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &levelHandler{Handler: h.Handler.WithAttrs(attrs), level: h.level}
}

func (h *levelHandler) WithGroup(name string) slog.Handler {
	return &levelHandler{Handler: h.Handler.WithGroup(name), level: h.level}
}
