// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
)

func TestDownloadDestinationCaseCollision(t *testing.T) {
	const first, second = "Report.txt", "report.txt"
	probe := t.TempDir()
	caseSensitive := caseSensitiveDir(t, probe)
	t.Logf("the destination volume is case-sensitive: %t", caseSensitive)
	firstEntry := &tar.Header{Name: first, Typeflag: tar.TypeReg, Size: 1, Mode: 0o644}
	secondEntry := &tar.Header{Name: second, Typeflag: tar.TypeReg, Size: 2, Mode: 0o644}
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			dest, result, err := downloadCaseArchive(t, t.TempDir(), existing, firstEntry, secondEntry)
			if !caseSensitive {
				checkArchiveCollision(t, dest, result, err, existing, first, second)
				return
			}
			if err != nil || result == nil || !result.Directory || result.Files != 2 || result.Bytes != 3 {
				t.Fatalf("case-distinct download = %+v, %v", result, err)
			}
			checkDownloadedFiles(t, dest, []string{first, second}, []string{"x", "xx"})
		})
	}
}

func TestDownloadArchiveSeparatorCollision(t *testing.T) {
	const first, second = "folder/report.txt", `./folder\report.txt`
	slashPath, backslashPath := filepath.FromSlash(first), filepath.FromSlash(strings.TrimPrefix(second, "./"))
	aliases := slashPath == backslashPath
	t.Logf("the host maps %q and %q to the same path: %t", first, second, aliases)
	firstEntry := &tar.Header{Name: first, Typeflag: tar.TypeReg, Size: 1, Mode: 0o644}
	secondEntry := &tar.Header{Name: second, Typeflag: tar.TypeReg, Size: 2, Mode: 0o644}
	for _, order := range [][]*tar.Header{{firstEntry, secondEntry}, {secondEntry, firstEntry}} {
		t.Run(fmt.Sprintf("first-size=%d", order[0].Size), func(t *testing.T) {
			for _, existing := range []bool{false, true} {
				t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
					parent := t.TempDir()
					dest, result, err := downloadCaseArchive(t, parent, existing, order...)
					if aliases {
						checkArchiveCollision(t, dest, result, err, existing, order[0].Name, order[1].Name)
						return
					}
					if err != nil || result == nil || !result.Directory || result.Files != 2 || result.Bytes != 3 {
						t.Fatalf("separator-distinct download = %+v, %v", result, err)
					}
					if data, err := os.ReadFile(filepath.Join(dest, backslashPath)); err != nil || string(data) != "xx" {
						t.Fatalf("literal backslash path = %q, %v, want %q", data, err, "xx")
					}
					checkDownloadedFiles(t, filepath.Join(dest, "folder"), []string{"report.txt"}, []string{"x"})
				})
			}
		})
	}
}

func TestDownloadArchiveMixedCaseCollision(t *testing.T) {
	const regular, link, target = "Report.txt", "report.txt", "anchor.txt"
	parent := t.TempDir()
	probe := filepath.Join(parent, "probe")
	if err := os.Mkdir(probe, 0o755); err != nil {
		t.Fatal(err)
	}
	caseSensitive := caseSensitiveDir(t, probe)
	linkErr := os.Symlink("case", filepath.Join(probe, "link"))
	t.Logf("the destination volume is case-sensitive: %t. The process can create a symlink: %t", caseSensitive, linkErr == nil)

	anchor := &tar.Header{Name: target, Typeflag: tar.TypeReg, Size: 1, Mode: 0o644}
	file := &tar.Header{Name: regular, Typeflag: tar.TypeReg, Size: 2, Mode: 0o644}
	symlink := &tar.Header{Name: link, Typeflag: tar.TypeSymlink, Linkname: target}
	for _, tc := range []struct {
		name  string
		order []*tar.Header
	}{
		{name: "regular-first", order: []*tar.Header{anchor, file, symlink}},
		{name: "symlink-first", order: []*tar.Header{anchor, symlink, file}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if linkErr != nil && tc.order[1] == symlink {
				t.Skipf("a staged symlink is required: %v", linkErr)
			}
			for _, existing := range []bool{false, true} {
				t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
					dest, result, err := downloadCaseArchive(t, t.TempDir(), existing, tc.order...)
					if !caseSensitive {
						checkArchiveCollision(t, dest, result, err, existing, tc.order[1].Name, tc.order[2].Name)
						return
					}
					if linkErr != nil {
						checkArchiveLinkWarning(t, dest, result, err, link)
						return
					}
					if err != nil || result == nil || !result.Directory || result.Files != 3 || result.Bytes != 3 || len(result.Warnings) != 0 {
						t.Fatalf("case-distinct mixed download = %+v, %v", result, err)
					}
					checkDownloadedFiles(t, dest, []string{regular, target, link}, []string{"xx", "x", "x"})
					if got, err := os.Readlink(filepath.Join(dest, link)); err != nil || got != target {
						t.Fatalf("symlink target = %q, %v, want %q", got, err, target)
					}
				})
			}
		})
	}

	t.Run("noncolliding-symlink-without-privileges", func(t *testing.T) {
		if linkErr == nil {
			t.Skip("the process can create symbolic links")
		}
		noncolliding := &tar.Header{Name: "shortcut.txt", Typeflag: tar.TypeSymlink, Linkname: target}
		dest, result, err := downloadCaseArchive(t, t.TempDir(), true, anchor, file, noncolliding)
		checkArchiveLinkWarning(t, dest, result, err, noncolliding.Name)
	})
}

func TestDownloadArchiveMixedParentAliasCollision(t *testing.T) {
	parent := t.TempDir()
	if !caseSensitiveDir(t, parent) {
		t.Skip("this fixture uses a separate case-variant parent symlink")
	}
	if err := os.Symlink(".", filepath.Join(parent, "probe-link")); err != nil {
		t.Skipf("this destination does not support symbolic links: %v", err)
	}
	dir := &tar.Header{Name: "folder/", Typeflag: tar.TypeDir, Mode: 0o755}
	parentLink := &tar.Header{Name: "Folder", Typeflag: tar.TypeSymlink, Linkname: "folder"}
	anchor := &tar.Header{Name: "folder/anchor.txt", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644}
	file := &tar.Header{Name: "folder/report.txt", Typeflag: tar.TypeReg, Size: 2, Mode: 0o644}
	link := &tar.Header{Name: "Folder/report.txt", Typeflag: tar.TypeSymlink, Linkname: "anchor.txt"}
	for _, order := range [][]*tar.Header{{file, link}, {link, file}} {
		t.Run(order[0].Name, func(t *testing.T) {
			dest, result, err := downloadCaseArchive(t, t.TempDir(), true, dir, parentLink, anchor, order[0], order[1])
			checkArchiveCollision(t, dest, result, err, true, order[0].Name, order[1].Name)
		})
	}
}

func downloadCaseArchive(t *testing.T, parent string, existing bool, entries ...*tar.Header) (string, *DownloadResult, error) {
	t.Helper()
	body, err := io.ReadAll(tarOf(t, entries...))
	if err != nil {
		t.Fatal(err)
	}
	fake := &scriptedDownloadSession{probe: fmt.Sprintf("dir %d", len(body)), chunks: [][]byte{body}, endSize: int64(len(body))}
	client := newScriptedDownloadClient(t, fake)
	dest := filepath.Join(parent, "result")
	if existing {
		if err := os.Mkdir(dest, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dest, "keep.txt"), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	result, err := client.Download(t.Context(), DownloadParams{OrganizationID: "org", DevSessionID: "session", GuestPath: "out", LocalPath: dest})
	checkDownloadArchiveCleanup(t, parent, fake)
	return dest, result, err
}

func checkArchiveCollision(t *testing.T, dest string, result *DownloadResult, err error, existing bool, earlier, later string) {
	t.Helper()
	want := fmt.Sprintf("the archive entries %q and %q conflict", earlier, later)
	if result != nil || connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), want) {
		t.Fatalf("archive download = %+v, %v, want failed_precondition with %q", result, err, want)
	}
	if existing {
		checkDownloadedFiles(t, dest, []string{"keep.txt"}, []string{"old"})
	} else if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed download changed absent destination: %v", err)
	}
}

func checkDownloadArchiveCleanup(t *testing.T, parent string, fake *scriptedDownloadSession) {
	t.Helper()
	if matches, err := filepath.Glob(filepath.Join(parent, ".sandrun-download-*")); err != nil || len(matches) != 0 {
		t.Fatalf("local staging remains: %v, %v", matches, err)
	}
	fake.mu.Lock()
	guestArchive := fake.path
	fake.mu.Unlock()
	if guestArchive != "" {
		t.Fatalf("guest archive remains: %s", guestArchive)
	}
}

func checkArchiveLinkWarning(t *testing.T, dest string, result *DownloadResult, err error, link string) {
	t.Helper()
	want := []string{link + ": the symbolic link could not be created"}
	if err != nil || result == nil || !result.Directory || result.Files != 2 || result.Bytes != 3 || !slices.Equal(result.Warnings, want) {
		t.Fatalf("download with unavailable symlink = %+v, %v, want warning %q", result, err, want)
	}
	checkDownloadedFiles(t, dest, []string{"Report.txt", "anchor.txt"}, []string{"xx", "x"})
}

func checkDownloadedFiles(t *testing.T, dir string, names, contents []string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	if !slices.Equal(got, names) {
		t.Fatalf("destination entries = %v, want %v", got, names)
	}
	for i, name := range names {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(data) != contents[i] {
			t.Errorf("%s = %q, %v, want %q", name, data, err, contents[i])
		}
	}
}

func TestDownloadArchiveRepeatedName(t *testing.T) {
	dir := t.TempDir()
	extractor := archiveExtractor{fileLimit: 100, totalLimit: 1000, link: (*os.Root).Link}
	_, err := extractor.extractArchive(t.Context(), tarOf(t,
		&tar.Header{Name: "./file", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2},
		&tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3},
	), dir)
	if err != nil {
		t.Fatal(err)
	}
	checkDownloadedFiles(t, dir, []string{"file"}, []string{"xxx"})
}

func TestDownloadArchiveMixedRepeatedName(t *testing.T) {
	const name, target = "report.txt", "anchor.txt"
	probe := t.TempDir()
	if err := os.WriteFile(filepath.Join(probe, target), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(probe, name)); err != nil {
		t.Skipf("this destination does not support symbolic links: %v", err)
	}
	anchor := &tar.Header{Name: target, Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}
	file := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: 2}
	link := &tar.Header{Name: "./" + name, Typeflag: tar.TypeSymlink, Linkname: target}

	t.Run("symlink-replaces-regular", func(t *testing.T) {
		dest, _, err := downloadCaseArchive(t, t.TempDir(), false, anchor, file, link)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := os.Readlink(filepath.Join(dest, name)); err != nil || got != target {
			t.Fatalf("symlink target = %q, %v, want %q", got, err, target)
		}
	})
	t.Run("regular-replaces-symlink", func(t *testing.T) {
		dest, _, err := downloadCaseArchive(t, t.TempDir(), false, anchor, link, file)
		if err != nil {
			t.Fatal(err)
		}
		checkDownloadedFiles(t, dest, []string{target, name}, []string{"x", "xx"})
	})
}

func TestDownloadArchiveDistinctHardLink(t *testing.T) {
	dir := t.TempDir()
	probe := filepath.Join(dir, "probe")
	if err := os.Mkdir(probe, 0o755); err != nil {
		t.Fatal(err)
	}
	if !caseSensitiveDir(t, probe) {
		t.Skip("this destination volume does not store both names separately")
	}
	if err := os.Link(filepath.Join(probe, "case"), filepath.Join(probe, "CASE")); err != nil {
		t.Skipf("this destination volume does not support hard links: %v", err)
	}

	// The second regular entry must replace just the distinct hard link,
	// even though it shares the original file's identity before replacement.
	result := filepath.Join(dir, "result")
	if err := os.Mkdir(result, 0o755); err != nil {
		t.Fatal(err)
	}
	extractor := archiveExtractor{fileLimit: 100, totalLimit: 1000, link: (*os.Root).Link}
	_, err := extractor.extractArchive(t.Context(), tarOf(t,
		&tar.Header{Name: "Report.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2},
		&tar.Header{Name: "report.txt", Typeflag: tar.TypeLink, Linkname: "Report.txt"},
		&tar.Header{Name: "report.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3},
	), result)
	if err != nil {
		t.Fatal(err)
	}
	checkDownloadedFiles(t, result, []string{"Report.txt", "report.txt"}, []string{"xx", "xxx"})
}
