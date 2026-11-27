// SPDX-License-Identifier: FSL-1.1-ALv2

package runtimelog

import "log/slog"

// approvedMessage admits reviewed static event text. Anything formatted from
// runtime input stays in the local JSON sink.
func (c Component) approvedMessage(message string) bool {
	switch c {
	case Agent:
		switch message {
		case "open state directory failed", "the device is not enrolled; run `ownsand-agent enroll`",
			"read device identity failed", "read agent state failed", "control plane client failed",
			"sandbox runtime unavailable", "agent start failed", "agent starting", "agent stopped",
			"authentication failed", "authenticated", "agent channel connected", "agent channel disconnected",
			"network setup failed", "network setup finished", "sandbox state changed", "sandbox TTL expired",
			"snapshot failed", "snapshot ready", "runtime close failed", "removal clean-up failed",
			"removal clean-up finished", "agent update verified", "agent update installed":
			return true
		}
	case Guest:
		switch message {
		case "listen failed", "start failed", "serve failed", "guest helper serving",
			"log rotation failed; appending to the active log", "network tunnel started", "network tunnel failed",
			"network tunnel failed; exiting so that init restarts the helper", "set clock ignored",
			"set clock failed", "clock set", "exec started", "exec failed to start", "exec exited",
			"system disk checked", "system disk grown":
			return true
		}
	}
	return false
}

// approvedAttrs applies the same policy to bound and per-record attributes.
// It inspects concrete slog values without evaluating arbitrary LogValuers
// or converting errors and other objects to text.
func (h *sdkHandler) approvedAttrs(scope attrScope, attrs []slog.Attr) []slog.Attr {
	if scope == localOnlyScope {
		return nil
	}
	var filtered []slog.Attr
	for _, attr := range attrs {
		if attr.Value.Kind() == slog.KindGroup {
			switch {
			case attr.Key == "":
				filtered = append(filtered, h.approvedAttrs(scope, attr.Value.Group())...)
			case scope == rootScope && attr.Key == detailsGroup:
				children := h.approvedAttrs(detailsScope, attr.Value.Group())
				if len(children) > 0 {
					filtered = append(filtered, slog.GroupAttrs(attr.Key, children...))
				}
			}
		} else if h.component.approvedField(attr) {
			filtered = append(filtered, attr)
		}
	}
	return filtered
}

func (c Component) approvedField(attr slog.Attr) bool {
	value := attr.Value
	switch c {
	case Agent:
		switch attr.Key {
		case "runtime":
			if value.Kind() != slog.KindString {
				return false
			}
			switch value.String() {
			case "unspecified", "firecracker", "process", "virtualization", "hyperv":
				return true
			}
		case "setting":
			return value.Kind() == slog.KindString && value.String() == "OWNSAND_AGENT_API_URL"
		}
	case Guest:
		switch attr.Key {
		case "operation":
			return value.Kind() == slog.KindString && value.String() == "rotate"
		case "network":
			if value.Kind() == slog.KindString {
				switch value.String() {
				case "unix", "tcp", "vsock", "hvsock":
					return true
				}
			}
		case "clock_mode":
			return value.Kind() == slog.KindString && value.String() == "ignore"
		case "limit_bytes", "offset_ms", "duration_ms", "exit_code":
			return value.Kind() == slog.KindInt64
		case "timed_out":
			return value.Kind() == slog.KindBool
		}
	}
	return false
}
