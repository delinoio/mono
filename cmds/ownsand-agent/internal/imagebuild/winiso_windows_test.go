// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// These tests run the production UTF-8 JSON wrapper of the Windows image
// reads against synthetic localized metadata. They need no Windows image or
// ISO, no elevation and no VM; they start only the Windows PowerShell of the
// host.

// localizedImages are synthetic images with localized edition names.
var localizedImages = []wimImage{
	{1, "Windows 11 Pro"},
	{2, "Windows 11 ホーム"},                   // Japanese
	{3, "Windows 11 프로 한국어"},                // Korean
	{4, "Windows 11 Édition Intégrale éàü"}, // accented Latin
	{5, "Windows 11 专业版 \U0001F5A5"},        // Chinese and a character outside the BMP
}

// psText returns a PowerShell expression that builds s from UTF-16 code
// units, so the script text is independent of how the command line is
// encoded.
func psText(s string) string {
	units := utf16.Encode([]rune(s))
	parts := make([]string, len(units))
	for i, u := range units {
		parts[i] = fmt.Sprintf("0x%04x", u)
	}
	return "(-join ([char[]]@(" + strings.Join(parts, ",") + ")))"
}

// listPipeline returns a pipeline that prints images like listImagesPipeline.
func listPipeline(images []wimImage) string {
	objs := make([]string, len(images))
	for i, im := range images {
		objs[i] = fmt.Sprintf("[pscustomobject]@{ ImageIndex = %d; ImageName = %s }", im.Index, psText(im.Name))
	}
	return "@(" + strings.Join(objs, ", ") + ") | ConvertTo-Json -Compress"
}

// detailsPipeline returns a pipeline that prints one image like
// imageDetailsPipeline.
func detailsPipeline(name string) string {
	return fmt.Sprintf("[pscustomobject]@{ ImageName = %s; InstallationType = 'Client'; Architecture = 9; Version = '10.0.26100'; Languages = @('ja-JP', 'en-US'); DefaultLanguageIndex = 0 } | ConvertTo-Json -Compress", psText(name))
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// checkLocalizedReads decodes the output of the list and details pipelines
// and selects the localized editions by name and by index.
func checkLocalizedReads(t *testing.T, list, details []byte) {
	t.Helper()
	got, err := parseImageList(list)
	if err != nil {
		t.Fatalf("parse the image list %q: %v", list, err)
	}
	if len(got) != len(localizedImages) {
		t.Fatalf("got %d images, want %d: %+v", len(got), len(localizedImages), got)
	}
	for i, want := range localizedImages {
		if got[i] != want {
			t.Errorf("image %d = %+v, want %+v", i, got[i], want)
		}
		byName, err := selectEdition(got, want.Name)
		if err != nil || byName != want {
			t.Errorf("select %q by name = %+v, %v", want.Name, byName, err)
		}
		byIndex, err := selectEdition(got, fmt.Sprint(want.Index))
		if err != nil || byIndex != want {
			t.Errorf("select index %d = %+v, %v", want.Index, byIndex, err)
		}
	}
	var d wimDetails
	if err := json.Unmarshal(bytes.TrimSpace(details), &d); err != nil {
		t.Fatalf("decode the image details %q: %v", details, err)
	}
	if d.Name != localizedImages[1].Name || d.InstallationType != "Client" || d.Architecture != archAMD64 || d.language() != "ja-JP" {
		t.Errorf("image details = %+v", d)
	}
}

func TestUTF8JSONLocalizedMedia(t *testing.T) {
	ctx := testContext(t)
	list, err := powershellUTF8JSON(ctx, listPipeline(localizedImages))
	if err != nil {
		t.Fatal(err)
	}
	details, err := powershellUTF8JSON(ctx, detailsPipeline(localizedImages[1].Name))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(list, []byte{0xEF, 0xBB, 0xBF}) {
		t.Error("the list output starts with a byte order mark")
	}
	checkLocalizedReads(t, list, details)
}

// TestUTF8JSONASCIIAndSingleImage checks that ASCII names are unchanged and
// that a single image, which ConvertTo-Json writes as an object, still parses.
func TestUTF8JSONASCIIAndSingleImage(t *testing.T) {
	ctx := testContext(t)
	one := []wimImage{{1, "Windows Server 2025 Standard Evaluation"}}
	out, err := powershellUTF8JSON(ctx, listPipeline(one))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(out), []byte("{")) {
		t.Fatalf("a single image is not an object: %s", out)
	}
	got, err := parseImageList(out)
	if err != nil || len(got) != 1 || got[0] != one[0] {
		t.Fatalf("single image = %+v, %v", got, err)
	}
	if im, err := selectEdition(got, "windows server 2025 standard evaluation"); err != nil || im != one[0] {
		t.Errorf("case-insensitive select = %+v, %v", im, err)
	}
	ascii := []wimImage{{1, "Windows 11 Pro"}, {2, "Windows 11 Home"}}
	out, err = powershellUTF8JSON(ctx, listPipeline(ascii))
	if err != nil {
		t.Fatal(err)
	}
	if want := `[{"ImageIndex":1,"ImageName":"Windows 11 Pro"},{"ImageIndex":2,"ImageName":"Windows 11 Home"}]`; string(bytes.TrimSpace(out)) != want {
		t.Errorf("ASCII output = %q, want %q", out, want)
	}
}

var (
	kernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procGetConsoleOutputCP = kernel32.NewProc("GetConsoleOutputCP")
	procSetConsoleOutputCP = kernel32.NewProc("SetConsoleOutputCP")
	errNoConsole           = fmt.Errorf("the test process has no console")
)

// consoleOutputCP returns the output code page of the console of the test
// process, or 0 when it has no console.
func consoleOutputCP() uint32 {
	cp, _, _ := procGetConsoleOutputCP.Call()
	return uint32(cp)
}

// setConsoleOutputCP sets the output code page of the console of the test
// process, which the child PowerShell processes share.
func setConsoleOutputCP(cp uint32) error {
	if consoleOutputCP() == 0 {
		return errNoConsole
	}
	if r, _, err := procSetConsoleOutputCP.Call(uintptr(cp)); r == 0 {
		return err
	}
	return nil
}

// TestUTF8JSONUnderOEMCodePages simulates localized hosts: it sets the
// console output code page, which Windows PowerShell uses to encode redirected
// standard output, to an OEM or ANSI code page of a localized Windows and
// reads the synthetic localized metadata through the wrapper. It restores the
// code page and skips where the process has no console or the code page is
// not installed.
func TestUTF8JSONUnderOEMCodePages(t *testing.T) {
	original := consoleOutputCP()
	if original == 0 {
		t.Skip("the test process has no console, so its output code page cannot be set")
	}
	t.Cleanup(func() {
		if err := setConsoleOutputCP(original); err != nil {
			t.Errorf("restore console output code page %d: %v", original, err)
		}
	})
	ctx := testContext(t)
	for _, cp := range []uint32{437, 850, 932, 936, 949, 1252} {
		t.Run(fmt.Sprintf("cp%d", cp), func(t *testing.T) {
			if err := setConsoleOutputCP(cp); err != nil {
				t.Skipf("code page %d is not available: %v", cp, err)
			}
			list, err := powershellUTF8JSON(ctx, listPipeline(localizedImages))
			if err != nil {
				t.Fatal(err)
			}
			details, err := powershellUTF8JSON(ctx, detailsPipeline(localizedImages[1].Name))
			if err != nil {
				t.Fatal(err)
			}
			checkLocalizedReads(t, list, details)

			// Control: the unwrapped script is the behavior before the fix. It
			// must lose the names, which shows that this code page changes
			// the PowerShell output; where it does not, the check above does
			// not reproduce the problem.
			legacy, err := powershell(ctx, listPipeline(localizedImages))
			if err != nil {
				t.Fatal(err)
			}
			if got, err := parseImageList(legacy); err == nil && len(got) == len(localizedImages) && got[1] == localizedImages[1] {
				t.Logf("code page %d did not change the unwrapped PowerShell output; this run does not reproduce the problem", cp)
			}
		})
	}
}

// TestUTF8JSONWithoutWindow runs the wrapper in a PowerShell process that
// has no console window (CREATE_NO_WINDOW). It does not test a process with no console at all: Windows
// PowerShell 5.1 started with DETACHED_PROCESS does not run its script.
func TestUTF8JSONWithoutWindow(t *testing.T) {
	const createNoWindow = 0x08000000
	ctx := testContext(t)
	cmd := powershellCommand(ctx, utf8JSONScript(listPipeline(localizedImages)))
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run without a window: %v: %s", err, stderr.String())
	}
	got, err := parseImageList(stdout.Bytes())
	if err != nil || len(got) != len(localizedImages) {
		t.Fatalf("image list without a window = %+v, %v (stderr %q)", got, err, stderr.String())
	}
	for i := range localizedImages {
		if got[i] != localizedImages[i] {
			t.Errorf("image %d = %+v, want %+v", i, got[i], localizedImages[i])
		}
	}
}
