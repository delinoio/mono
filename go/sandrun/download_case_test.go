// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
)

func TestDownloadDestinationCaseCollision(t *testing.T) {
	const anchor, first, second = "anchor.txt", "Report.txt", "report.txt"
	parent := t.TempDir()
	probeDir := filepath.Join(parent, "probe")
	if err := os.Mkdir(probeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(probeDir, first), []byte("r"), 0o644); err != nil {
		t.Fatal(err)
	}
	aliasContents, err := os.ReadFile(filepath.Join(probeDir, second))
	aliases := err == nil
	if aliases && string(aliasContents) != "r" || !aliases && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("filesystem case probe = %q, %v", aliasContents, err)
	}
	t.Logf("the destination volume aliases %q and %q: %t", first, second, aliases)
	linkErr := os.Link(filepath.Join(probeDir, first), filepath.Join(probeDir, "link.txt"))

	type entry struct{ name, body, target string }
	firstFile := entry{name: first, body: "r"}
	secondFile := entry{name: second, body: "s"}
	firstLink := entry{name: first, body: "a", target: anchor}
	secondLink := entry{name: second, body: "a", target: anchor}

	for _, tc := range []struct {
		name          string
		first, second entry
		forceCopy     bool
	}{
		{name: "regular-regular", first: firstFile, second: secondFile},
		{name: "regular-link", first: firstFile, second: secondLink},
		{name: "link-regular", first: firstLink, second: secondFile},
		{name: "link-link", first: firstLink, second: secondLink},
		{name: "regular-link-copy", first: firstFile, second: secondLink, forceCopy: true},
		{name: "link-copy-regular", first: firstLink, second: secondFile, forceCopy: true},
		{name: "link-copy-link-copy", first: firstLink, second: secondLink, forceCopy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.forceCopy && linkErr != nil && (tc.first.target != "" || tc.second.target != "") {
				t.Skipf("this destination volume does not support hard links: %v", linkErr)
			}

			var archive bytes.Buffer
			tw := tar.NewWriter(&archive)
			var wantBytes int64
			for _, file := range []entry{{name: anchor, body: "a"}, tc.first, tc.second} {
				body := file.body
				hdr := &tar.Header{Name: file.name, Typeflag: tar.TypeReg, Mode: 0o644}
				if file.target != "" {
					hdr.Typeflag = tar.TypeLink
					hdr.Linkname = file.target
					body = ""
				}
				hdr.Size = int64(len(body))
				if err := tw.WriteHeader(hdr); err != nil {
					t.Fatal(err)
				}
				if _, err := tw.Write([]byte(body)); err != nil {
					t.Fatal(err)
				}
				if file.target == "" || tc.forceCopy {
					wantBytes += int64(len(file.body))
				}
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			for _, existing := range []bool{false, true} {
				t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
					size := int64(archive.Len())
					fake := &scriptedDownloadSession{probe: fmt.Sprintf("dir %d", size), chunks: [][]byte{archive.Bytes()}, endSize: size}
					client := newScriptedDownloadClient(t, fake)
					dest := filepath.Join(parent, "result-"+tc.name+fmt.Sprint(existing))
					if existing {
						if err := os.Mkdir(dest, 0o755); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(dest, "keep.txt"), []byte("old"), 0o644); err != nil {
							t.Fatal(err)
						}
					}

					params := DownloadParams{OrganizationID: "org", DevSessionID: "session", GuestPath: "out", LocalPath: dest}
					download := client.Download
					if tc.forceCopy {
						extractor := archiveExtractor{fileLimit: DownloadFileLimit, totalLimit: DownloadTotalLimit, link: func(*os.Root, string, string) error {
							return errors.ErrUnsupported
						}}
						download = func(ctx context.Context, p DownloadParams) (*DownloadResult, error) {
							return client.download(ctx, p, extractor)
						}
					}
					res, err := download(t.Context(), params)
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
						if err != nil || res == nil || !res.Directory || res.Files != 3 || res.Bytes != wantBytes {
							t.Fatalf("case-distinct download = %+v, %v, want 3 files and %d bytes", res, err, wantBytes)
						}
						checkDownloadedFiles(t, dest, []string{first, anchor, second}, []string{tc.first.body, "a", tc.second.body})
					}

					if matches, err := filepath.Glob(filepath.Join(parent, ".sandrun-download-*")); err != nil || len(matches) != 0 {
						t.Fatalf("local staging remains: %v, %v", matches, err)
					}
					fake.mu.Lock()
					guestArchive := fake.path
					fake.mu.Unlock()
					if guestArchive != "" {
						t.Fatalf("guest archive remains: %s", guestArchive)
					}
				})
			}
		})
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

func TestDownloadArchiveRepeatedHardLinkName(t *testing.T) {
	dir := t.TempDir()
	extractor := archiveExtractor{fileLimit: 100, totalLimit: 1000, link: (*os.Root).Link}
	_, err := extractor.extractArchive(t.Context(), tarOf(t,
		&tar.Header{Name: "anchor", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2},
		&tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3},
		&tar.Header{Name: "./file", Typeflag: tar.TypeLink, Linkname: "anchor"},
	), dir)
	if err != nil {
		t.Fatal(err)
	}
	checkDownloadedFiles(t, dir, []string{"anchor", "file"}, []string{"xx", "xx"})
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
