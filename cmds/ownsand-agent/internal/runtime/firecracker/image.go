package firecracker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/update"
)

// Base image files in <state>/images/linux/.
const (
	KernelFile    = "vmlinux"
	RootfsFile    = "rootfs.ext4"
	ImageManifest = "image.json"
)

// ImageInfo is the signed payload of image.json. It uses the same signed
// envelope and keys as the agent release manifest.
type ImageInfo struct {
	Version      string `json:"version"`
	Distribution string `json:"distribution"`
	KernelSource string `json:"kernel_source"`
	KernelSHA256 string `json:"kernel_sha256"`
	RootfsSHA256 string `json:"rootfs_sha256"`
}

// ImageStatus is the result of a base image check.
type ImageStatus int

const (
	ImageMissing ImageStatus = iota + 1
	ImageVerified
	ImageUnsigned // accepted only in the explicit development mode
	ImageInvalid
)

// VerifyImage checks the base image in dir: the signature of image.json with
// the built-in release keys and the SHA-256 of the kernel and the root file
// system. With allowUnsigned (development only), a present kernel and root
// file system without a valid manifest is accepted as ImageUnsigned.
func VerifyImage(dir string, keys []update.ReleaseKey, allowUnsigned bool) (ImageStatus, error) {
	kernel, rootfs := filepath.Join(dir, KernelFile), filepath.Join(dir, RootfsFile)
	for _, p := range []string{kernel, rootfs} {
		if _, err := os.Stat(p); err != nil {
			return ImageMissing, fmt.Errorf("base image file %s is missing", filepath.Base(p))
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, ImageManifest))
	var info ImageInfo
	if err == nil {
		err = verifySigned(data, keys, &info)
	}
	if err != nil {
		if allowUnsigned {
			return ImageUnsigned, nil
		}
		return ImageInvalid, fmt.Errorf("base image manifest: %w", err)
	}
	for _, f := range []struct{ path, want string }{{kernel, info.KernelSHA256}, {rootfs, info.RootfsSHA256}} {
		sum, err := fileSHA256(f.path)
		if err != nil {
			return ImageInvalid, err
		}
		if sum != f.want {
			return ImageInvalid, fmt.Errorf("base image file %s does not match its manifest hash", filepath.Base(f.path))
		}
	}
	return ImageVerified, nil
}

func verifySigned(data []byte, keys []update.ReleaseKey, v any) error {
	// The image manifest reuses the release manifest envelope: the payload is
	// signed with the same context string and keys.
	payload, err := update.VerifyPayload(data, keys)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(payload, v); err != nil {
		return errors.New("invalid image manifest payload")
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
