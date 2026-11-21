// SPDX-License-Identifier: FSL-1.1-ALv2

package fcrt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// apiClient calls the Firecracker API on its unix socket.
type apiClient struct {
	hc *http.Client
}

func newAPIClient(socket string) *apiClient {
	return &apiClient{hc: &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}},
	}}
}

type apiFault struct {
	FaultMessage string `json:"fault_message"`
}

// call sends one request; body is encoded as JSON when not nil.
func (c *apiClient) call(ctx context.Context, method, path string, body any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://firecracker"+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("fcrt: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		var f apiFault
		_ = json.Unmarshal(data, &f)
		return fmt.Errorf("fcrt: %s %s: HTTP %d: %s", method, path, resp.StatusCode, f.FaultMessage)
	}
	return nil
}

// waitReady polls the API until it answers.
func (c *apiClient) waitReady(ctx context.Context, exited <-chan struct{}) error {
	for {
		if err := c.call(ctx, http.MethodGet, "/", nil); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("fcrt: API socket not ready: %w", ctx.Err())
		case <-exited:
			return fmt.Errorf("fcrt: firecracker exited during start")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Request bodies of the Firecracker API.
type (
	machineConfig struct {
		VCPUCount  int32 `json:"vcpu_count"`
		MemSizeMiB int64 `json:"mem_size_mib"`
		SMT        bool  `json:"smt"`
	}
	bootSource struct {
		KernelImagePath string `json:"kernel_image_path"`
		BootArgs        string `json:"boot_args"`
	}
	drive struct {
		DriveID      string `json:"drive_id"`
		PathOnHost   string `json:"path_on_host"`
		IsRootDevice bool   `json:"is_root_device"`
		IsReadOnly   bool   `json:"is_read_only"`
	}
	networkInterface struct {
		IfaceID     string `json:"iface_id"`
		HostDevName string `json:"host_dev_name"`
	}
	vsock struct {
		GuestCID uint32 `json:"guest_cid"`
		UDSPath  string `json:"uds_path"`
	}
	action struct {
		ActionType string `json:"action_type"`
	}
	vmState struct {
		State string `json:"state"`
	}
	snapshotCreate struct {
		SnapshotType string `json:"snapshot_type"`
		SnapshotPath string `json:"snapshot_path"`
		MemFilePath  string `json:"mem_file_path"`
	}
	memBackend struct {
		BackendType string `json:"backend_type"`
		BackendPath string `json:"backend_path"`
	}
	vsockOverride struct {
		UDSPath string `json:"uds_path"`
	}
	snapshotLoad struct {
		SnapshotPath  string         `json:"snapshot_path"`
		MemBackend    memBackend     `json:"mem_backend"`
		ResumeVM      bool           `json:"resume_vm"`
		VsockOverride *vsockOverride `json:"vsock_override,omitempty"`
		ClockRealtime bool           `json:"clock_realtime"`
	}
)
