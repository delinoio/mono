// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
)

type archiveReadFunc func([]byte) (int, error)

func (read archiveReadFunc) Read(p []byte) (int, error) { return read(p) }

func TestExtractStopsDuringPayloadRead(t *testing.T) {
	for _, name := range []string{"cancel", "deadline"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const fileSize, prefixSize, headerSize = 32 << 10, 4 << 10, 512
				archive := tarOf(t, &tar.Header{
					Typeflag: tar.TypeReg, Name: "file", Size: fileSize, Mode: 0o644, Format: tar.FormatUSTAR,
				})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				stop := cancel
				wantErr := context.Canceled
				if name == "deadline" {
					deadlineCtx, cancelDeadline := context.WithTimeout(ctx, time.Second)
					defer cancelDeadline()
					ctx = deadlineCtx
					stop = func() { <-ctx.Done() }
					wantErr = context.DeadlineExceeded
				}
				payloadReads := 0
				offset := 0
				reader := archiveReadFunc(func(p []byte) (int, error) {
					if offset >= headerSize {
						payloadReads++
						if payloadReads == 1 {
							p = p[:min(len(p), prefixSize)]
							n, err := archive.Read(p)
							stop()
							return n, err
						}
					}
					n, err := archive.Read(p)
					offset += n
					return n, err
				})
				dir := t.TempDir()
				extractor := archiveExtractor{fileLimit: fileSize, totalLimit: fileSize}
				st, err := extractor.extractArchive(ctx, reader, dir)
				if !errors.Is(err, wantErr) {
					t.Errorf("extraction error = %v, want %v", err, wantErr)
				}
				if payloadReads != 1 {
					t.Errorf("payload reads = %d, want 1", payloadReads)
				}
				if fi, err := os.Stat(filepath.Join(dir, "file")); err != nil || fi.Size() != 0 {
					t.Errorf("staged file = %v, %v, want an empty file", fi, err)
				}
				if st.files != 0 || st.bytes != 0 {
					t.Errorf("partial entry counted in stats: %+v", st)
				}
			})
		})
	}
}

func TestExtractRejectsTruncatedPayload(t *testing.T) {
	archive := tarOf(t, &tar.Header{Typeflag: tar.TypeReg, Name: "file", Size: 4, Mode: 0o644, Format: tar.FormatUSTAR})
	extractor := archiveExtractor{fileLimit: 4, totalLimit: 4}
	st, err := extractor.extractArchive(t.Context(), io.LimitReader(archive, 512+2), t.TempDir())
	if !errors.Is(err, io.ErrUnexpectedEOF) || st.files != 0 || st.bytes != 0 {
		t.Fatalf("truncated entry gave stats %+v, error %v", st, err)
	}
}

func TestExtractSkipsCanceledHardLinkFallback(t *testing.T) {
	archive := tarOf(t,
		&tar.Header{Typeflag: tar.TypeReg, Name: "source", Size: 4, Mode: 0o644},
		&tar.Header{Typeflag: tar.TypeLink, Name: "copy", Linkname: "source"},
	)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	extractor := archiveExtractor{fileLimit: 4, totalLimit: 8, link: func(*os.Root, string, string) error {
		cancel()
		return errors.ErrUnsupported
	}}
	dir := t.TempDir()
	st, err := extractor.extractArchive(ctx, archive, dir)
	if !errors.Is(err, context.Canceled) || st.files != 1 || st.bytes != 4 {
		t.Errorf("canceled fallback gave stats %+v, error %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "copy")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("canceled fallback path stat = %v, want not exist", err)
	}
}

func TestDownloadCleansUpCanceledHardLinkFallback(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, sandrunv1.GuestOs_GUEST_OS_LINUX)
	guest := filepath.Join(ds.GetWorkspacePath(), "out")
	if err := os.Mkdir(guest, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(guest, "source")
	if err := os.WriteFile(source, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, filepath.Join(guest, "link")); err != nil {
		t.Skipf("guest fixture needs hard links: %v", err)
	}

	for _, name := range []string{"existing", "missing"} {
		t.Run(name, func(t *testing.T) {
			execsBefore := len(srv.ExecRequests())
			parent := t.TempDir()
			dest := filepath.Join(parent, "result")
			wantEntries := 0
			if name == "existing" {
				if err := os.WriteFile(dest, []byte("previous"), 0o644); err != nil {
					t.Fatal(err)
				}
				wantEntries = 1
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			extractor := archiveExtractor{fileLimit: DownloadFileLimit, totalLimit: DownloadTotalLimit, link: func(*os.Root, string, string) error {
				cancel()
				return errors.ErrUnsupported
			}}
			res, err := c.download(ctx, DownloadParams{
				OrganizationID: org, DevSessionID: ds.GetDevSessionId(), GuestPath: "out", LocalPath: dest,
			}, downloadLimits{file: DownloadFileLimit, total: DownloadTotalLimit}, extractor)
			if res != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("download = %+v, %v, want context.Canceled", res, err)
			}
			if files, err := os.ReadDir(parent); err != nil || len(files) != wantEntries {
				t.Errorf("destination parent entries = %v, %v, want %d entries", files, err, wantEntries)
			}
			if name == "existing" {
				if data, err := os.ReadFile(dest); err != nil || string(data) != "previous" {
					t.Errorf("destination = %q, %v, want previous contents", data, err)
				}
			}
			requests := srv.ExecRequests()[execsBefore:]
			if len(requests) != 2 {
				t.Fatalf("got %d exec requests, want a probe and a cleanup", len(requests))
			}
			archive := requests[0].GetEnv()[guestEnvDLArchive]
			cleanup := requests[1].GetEnv()
			if archive == "" || cleanup[guestEnvDLArchive] != archive || cleanup[guestEnvPath] != "" {
				t.Fatal("cleanup must target the probe archive")
			}
			if _, err := os.Stat(archive); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("guest archive stat after cleanup = %v, want not exist", err)
			}
		})
	}
}
