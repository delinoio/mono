// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"archive/tar"
	"bytes"
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
	const firstContent, secondContent = "first", "second"
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for _, file := range []struct{ name, content string }{{first, firstContent}, {second, secondContent}} {
		if err := tw.WriteHeader(&tar.Header{Name: file.name, Mode: 0o644, Size: int64(len(file.content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(file.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	size := int64(archive.Len())
	fake := &scriptedDownloadSession{probe: fmt.Sprintf("dir %d", size), chunks: [][]byte{archive.Bytes()}, endSize: size}
	client := newScriptedDownloadClient(t, fake)
	parent := t.TempDir()
	probeDir := filepath.Join(parent, "probe")
	if err := os.Mkdir(probeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(probeDir, first), []byte(firstContent), 0o644); err != nil {
		t.Fatal(err)
	}
	aliasContents, err := os.ReadFile(filepath.Join(probeDir, second))
	aliases := err == nil
	if aliases && string(aliasContents) != firstContent || !aliases && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("filesystem case probe = %q, %v", aliasContents, err)
	}
	t.Logf("the destination volume aliases %q and %q: %t", first, second, aliases)

	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			dest := filepath.Join(parent, fmt.Sprintf("result-%t", existing))
			if existing {
				if err := os.Mkdir(dest, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dest, "keep.txt"), []byte("old"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			res, err := client.Download(t.Context(), DownloadParams{OrganizationID: "org", DevSessionID: "session", GuestPath: "out", LocalPath: dest})
			if aliases {
				if res != nil || connect.CodeOf(err) != connect.CodeFailedPrecondition ||
					!strings.Contains(err.Error(), first) || !strings.Contains(err.Error(), second) {
					t.Fatalf("case collision = %+v, %v", res, err)
				}
				if existing {
					checkDownloadedFiles(t, dest, []string{"keep.txt"}, []string{"old"})
				} else if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed download changed absent destination: %v", err)
				}
			} else {
				if err != nil || res == nil || !res.Directory || res.Files != 2 || res.Bytes != 11 {
					t.Fatalf("case-distinct download = %+v, %v", res, err)
				}
				checkDownloadedFiles(t, dest, []string{first, second}, []string{firstContent, secondContent})
			}

			checkDownloadCleanup(t, parent, fake)
		})
	}
}

func TestDownloadDirectoryCaseCollision(t *testing.T) {
	const first, second = "Reports", "reports"
	parent := t.TempDir()
	probe := filepath.Join(parent, "probe")
	if err := os.MkdirAll(filepath.Join(probe, first), 0o755); err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(filepath.Join(probe, first))
	if err != nil {
		t.Fatal(err)
	}
	alternate, err := os.Stat(filepath.Join(probe, second))
	aliases := err == nil && os.SameFile(original, alternate)
	if !aliases && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected directory case probe result: %v", err)
	}
	t.Logf("the staging volume aliases %q and %q: %t", first, second, aliases)

	for _, tc := range []struct {
		name     string
		prefix   string
		explicit bool
	}{
		{name: "explicit", explicit: true},
		{name: "implicit", prefix: "nested/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			firstDir, secondDir := tc.prefix+first, tc.prefix+second
			left := &tar.Header{Typeflag: tar.TypeReg, Name: "./" + firstDir + "/left.txt", Mode: 0o644, Size: 1}
			right := &tar.Header{Typeflag: tar.TypeReg, Name: "./" + secondDir + "/right.txt", Mode: 0o644, Size: 2}
			entries := []*tar.Header{left, right}
			if tc.explicit {
				entries = []*tar.Header{
					{Typeflag: tar.TypeDir, Name: "./" + firstDir + "/", Mode: 0o755},
					left,
					{Typeflag: tar.TypeDir, Name: "./" + secondDir + "/", Mode: 0o755},
					right,
				}
			}
			chunk, err := io.ReadAll(tarOf(t, entries...))
			if err != nil {
				t.Fatal(err)
			}
			size := int64(len(chunk))
			fake := &scriptedDownloadSession{probe: fmt.Sprintf("dir %d", size), chunks: [][]byte{chunk}, endSize: size}
			client := newScriptedDownloadClient(t, fake)

			for _, existing := range []bool{false, true} {
				t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
					dest := filepath.Join(parent, fmt.Sprintf("%s-%t", tc.name, existing))
					if existing {
						if err := os.Mkdir(dest, 0o755); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(dest, "keep.txt"), []byte("old"), 0o644); err != nil {
							t.Fatal(err)
						}
					}
					res, err := client.Download(t.Context(), DownloadParams{
						OrganizationID: "org", DevSessionID: "session", GuestPath: "out", LocalPath: dest,
					})
					if aliases {
						if res != nil || connect.CodeOf(err) != connect.CodeFailedPrecondition ||
							!strings.Contains(err.Error(), fmt.Sprintf("%q", firstDir)) ||
							!strings.Contains(err.Error(), fmt.Sprintf("%q", secondDir)) {
							t.Fatalf("directory collision = %+v, %v", res, err)
						}
						if existing {
							checkDownloadedFiles(t, dest, []string{"keep.txt"}, []string{"old"})
						} else if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("failed download changed absent destination: %v", err)
						}
					} else {
						if err != nil || res == nil || !res.Directory || res.Files != 2 || res.Bytes != 3 {
							t.Fatalf("case-distinct directory download = %+v, %v", res, err)
						}
						dirs, err := os.ReadDir(filepath.Join(dest, tc.prefix))
						if err != nil || len(dirs) != 2 || dirs[0].Name() != first || dirs[1].Name() != second {
							t.Fatalf("downloaded directories = %v, %v", dirs, err)
						}
						checkDownloadedFiles(t, filepath.Join(dest, firstDir), []string{"left.txt"}, []string{"x"})
						checkDownloadedFiles(t, filepath.Join(dest, secondDir), []string{"right.txt"}, []string{"xx"})
					}
					checkDownloadCleanup(t, parent, fake)
				})
			}
		})
	}
}

func TestDownloadArchiveRepeatedDirectoryName(t *testing.T) {
	dir := t.TempDir()
	extractor := archiveExtractor{fileLimit: 100, totalLimit: 1000, link: (*os.Root).Link}
	_, err := extractor.extractArchive(t.Context(), tarOf(t,
		&tar.Header{Typeflag: tar.TypeReg, Name: "./Reports/left.txt", Mode: 0o644, Size: 1},
		&tar.Header{Typeflag: tar.TypeDir, Name: "Reports/", Mode: 0o755},
		&tar.Header{Typeflag: tar.TypeReg, Name: "Reports/right.txt", Mode: 0o644, Size: 2},
	), dir)
	if err != nil {
		t.Fatal(err)
	}
	checkDownloadedFiles(t, filepath.Join(dir, "Reports"), []string{"left.txt", "right.txt"}, []string{"x", "xx"})
}

func checkDownloadCleanup(t *testing.T, parent string, fake *scriptedDownloadSession) {
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
