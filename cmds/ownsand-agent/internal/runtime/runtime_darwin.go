//go:build darwin

package runtime

// New returns the macOS runtime. Virtualization.framework bindings are a new
// dependency that needs owner approval, so Linux and macOS guests are
// reported as unsupported on macOS hosts in this version.
func New(Config) (Runtime, error) {
	return &unsupportedRuntime{name: "virtualization-framework", checks: map[string]string{
		"virtualization_framework": "not implemented in this agent version",
	}}, nil
}
