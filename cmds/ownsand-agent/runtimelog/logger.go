// SPDX-License-Identifier: FSL-1.1-ALv2

// Package runtimelog bridges reviewed OwnSand runtime events to the Logs SDK.
package runtimelog

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

// Component identifies the OwnSand process that owns the runtime logger.
type Component string

const (
	Agent Component = "ownsand-agent"
	Guest Component = "ownsand-guest"
)

// New preserves the local JSON sink and attaches a process-local Logs SDK
// provider. Calling it without a processor enables no export.
func New(out io.Writer, level slog.Level, component Component, processors ...sdklog.Processor) (*slog.Logger, func()) {
	componentAttr := slog.String("component", string(component))
	local := slog.NewJSONHandler(out, &slog.HandlerOptions{Level: level})
	res := resource.NewSchemaless(attribute.String("service.name", string(component)))
	options := []sdklog.LoggerProviderOption{sdklog.WithResource(res)}
	for _, processor := range processors {
		options = append(options, sdklog.WithProcessor(runtimeProcessor{Processor: processor, resource: res}))
	}
	provider := sdklog.NewLoggerProvider(options...)
	sdk := otelslog.NewHandler(string(component), otelslog.WithLoggerProvider(provider))
	handler := slog.NewMultiHandler(
		local.WithAttrs([]slog.Attr{componentAttr}),
		&sdkHandler{
			bridge:    sdk.WithAttrs([]slog.Attr{componentAttr}),
			level:     level,
			component: component,
		},
	)
	return slog.New(handler), func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = provider.Shutdown(ctx)
	}
}

type runtimeProcessor struct {
	sdklog.Processor
	resource *resource.Resource
}

func (p runtimeProcessor) OnEmit(ctx context.Context, record *sdklog.Record) error {
	// The SDK merges OTEL_RESOURCE_ATTRIBUTES into the configured resource.
	// Drop records with extra resource data before they reach the processor.
	if record.Resource().Equal(p.resource) {
		// Keep processor errors out of the SDK's global stderr error handler.
		filtered := record.Clone()
		_ = p.Processor.OnEmit(ctx, &filtered)
	}
	return nil
}

type attrScope uint8

const (
	rootScope attrScope = iota
	detailsScope
	localOnlyScope
)

const detailsGroup = "details"

type sdkHandler struct {
	bridge    slog.Handler
	level     slog.Level
	component Component
	scope     attrScope
}

func (h *sdkHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.level && h.bridge.Enabled(ctx, level)
}

func (h *sdkHandler) Handle(ctx context.Context, record slog.Record) error {
	if !h.component.approvedMessage(record.Message) {
		return nil
	}
	filtered := slog.NewRecord(record.Time, record.Level, record.Message, 0)
	record.Attrs(func(attr slog.Attr) bool {
		filtered.AddAttrs(h.approvedAttrs(h.scope, []slog.Attr{attr})...)
		return true
	})
	return h.bridge.Handle(ctx, filtered)
}

func (h *sdkHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	filtered := h.approvedAttrs(h.scope, attrs)
	if len(filtered) == 0 {
		return h
	}
	next := *h
	next.bridge = h.bridge.WithAttrs(filtered)
	return &next
}

func (h *sdkHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	// Only a top-level details group can contain reviewed attributes. Logging
	// from any other group can still produce an approved event.
	next.scope = localOnlyScope
	if h.scope == rootScope && name == detailsGroup {
		next.scope = detailsScope
		next.bridge = h.bridge.WithGroup(name)
	}
	return &next
}
