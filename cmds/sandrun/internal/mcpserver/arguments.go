// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type toolArgumentValidationKey struct{}

type toolArgumentValidation struct {
	entered       bool
	durationField string
	durationErr   error
}

func validateToolArguments(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		call, ok := req.(*mcp.CallToolRequest)
		if !ok || call.Params == nil {
			return next(ctx, method, req)
		}

		// Inspect the original numbers before the SDK converts them to float64.
		// Let the typed adapter run so its schema and decode diagnostics take precedence.
		field, durationErr := validateDurationArguments(call.Params)
		validation := &toolArgumentValidation{durationField: field, durationErr: durationErr}
		result, err := next(context.WithValue(ctx, toolArgumentValidationKey{}, validation), method, req)
		if err != nil || validation.entered {
			return result, err
		}
		if res, ok := result.(*mcp.CallToolResult); ok && res != nil && res.GetError() != nil {
			diagnostic := res.GetError().Error()
			if validation.durationErr != nil && !strings.Contains(diagnostic, validation.durationField) {
				diagnostic = fmt.Sprintf("%s (%s)", diagnostic, validation.durationErr)
			}
			// SetError fills in the formatted diagnostic only when Content is empty.
			res.Content = nil
			res.SetError(invalid("%s", diagnostic))
		}
		return result, nil
	}
}

func addTool[In, Out any](server *mcp.Server, tool *mcp.Tool, handler mcp.ToolHandlerFor[In, Out]) {
	mcp.AddTool(server, tool, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		if validation, ok := ctx.Value(toolArgumentValidationKey{}).(*toolArgumentValidation); ok {
			validation.entered = true
			if validation.durationErr != nil {
				var zero Out
				return nil, zero, invalid("%v", validation.durationErr)
			}
		}
		return handler(ctx, req, in)
	})
}

func validateDurationArguments(params *mcp.CallToolParamsRaw) (string, error) {
	switch params.Name {
	case "wait_session", "exec", "exec_output", "sync", "download", "transfer_status":
	default:
		return "", nil
	}

	var args map[string]json.RawMessage
	if err := json.Unmarshal(params.Arguments, &args); err != nil {
		return "", nil // The SDK reports malformed arguments.
	}
	const waitField, timeoutField = "wait_seconds", "timeout_seconds"
	const maxWaitSeconds = int64(MaxWait / time.Second)
	if !validDurationNumber(args[waitField], maxWaitSeconds) {
		return waitField, fmt.Errorf("%s must be between 0 and %d in whole seconds", waitField, maxWaitSeconds)
	}
	if params.Name == "exec" && !validDurationNumber(args[timeoutField], maxExecTimeoutSeconds) {
		return timeoutField, fmt.Errorf("%s must be between 0 and %d in whole seconds", timeoutField, maxExecTimeoutSeconds)
	}
	return "", nil
}

// validDurationNumber checks the exact value of a JSON number. The SDK checks
// whether missing, null and non-numeric values match the tool schema.
func validDurationNumber(raw json.RawMessage, maxSeconds int64) bool {
	if len(raw) == 0 || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) {
		return true
	}

	mantissa, negative := strings.CutPrefix(string(raw), "-")
	var exponent string
	if i := strings.IndexAny(mantissa, "eE"); i >= 0 {
		mantissa, exponent = mantissa[:i], mantissa[i+1:]
	}
	whole, fraction, _ := strings.Cut(mantissa, ".")
	digits := strings.TrimLeft(whole+fraction, "0")
	if digits == "" {
		return true
	}
	if negative {
		return false
	}

	power := 0
	if exponent != "" {
		var err error
		power, err = strconv.Atoi(exponent)
		if err != nil {
			return false
		}
	}
	significant := strings.TrimRight(digits, "0")
	places := len(fraction) - (len(digits) - len(significant))
	// Bound the shift before expanding it. Negative shifts leave a fractional
	// part, and a result with more digits than the limit is out of range.
	maxDigits := len(strconv.FormatInt(maxSeconds, 10))
	if power < places || power > places+maxDigits-len(significant) {
		return false
	}
	seconds, err := strconv.ParseInt(significant+strings.Repeat("0", power-places), 10, 64)
	return err == nil && seconds <= maxSeconds
}
