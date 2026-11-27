// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"archive/tar"
	"bytes"
	"context"
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

func TestDownloadDestinationHardLinkCaseCollision(t *testing.T) {
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

					checkDownloadCleanup(t, parent, fake)
				})
			}
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

func TestDownloadArchiveCollisionBelowAliasedDirectory(t *testing.T) {
	const first, second = "./Reports/output.txt", "./reports/output.txt"
	const firstDir, secondDir = "Reports", "reports"
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			parent := t.TempDir()
			caseSensitive := caseSensitiveDir(t, parent)
			dest, result, err := downloadCaseArchive(t, parent, existing,
				&tar.Header{Name: first, Typeflag: tar.TypeReg, Mode: 0o644, Size: 1},
				&tar.Header{Name: second, Typeflag: tar.TypeReg, Mode: 0o644, Size: 2},
			)
			if !caseSensitive {
				checkArchiveCollision(t, dest, result, err, existing, first, second)
				return
			}
			if err != nil || result == nil || !result.Directory || result.Files != 2 || result.Bytes != 3 {
				t.Fatalf("case-distinct download = %+v, %v", result, err)
			}
			checkDownloadedFiles(t, filepath.Join(dest, firstDir), []string{"output.txt"}, []string{"x"})
			checkDownloadedFiles(t, filepath.Join(dest, secondDir), []string{"output.txt"}, []string{"xx"})
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
	checkDownloadCleanup(t, parent, fake)
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
