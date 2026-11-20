package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"time"
)

// API is a client of the Firecracker API on its Unix socket.
type API struct {
	hc *http.Client
}

// NewAPI returns a client for the API socket at path.
func NewAPI(path string) *API {
	return &API{hc: &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialUnix(ctx, path)
			},
			DisableKeepAlives: true,
		},
	}}
}

// APIError is an error response of the Firecracker API.
type APIError struct {
	Method, Path string
	Status       int
	Fault        string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("firecracker %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Fault)
}

// Do sends a JSON request.
func (a *API) Do(ctx context.Context, method, path string, body any) error {
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := a.hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 == 2 {
		return nil
	}
	var fault struct {
		FaultMessage string `json:"fault_message"`
	}
	data, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	_ = json.Unmarshal(data, &fault)
	return &APIError{Method: method, Path: path, Status: res.StatusCode, Fault: fault.FaultMessage}
}

// BootSource is PUT /boot-source.
type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
}

// Drive is PUT /drives/{id}.
type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

// MachineConfig is PUT /machine-config.
type MachineConfig struct {
	VCPUCount  int64 `json:"vcpu_count"`
	MemSizeMiB int64 `json:"mem_size_mib"`
	SMT        bool  `json:"smt"`
}

// NetworkInterface is PUT /network-interfaces/{id}.
type NetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac,omitempty"`
}

// Vsock is PUT /vsock.
type Vsock struct {
	GuestCID uint32 `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

// Action is PUT /actions.
type Action struct {
	ActionType string `json:"action_type"`
}

// VMState is PATCH /vm.
type VMState struct {
	State string `json:"state"`
}

// SnapshotCreate is PUT /snapshot/create.
type SnapshotCreate struct {
	SnapshotType string `json:"snapshot_type"`
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

// MemBackend is the memory backend of a snapshot load.
type MemBackend struct {
	BackendType string `json:"backend_type"`
	BackendPath string `json:"backend_path"`
}

// NetworkOverride replaces the tap device of an interface at restore.
type NetworkOverride struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
}

// VsockOverride replaces the vsock socket path at restore.
type VsockOverride struct {
	UDSPath string `json:"uds_path"`
}

// SnapshotLoad is PUT /snapshot/load.
type SnapshotLoad struct {
	SnapshotPath     string            `json:"snapshot_path"`
	MemBackend       MemBackend        `json:"mem_backend"`
	ResumeVM         bool              `json:"resume_vm"`
	NetworkOverrides []NetworkOverride `json:"network_overrides,omitempty"`
	VsockOverride    *VsockOverride    `json:"vsock_override,omitempty"`
}

// maxSunPath is a safe length limit of a Unix socket path (sun_path holds
// 108 bytes on Linux).
const maxSunPath = 100

// dialUnix connects to a Unix socket. A path longer than sun_path allows is
// reached through its directory: see dialLongUnix.
func dialUnix(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	if len(path) <= maxSunPath {
		return d.DialContext(ctx, "unix", path)
	}
	return dialLongUnix(ctx, filepath.Dir(path), filepath.Base(path))
}
