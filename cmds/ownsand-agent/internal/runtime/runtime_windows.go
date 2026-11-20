//go:build windows

package runtime

// New returns the Windows runtime. Host Compute Service bindings are a new
// dependency that needs owner approval, so Linux and Windows guests are
// reported as unsupported on Windows hosts in this version.
func New(Config) (Runtime, error) {
	return &unsupportedRuntime{name: "hyper-v", checks: map[string]string{
		"hyper_v": "not implemented in this agent version",
	}}, nil
}
