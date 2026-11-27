package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func main() {
	baseline := flag.Bool("baseline", false, "expect the original fixture to accept omissions")
	flag.Parse()
	log.SetFlags(0)

	dir, err := filepath.Abs("cmds/ownsand-agent/internal/imagebuild")
	check(err)
	source := filepath.Join(dir, "unattend_windows.go")
	body, err := os.ReadFile(source)
	check(err)
	scratch, err := os.MkdirTemp(".t3-tmp", "unattend-probe-")
	check(err)
	defer os.RemoveAll(scratch)
	scratch, err = filepath.Abs(scratch)
	check(err)
	overlay := filepath.Join(scratch, "overlay.json")
	replace := map[string]string{}
	testInputs := []string{"./cmds/ownsand-agent/internal/imagebuild"}
	if runtime.GOOS == "linux" {
		// Compile the unchanged Windows generator and only the test under
		// validation. Both use portable APIs.
		fixture := filepath.Join(dir, "windows_windows_test.go")
		code, err := os.ReadFile(fixture)
		check(err)
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, fixture, code, 0)
		check(err)
		var test []byte
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "TestUnattendXML" {
				test = code[fset.Position(fn.Pos()).Offset:fset.Position(fn.End()).Offset]
			}
		}
		if len(test) == 0 {
			log.Fatal("TestUnattendXML declaration missing")
		}
		target := filepath.Join(dir, "unattend_linux_probe_test.go")
		location := filepath.Join(scratch, "unattend_linux_probe_test.go")
		prefix := []byte("package imagebuild\nimport (\n\"encoding/xml\"\n\"testing\"\n)\n")
		check(os.WriteFile(location, append(prefix, test...), 0o600))
		replace[target] = location
		testInputs = []string{source, target}
	} else if runtime.GOOS != "windows" {
		log.Fatalf("unsupported probe host %s", runtime.GOOS)
	}

	run := func(label, expectedFailure string) {
		buf, err := json.Marshal(struct{ Replace map[string]string }{replace})
		check(err)
		check(os.WriteFile(overlay, buf, 0o600))
		args := []string{"test", "-v", "-count=1", "-run", "^TestUnattendXML$", "-overlay", overlay}
		cmd := exec.Command("go", append(args, testInputs...)...)
		output, err := cmd.CombinedOutput()
		fmt.Printf("\n%s\n%s", label, output)
		if expectedFailure == "" {
			check(err)
			if !strings.Contains(string(output), "--- PASS: TestUnattendXML") {
				log.Fatal("TestUnattendXML did not run")
			}
			return
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 ||
			!strings.Contains(string(output), "--- FAIL: TestUnattendXML") ||
			!strings.Contains(string(output), expectedFailure) {
			log.Fatalf("expected fixture failure containing %q, got %v", expectedFailure, err)
		}
	}

	run("Generated answer file", "")
	for i, pair := range []struct{ pass, name string }{
		{"specialize", "Microsoft-Windows-Shell-Setup"},
		{"oobeSystem", "Microsoft-Windows-Shell-Setup"},
		{"oobeSystem", "Microsoft-Windows-International-Core"},
	} {
		openTag := fmt.Sprintf("<settings pass=%q>", pair.pass)
		before, settings, ok := strings.Cut(string(body), openTag)
		if !ok {
			log.Fatalf("missing pass %q in generator template", pair.pass)
		}
		components, after, ok := strings.Cut(settings, "</settings>")
		if !ok {
			log.Fatalf("missing closing settings tag for pass %q", pair.pass)
		}
		pattern := regexp.MustCompile(fmt.Sprintf("(?s)<component name=%q[[:space:]].*?</component>", pair.name))
		if len(pattern.FindAllStringIndex(components, -1)) != 1 {
			log.Fatalf("expected one template match for pass %q component %q", pair.pass, pair.name)
		}
		mutant := before + openTag + pattern.ReplaceAllString(components, "") + "</settings>" + after
		location := filepath.Join(scratch, fmt.Sprintf("omit-component-%d.go", i))
		check(os.WriteFile(location, []byte(mutant), 0o600))
		replace[source] = location
		diagnostic := fmt.Sprintf("pass %q is missing required component %q", pair.pass, pair.name)
		expected := diagnostic
		if *baseline {
			expected = ""
		}
		run(fmt.Sprintf("Omit %s from %s", pair.name, pair.pass), expected)
	}
}
