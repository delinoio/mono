//go:build linux

package runtime

// New returns the Linux runtime. The Firecracker runtime is not part of this
// layer yet, so every guest is reported as unsupported.
func New(Config) (Runtime, error) {
	return &unsupportedRuntime{name: "firecracker", checks: map[string]string{
		"firecracker": "not implemented in this agent version",
	}}, nil
}
