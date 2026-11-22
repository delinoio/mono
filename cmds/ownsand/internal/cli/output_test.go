// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// TestPrintFieldsIntegersUseDecimalForm checks that text mode prints every
// integer in full decimal form. A float64 would print 1e+06 from one million
// upward.
func TestPrintFieldsIntegersUseDecimalForm(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  proto.Message
		want []string
	}{
		{"below one million", &ownsandv1.Resources{VcpuCount: 999999}, []string{"vcpu_count: 999999"}},
		{"one million", &ownsandv1.Resources{VcpuCount: 1000000}, []string{"vcpu_count: 1000000"}},
		{"negative million", &ownsandv1.Resources{VcpuCount: -1000000}, []string{"vcpu_count: -1000000"}},
		{"int32 minimum", &ownsandv1.Resources{VcpuCount: -2147483648}, []string{"vcpu_count: -2147483648"}},
		{"int32 maximum", &ownsandv1.Resources{VcpuCount: 2147483647}, []string{"vcpu_count: 2147483647"}},
		{
			"int64 stays exact",
			&ownsandv1.Resources{VcpuCount: 3, MemoryMib: 9007199254740993, DiskGib: 12},
			[]string{"vcpu_count: 3", "memory_mib: 9007199254740993", "disk_gib: 12"},
		},
		{
			"nested fields",
			&ownsandv1.Device{Limits: &ownsandv1.DeviceLimits{VcpuCount: 2147483647, VcpuOvercommitPercent: 100}, EffectiveVcpuLimit: 2147483647, EffectiveVcpuLimitCount: 2147483648},
			[]string{"effective_vcpu_limit: 2147483647", "effective_vcpu_limit_count: 2147483648", "limits.vcpu_count: 2147483647", "limits.vcpu_overcommit_percent: 100"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			a := &app{}
			if err := a.printFields(&out, tc.msg); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "e+") {
				t.Fatalf("exponent form in:\n%s", out.String())
			}
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want+"\n") {
					t.Errorf("output misses the line %q:\n%s", want, out.String())
				}
			}
		})
	}
}

// TestFlattenKeepsTextFormats checks the text layout that the number fix
// must not change: strings, bools, nested keys, key order and simple
// repeated fields, which join their elements with a comma.
func TestFlattenKeepsTextFormats(t *testing.T) {
	v := map[string]any{}
	dec := json.NewDecoder(strings.NewReader(`{"b":"text","a":{"y":true,"x":1000000},"list":[1000000,2,"s"],"objs":[{"k":1},{"k":2}]}`))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	var lines [][2]string
	flatten("", v, &lines)
	got := map[string]string{}
	for _, l := range lines {
		got[l[0]] = l[1]
	}
	for k, want := range map[string]string{
		"b": "text", "a.y": "true", "a.x": "1000000", "list": "1000000, 2, s", "objs[0].k": "1", "objs[1].k": "2",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
}

// TestProtoValueJSONIsByteIdentical checks that the generic value still
// marshals to the bytes that the former float64 decoding produced.
func TestProtoValueJSONIsByteIdentical(t *testing.T) {
	m := &ownsandv1.Device{
		DeviceId:                "d",
		Name:                    "mac",
		Limits:                  &ownsandv1.DeviceLimits{VcpuCount: 2147483647, MemoryMib: 1, DiskGib: 1, VcpuOvercommitPercent: 100},
		EffectiveVcpuLimit:      2147483647,
		EffectiveVcpuLimitCount: 9007199254740993,
		Allocation:              &ownsandv1.DeviceAllocation{VcpuCount: -1000000},
	}
	raw, err := jsonOpts.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	legacy := map[string]any{}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	if err := writeJSON(&want, legacy); err != nil {
		t.Fatal(err)
	}
	v, err := protoValue(m)
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := writeJSON(&got, v); err != nil {
		t.Fatal(err)
	}
	if got.String() != want.String() {
		t.Fatalf("JSON changed:\n got %s\nwant %s", got.String(), want.String())
	}
}

// failingWriter accepts failAfter writes and then fails every write with err.
// A negative failAfter never fails.
type failingWriter struct {
	failAfter int
	err       error
	writes    int
	buf       bytes.Buffer
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.failAfter >= 0 && w.writes >= w.failAfter {
		return 0, w.err
	}
	w.writes++
	return w.buf.Write(p)
}

var errDiskFull = errors.New("write failed: no space left on device")

// TestTextResultWriteErrorExitsWithError checks that a text-mode command exits
// with 1 and the writer error on standard error when standard output rejects
// the result, as it does in JSON mode.
func TestTextResultWriteErrorExitsWithError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(r *runner)
		args  []string
	}{
		{"get", nil, []string{"device", "get", "DEVICE"}},
		{"sandbox get", nil, []string{"sandbox", "get", "SANDBOX"}},
		{"list", nil, []string{"api-key", "list"}},
		{"sandbox list", nil, []string{"sandbox", "list"}},
		{"billing status", func(r *runner) { r.f.billing, r.f.usage = linkedAllowed(), billingUsage() }, []string{"billing", "status"}},
		{"billing status without usage", func(r *runner) { r.f.billing = unlinkedBlocked() }, []string{"billing", "status"}},
	} {
		for _, mode := range []string{"text", "json"} {
			t.Run(tc.name+" "+mode, func(t *testing.T) {
				r := newRunner(t)
				if tc.setup != nil {
					tc.setup(r)
				}
				args := []string{"--format", mode}
				for _, a := range tc.args {
					a = strings.ReplaceAll(a, "DEVICE", r.f.dev)
					args = append(args, strings.ReplaceAll(a, "SANDBOX", r.f.sb))
				}
				var stderr bytes.Buffer
				stdout := &failingWriter{failAfter: 0, err: errDiskFull}
				if code := r.runWriters(stdout, &stderr, args...); code != ExitError {
					t.Fatalf("exit %d, want %d (stderr %q)", code, ExitError, stderr.String())
				}
				if !strings.Contains(stderr.String(), errDiskFull.Error()) {
					t.Fatalf("stderr misses the writer error: %q", stderr.String())
				}
				if strings.Contains(stderr.String(), "More results") {
					t.Fatalf("the next page hint follows a failed result: %q", stderr.String())
				}
			})
		}
	}
}

// TestTextListKeepsNextPageHint checks that a successful list prints the table
// on standard output and the next page hint on standard error.
func TestTextListKeepsNextPageHint(t *testing.T) {
	r := newRunner(t)
	res := r.run("api-key", "list", "--no-color")
	if res.code != ExitOK {
		t.Fatalf("%+v", res)
	}
	if !strings.HasPrefix(res.stdout, "ID") || !strings.Contains(res.stdout, "ci") || strings.Contains(res.stdout, "More results") {
		t.Fatalf("stdout:\n%s", res.stdout)
	}
	if res.stderr != "More results: pass --page-token next\n" {
		t.Fatalf("stderr %q", res.stderr)
	}
}

// TestPrintTableReturnsEveryWriteError checks that the error of any write of
// the table reaches the caller: a row write that flushes early as well as the
// final flush.
func TestPrintTableReturnsEveryWriteError(t *testing.T) {
	headers := []string{"ID", "NAME"}
	// A row without a tab ends the column block, so the tabwriter flushes
	// inside the write of that row and writes the block before it.
	rows := [][]string{{"1", "one"}, {"only one cell"}, {"2", "two"}, {"3", "three"}}
	a := &app{}
	ok := &failingWriter{failAfter: -1}
	if err := a.printTable(ok, headers, rows); err != nil {
		t.Fatal(err)
	}
	if ok.writes < 2 {
		t.Fatalf("the table needs %d writes, want an early flush and a final flush", ok.writes)
	}
	for n := 0; n < ok.writes; n++ {
		w := &failingWriter{failAfter: n, err: errDiskFull}
		if err := a.printTable(w, headers, rows); !errors.Is(err, errDiskFull) {
			t.Errorf("fail at write %d: err %v, want %v", n+1, err, errDiskFull)
		}
	}
	// A table without a single-cell row writes only in the final flush.
	flushOnly := &failingWriter{failAfter: 0, err: errDiskFull}
	if err := a.printTable(flushOnly, headers, [][]string{{"1", "one"}}); !errors.Is(err, errDiskFull) {
		t.Errorf("flush error: %v", err)
	}
}

// TestPrintFieldsReturnsWriteError checks the error of a record write.
func TestPrintFieldsReturnsWriteError(t *testing.T) {
	a := &app{}
	w := &failingWriter{failAfter: 1, err: errDiskFull}
	if err := a.printFields(w, &ownsandv1.Resources{VcpuCount: 1, MemoryMib: 2, DiskGib: 3}); !errors.Is(err, errDiskFull) {
		t.Fatalf("err %v", err)
	}
	if got := w.buf.String(); got != "disk_gib: 3\n" {
		t.Fatalf("output %q", got)
	}
}

// TestPrintSecretWritesSecretWhenStderrFails checks that the one-time secret
// still reaches standard output when the fields on standard error fail, and
// that the failure is still reported.
func TestPrintSecretWritesSecretWhenStderrFails(t *testing.T) {
	meta := &ownsandv1.ApiKey{ApiKeyId: "id", Name: "ci"}
	for _, failAfter := range []int{0, 1} { // the first field write fails, or a later write fails
		var out bytes.Buffer
		a := &app{env: Env{Stdout: &out, Stderr: &failingWriter{failAfter: failAfter, err: errDiskFull}}}
		err := a.printSecret(meta, "api_key_secret", "osk_secret")
		if !errors.Is(err, errDiskFull) {
			t.Errorf("failAfter %d: err %v", failAfter, err)
		}
		if out.String() != "osk_secret\n" {
			t.Errorf("failAfter %d: stdout %q", failAfter, out.String())
		}
	}
	// A failing standard output is reported, and the fields stay on standard error.
	var errb bytes.Buffer
	a := &app{env: Env{Stdout: &failingWriter{failAfter: 0, err: errDiskFull}, Stderr: &errb}}
	if err := a.printSecret(meta, "api_key_secret", "osk_secret"); !errors.Is(err, errDiskFull) {
		t.Errorf("stdout failure: err %v", err)
	}
	if !strings.Contains(errb.String(), "name: ci") || strings.Contains(errb.String(), "osk_secret") {
		t.Errorf("stderr %q", errb.String())
	}
}

// TestSecretCommandWriteErrors runs a secret command with a failing stream.
func TestSecretCommandWriteErrors(t *testing.T) {
	r := newRunner(t)
	var out bytes.Buffer
	code := r.runWriters(&out, &failingWriter{failAfter: 0, err: errDiskFull}, "api-key", "create", "--name", "ci")
	if code != ExitError || out.String() != "osk_newsecretvalue\n" {
		t.Fatalf("stderr failure: exit %d, stdout %q", code, out.String())
	}
	var errb bytes.Buffer
	code = r.runWriters(&failingWriter{failAfter: 0, err: errDiskFull}, &errb, "api-key", "create", "--name", "ci")
	if code != ExitError || !strings.Contains(errb.String(), errDiskFull.Error()) || strings.Contains(errb.String(), "osk_newsecretvalue") {
		t.Fatalf("stdout failure: exit %d, stderr %q", code, errb.String())
	}
	res := r.run("api-key", "create", "--name", "ci", "--no-color")
	if res.code != ExitOK || res.stdout != "osk_newsecretvalue\n" || !strings.Contains(res.stderr, "The secret is shown once; store it now.") {
		t.Fatalf("unchanged output: %+v", res)
	}
}

// TestStickyWriterKeepsFirstError checks that later writes do nothing after
// the first error.
func TestStickyWriterKeepsFirstError(t *testing.T) {
	under := &failingWriter{failAfter: 1, err: errDiskFull}
	w := &stickyWriter{w: under}
	fmt.Fprint(w, "a")
	fmt.Fprint(w, "b")
	fmt.Fprint(w, "c")
	if !errors.Is(w.err, errDiskFull) || under.buf.String() != "a" {
		t.Fatalf("err %v, wrote %q", w.err, under.buf.String())
	}
}
