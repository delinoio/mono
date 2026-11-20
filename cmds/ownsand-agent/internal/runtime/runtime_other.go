//go:build !linux && !darwin && !windows

package runtime

// New returns a runtime that supports no guest on other host OSes.
func New(Config) (Runtime, error) {
	return &unsupportedRuntime{name: "none", checks: map[string]string{"hypervisor": "unsupported host OS"}}, nil
}
