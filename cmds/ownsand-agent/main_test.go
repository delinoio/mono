// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
)

func TestCLIEnrollStatusReset(t *testing.T) {
	cp := testcp.New(t)
	dir := t.TempDir()
	t.Setenv("OWNSAND_AGENT_STATE_DIR", dir)
	// The process runtime makes reset skip the host network cleanup, so that
	// the test never deletes tap devices or the nftables table of the machine
	// that runs it, whatever runtime the parent environment selects.
	t.Setenv("OWNSAND_AGENT_RUNTIME", "process")
	t.Setenv("NO_COLOR", "1")
	var out, errb bytes.Buffer
	if code := run([]string{"status"}, nil, &out, &errb); code != exitNotEnrolled {
		t.Fatalf("status before enroll: %d", code)
	}
	out.Reset()
	if code := run([]string{"enroll", "--api-url", cp.URL}, strings.NewReader(testcp.Token+"\n"), &out, &errb); code != exitOK {
		t.Fatalf("enroll: %d %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "Enrolled device") || strings.Contains(out.String()+errb.String(), testcp.Token) {
		t.Fatalf("enroll output %q / %q", out.String(), errb.String())
	}
	out.Reset()
	if code := run([]string{"status", "--no-color"}, nil, &out, &errb); code != exitOK || !strings.Contains(out.String(), "Enrollment: enrolled") || !strings.Contains(out.String(), cp.URL) {
		t.Fatalf("status: %d %q", code, out.String())
	}
	if code := run([]string{"reset"}, nil, &out, &errb); code != exitUsage {
		t.Fatalf("reset without --yes: %d", code)
	}
	out.Reset()
	if code := run([]string{"reset", "--yes"}, nil, &out, &errb); code != exitOK {
		t.Fatalf("reset: %d", code)
	}
	if code := run([]string{"status"}, nil, &out, &errb); code != exitNotEnrolled {
		t.Fatalf("status after reset: %d", code)
	}
	if code := run([]string{"enroll", "--api-url", cp.URL}, strings.NewReader(""), &out, &errb); code != exitUsage {
		t.Fatalf("enroll without token: %d", code)
	}
}

func TestCLIMisc(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, nil, &out, &errb); code != exitOK || out.Len() == 0 {
		t.Fatal("version")
	}
	out.Reset()
	if code := run([]string{"service", "print"}, nil, &out, &errb); code != exitOK || !strings.Contains(out.String(), "run") {
		t.Fatalf("service print: %q", out.String())
	}
	if code := run(nil, nil, &out, &errb); code != exitUsage {
		t.Fatal("no command")
	}
	// Linux base images are not built by the agent, and an unknown guest
	// OS is a usage error; neither touches the state directory.
	for _, os := range []string{"linux", "plan9"} {
		if code := run([]string{"image", "build", "--os", os}, nil, &out, &errb); code != exitUsage {
			t.Fatalf("image build --os %s: %d", os, code)
		}
	}
	t.Setenv("OWNSAND_AGENT_STATE_DIR", t.TempDir())
	if code := run([]string{"run"}, nil, &out, &errb); code != exitNotEnrolled {
		t.Fatalf("run without enrollment: %d", code)
	}
	// Under launchd (hidden marker flag) a missing enrollment exits 0, so
	// that KeepAlive SuccessfulExit=false does not restart the agent; the
	// public exit code of plain `run` stays 3 (checked above).
	if code := run([]string{"run", "--service-manager", "launchd"}, nil, &out, &errb); code != exitOK {
		t.Fatalf("launchd run without enrollment: %d", code)
	}
	if code := run([]string{"run", "--service-manager", "bogus"}, nil, &out, &errb); code != exitUsage {
		t.Fatalf("unknown service manager: %d", code)
	}
}
