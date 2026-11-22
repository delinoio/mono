// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// The ISO and the Windows image are read with PowerShell (Mount-DiskImage,
// Get-WindowsImage), whose objects have stable property names; dism.exe and
// diskpart.exe print localized text, so the build reads only their exit
// codes. Every path goes to a script through an environment variable, never
// through the script text.

// powershell runs script with env added to the environment and returns its
// standard output.
func powershell(ctx context.Context, script string, env ...string) ([]byte, error) {
	ps := filepath.Join(os.Getenv("SystemRoot"), `System32\WindowsPowerShell\v1.0\powershell.exe`)
	cmd := exec.CommandContext(ctx, ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command",
		"$ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue'; "+script)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, tail(stderr.String(), 2048))
	}
	return stdout.Bytes(), nil
}

// mountISO attaches the ISO read-only, or uses it where it is attached
// already; see mountISOWith.
func mountISO(ctx context.Context, path string) (*mountedISO, error) {
	return mountISOWith(ctx, path, powershell)
}

// installImage returns sources\install.wim or sources\install.esd of the
// mounted ISO.
func (m *mountedISO) installImage() (string, error) {
	for _, name := range []string{"install.wim", "install.esd"} {
		p := filepath.Join(m.Root, "sources", name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p, nil
		}
	}
	return "", fmt.Errorf("imagebuild: the ISO has no sources\\install.wim or sources\\install.esd; is it a Windows installation ISO?")
}

// wimImage is one image of a Windows image file.
type wimImage struct {
	Index int    `json:"ImageIndex"`
	Name  string `json:"ImageName"`
}

// wimDetails are the properties of the selected image.
type wimDetails struct {
	Name             string   `json:"ImageName"`
	InstallationType string   `json:"InstallationType"`
	Architecture     int      `json:"Architecture"`
	Version          string   `json:"Version"`
	Languages        []string `json:"Languages"`
	DefaultLanguage  int      `json:"DefaultLanguageIndex"`
}

// archAMD64 is the Architecture value of an x64 image (PROCESSOR_ARCHITECTURE_AMD64).
const archAMD64 = 9

// listImages lists the images of a WIM or ESD file.
func listImages(ctx context.Context, wim string) ([]wimImage, error) {
	out, err := powershell(ctx, `@(Get-WindowsImage -ImagePath $env:OWNSAND_WIM | Select-Object ImageIndex, ImageName) | ConvertTo-Json -Compress`, "OWNSAND_WIM="+wim)
	if err != nil {
		return nil, fmt.Errorf("imagebuild: list the Windows images: %w", err)
	}
	return parseImageList(out)
}

// parseImageList parses the JSON of listImages; ConvertTo-Json writes a
// single image as an object, not an array.
func parseImageList(out []byte) ([]wimImage, error) {
	out = bytes.TrimSpace(out)
	var list []wimImage
	if len(out) > 0 && out[0] == '{' {
		var one wimImage
		if err := json.Unmarshal(out, &one); err != nil {
			return nil, err
		}
		list = append(list, one)
	} else if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("imagebuild: unexpected image list %q", tail(string(out), 256))
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("imagebuild: the Windows image file holds no image")
	}
	return list, nil
}

// imageDetails reads the properties of one image.
func imageDetails(ctx context.Context, wim string, index int) (wimDetails, error) {
	out, err := powershell(ctx, `Get-WindowsImage -ImagePath $env:OWNSAND_WIM -Index ([int]$env:OWNSAND_INDEX) | Select-Object ImageName, InstallationType, Architecture, Version, Languages, DefaultLanguageIndex | ConvertTo-Json -Compress`,
		"OWNSAND_WIM="+wim, "OWNSAND_INDEX="+strconv.Itoa(index))
	if err != nil {
		return wimDetails{}, fmt.Errorf("imagebuild: read the Windows image: %w", err)
	}
	var d wimDetails
	if err := json.Unmarshal(bytes.TrimSpace(out), &d); err != nil {
		return wimDetails{}, fmt.Errorf("imagebuild: unexpected image details %q", tail(string(out), 256))
	}
	return d, nil
}

// language returns the default language of the image.
func (d wimDetails) language() string {
	if d.DefaultLanguage >= 0 && d.DefaultLanguage < len(d.Languages) {
		return d.Languages[d.DefaultLanguage]
	}
	if len(d.Languages) > 0 {
		return d.Languages[0]
	}
	return "en-US"
}

// selectEdition picks the image of --edition: empty selects index 1, a
// number selects that index, and other text selects the image with exactly
// that name (case-insensitive when no name matches exactly).
func selectEdition(list []wimImage, edition string) (wimImage, error) {
	edition = strings.TrimSpace(edition)
	if edition == "" {
		edition = "1"
	}
	if n, err := strconv.Atoi(edition); err == nil {
		for _, im := range list {
			if im.Index == n {
				return im, nil
			}
		}
		return wimImage{}, fmt.Errorf("imagebuild: the ISO has no image with index %d; %s", n, describeImages(list))
	}
	for _, im := range list {
		if im.Name == edition {
			return im, nil
		}
	}
	var found []wimImage
	for _, im := range list {
		if strings.EqualFold(im.Name, edition) {
			found = append(found, im)
		}
	}
	if len(found) == 1 {
		return found[0], nil
	}
	return wimImage{}, fmt.Errorf("imagebuild: the ISO has no image named %q; %s", edition, describeImages(list))
}

func describeImages(list []wimImage) string {
	parts := make([]string, len(list))
	for i, im := range list {
		parts[i] = fmt.Sprintf("%d: %s", im.Index, im.Name)
	}
	return "pass --edition with one of " + strings.Join(parts, ", ")
}

// guestMinimums returns the smallest vCPU count and memory of a guest of
// the installation type (Microsoft's minimum system requirements; Windows
// 11 needs 2 cores and 4 GB, Windows Server with Desktop Experience 2 GB).
func guestMinimums(installationType string) (cpus int32, memoryMiB int64) {
	switch strings.ToLower(installationType) {
	case "client":
		return 2, 4096
	case "server core":
		return 1, 1024
	}
	return 1, 2048
}
