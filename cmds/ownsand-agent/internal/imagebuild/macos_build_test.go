// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestMacOSImageFirstBootAfterDiskCleanup(t *testing.T) {
	const guestOS = ownsandv1.GuestOs_GUEST_OS_MACOS
	unmountErr := errors.New("unmount failed")
	for _, tc := range []struct {
		name       string
		interrupt  string
		previous   bool
		unmountErr error
		wantErr    error
	}{
		{name: "cancel during unmount", interrupt: "unmount", previous: true, wantErr: context.Canceled},
		{name: "cancel during detach", interrupt: "detach", wantErr: context.Canceled},
		{name: "deadline during detach", interrupt: "detach", previous: true, wantErr: context.DeadlineExceeded},
		{name: "unmount error takes precedence", interrupt: "unmount", unmountErr: unmountErr, wantErr: unmountErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dir := filepath.Join(t.TempDir(), images.MacOSDir)
				metadataPath := filepath.Join(dir, images.MetadataFile)
				var previousMetadata []byte
				if tc.previous {
					writeMacOSFixture(t, dir, "previous")
					if err := images.WriteMetadata(dir, images.Metadata{GuestOS: guestOS, GuestVersion: "previous"}); err != nil {
						t.Fatal(err)
					}
					var err error
					previousMetadata, err = os.ReadFile(metadataPath)
					if err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				var calls []string
				run := func(toolCtx context.Context, name string, args ...string) ([]byte, error) {
					step := args[0]
					calls = append(calls, step)
					if err := toolCtx.Err(); err != nil {
						t.Fatalf("%s started with an ended context: %v", step, err)
					}
					var wantTimeout time.Duration
					switch step {
					case "attach":
						return []byte(attachPlist), nil
					case "info":
						return []byte(emptyDiskInventory), nil
					case "mount":
						return nil, nil
					case "unmount":
						wantTimeout = unmountTimeout
					case "detach":
						wantTimeout = detachTimeout
					default:
						t.Fatalf("unexpected host command: %s %v", name, args)
					}
					if deadline, ok := toolCtx.Deadline(); !ok || time.Until(deadline) != wantTimeout {
						t.Fatalf("%s deadline = %v, want a fresh %v deadline", step, deadline, wantTimeout)
					}
					if step == tc.interrupt {
						if tc.wantErr == context.DeadlineExceeded {
							<-ctx.Done()
						} else {
							cancel()
						}
					}
					if err := toolCtx.Err(); err != nil {
						t.Fatalf("%s cleanup context ended with the build context: %v", step, err)
					}
					if step == "unmount" {
						return nil, tc.unmountErr
					}
					return nil, nil
				}
				logger := discardLogger()
				err := withMacOSBuildDir(ctx, dir, logger, run, func() ([]string, error) { return nil, nil }, func(build string) error {
					writeMacOSFixture(t, build, "staged")
					if err := withAttachedDisk(ctx, run, filepath.Join(build, images.MacOSDisk), logger, func(string) error {
						return withMountedDataVolume(ctx, run, "disk5s1", filepath.Join(build, "mnt"), func() error { return nil })
					}); err != nil {
						return err
					}
					return finishMacOSImage(ctx, dir, images.Metadata{GuestOS: guestOS}, "", func() (string, error) {
						t.Error("first boot ran after canceled disk cleanup")
						return "staged-helper", nil
					})
				})
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("build error = %v, want %v", err, tc.wantErr)
				}
				if want := []string{"attach", "mount", "unmount", "detach", "info"}; !slices.Equal(calls, want) {
					t.Errorf("host commands = %v, want %v", calls, want)
				}
				if _, err := os.Stat(images.BuildDir(dir)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("staging after build = %v, want os.ErrNotExist", err)
				}
				if tc.previous {
					assertMacOSFixture(t, dir, "previous", "previous")
					if got, err := os.ReadFile(metadataPath); err != nil || !bytes.Equal(got, previousMetadata) {
						t.Errorf("installed metadata changed: got %q, %v", got, err)
					}
				} else if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("first install after cancellation = %v, want os.ErrNotExist", err)
				}
			})
		})
	}
}

func TestMacOSImagePublication(t *testing.T) {
	for _, tt := range []struct {
		name            string
		cancelAfterBoot bool
		wantErr         error
	}{
		{name: "canceled after first boot", cancelAfterBoot: true, wantErr: context.Canceled},
		{name: "canceled before exchange", wantErr: context.Canceled},
		{name: "deadline before exchange", wantErr: context.DeadlineExceeded},
		{name: "live context"},
	} {
		t.Run(tt.name+"/replacement", func(t *testing.T) {
			testMacOSImagePublication(t, true, tt.cancelAfterBoot, tt.wantErr)
		})
		t.Run(tt.name+"/first install", func(t *testing.T) {
			testMacOSImagePublication(t, false, tt.cancelAfterBoot, tt.wantErr)
		})
	}
}

func testMacOSImagePublication(t *testing.T, previous, cancelAfterBoot bool, wantErr error) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		const guestOS = ownsandv1.GuestOs_GUEST_OS_MACOS
		const helper = "staged-helper"
		root := t.TempDir()
		dir := filepath.Join(root, images.MacOSDir)
		var previousMetadata []byte
		if previous {
			writeMacOSFixture(t, dir, "previous")
			if err := images.WriteMetadata(dir, images.Metadata{GuestOS: guestOS, GuestVersion: "previous"}); err != nil {
				t.Fatal(err)
			}
			var err error
			previousMetadata, err = os.ReadFile(filepath.Join(dir, images.MetadataFile))
			if err != nil {
				t.Fatal(err)
			}
		}
		ipsw := filepath.Join(root, "operator.ipsw")
		if err := os.WriteFile(ipsw, []byte("operator restore image"), 0o600); err != nil {
			t.Fatal(err)
		}
		// Install removes this incomplete backup as its last step before
		// checking the context and exchanging directories.
		old := images.OldDir(dir)
		if err := os.Mkdir(old, 0o700); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
		defer cancel()
		gated := publicationGateContext{Context: ctx, beforeErr: func() {
			if cancelAfterBoot || wantErr == nil || ctx.Err() != nil {
				return
			}
			if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
				return
			}
			if wantErr == context.DeadlineExceeded {
				<-ctx.Done() // Let the synctest clock reach the deadline.
			} else {
				cancel()
			}
		}}
		run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return []byte(emptyDiskInventory), nil
		}
		mountPoints := func() ([]string, error) { return nil, nil }
		err := withMacOSBuildDir(gated, dir, slog.Default(), run, mountPoints, func(build string) error {
			writeMacOSFixture(t, build, "staged")
			err := finishMacOSImage(gated, dir, images.Metadata{GuestOS: guestOS}, "", func() (string, error) {
				if cancelAfterBoot {
					cancel()
				}
				return helper, nil
			})
			if cancelAfterBoot {
				if _, err := os.Stat(filepath.Join(build, images.MetadataFile)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("metadata after canceled first boot = %v, want os.ErrNotExist", err)
				}
			}
			return err
		})
		if !errors.Is(err, wantErr) {
			t.Fatalf("build error = %v, want %v", err, wantErr)
		}
		if _, err := os.Stat(images.BuildDir(dir)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("staging after build = %v, want os.ErrNotExist", err)
		}
		if got, err := os.ReadFile(ipsw); err != nil || string(got) != "operator restore image" {
			t.Errorf("operator IPSW = %q, %v", got, err)
		}
		switch {
		case wantErr == nil:
			assertMacOSFixture(t, dir, helper, "staged")
		case previous:
			assertMacOSFixture(t, dir, "previous", "previous")
			if got, err := os.ReadFile(filepath.Join(dir, images.MetadataFile)); err != nil || !bytes.Equal(got, previousMetadata) {
				t.Errorf("installed metadata changed: got %q, %v", got, err)
			}
		default:
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("first install after cancellation = %v, want os.ErrNotExist", err)
			}
		}
	})
}

type publicationGateContext struct {
	context.Context
	beforeErr func()
}

func (c publicationGateContext) Err() error {
	c.beforeErr()
	return c.Context.Err()
}

func writeMacOSFixture(t *testing.T, dir, contents string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{images.MacOSDisk, images.MacOSAuxStorage, images.MacOSHardwareModel} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func assertMacOSFixture(t *testing.T, dir, helperVersion, diskContents string) {
	t.Helper()
	metadata, err := images.ReadMetadata(dir)
	if err != nil || metadata.GuestVersion != helperVersion {
		t.Fatalf("installed helper version = %q, %v, want %q", metadata.GuestVersion, err, helperVersion)
	}
	disk, err := os.ReadFile(filepath.Join(dir, images.MacOSDisk))
	if err != nil || string(disk) != diskContents {
		t.Fatalf("installed disk = %q, %v, want %q", disk, err, diskContents)
	}
}
