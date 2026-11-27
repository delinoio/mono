package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const buildPackage = "cmds/ownsand-agent/internal/imagebuild"
const generatorPath = buildPackage + "/windisk_windows.go"
const fixturePath = buildPackage + "/windows_windows_test.go"
const baselineSHA = "b0f282e2c1d64b37f7ab57d59574f3a0202853cf"

func main() {
	log.SetFlags(0)
	generator := read(generatorPath)
	fixture := read(fixturePath)
	baseline := len(os.Args) == 2 && os.Args[1] == "-baseline"
	if baseline {
		if runtime.GOOS != "linux" {
			log.Fatal("The baseline comparison uses the Linux extraction")
		}
		out, err := exec.Command("git", "show", baselineSHA+":"+fixturePath).CombinedOutput()
		if err != nil {
			log.Fatalf("Read baseline fixture: %v\n%s", err, out)
		}
		fixture = string(out)
	}

	scratch, err := os.MkdirTemp(filepath.Join(".t3-tmp", "diskpart-layout"), "probe-")
	must(err)
	scratch, err = filepath.Abs(scratch)
	must(err)
	fmt.Printf("Probe logs: %s\n", scratch)

	const signature = "func diskpartScript(number int, system, win string) string {"
	if strings.Count(generator, signature) != 1 {
		log.Fatal("Expected one diskpartScript definition")
	}
	const efi = "create partition efi size=260\r\n"
	const msr = "create partition msr size=16\r\n"
	const primary = "create partition primary\r\n"
	const fat32 = "format quick fs=fat32 label=\"System\"\r\n"
	const ntfs = "format quick fs=ntfs label=\"Windows\"\r\n"
	for _, scenario := range []struct {
		name, old, replacement string
		baselineRejects        bool
	}{
		{name: "original"},
		{name: "missing-msr", old: msr},
		{name: "msr-size", old: msr, replacement: "create partition msr size=32\r\n"},
		{name: "efi-size", old: efi, replacement: "create partition efi size=100\r\n"},
		{name: "msr-before-efi", old: efi + fat32 + "assign letter=S\r\n" + msr, replacement: msr + efi + fat32 + "assign letter=S\r\n"},
		{name: "missing-fat32", old: fat32},
		{name: "formats-swapped", old: fat32 + "assign letter=S\r\n" + msr + primary + ntfs, replacement: ntfs + "assign letter=S\r\n" + msr + primary + fat32},
		{name: "wrong-primary-filesystem", old: ntfs, replacement: "format quick fs=fat32 label=\"Windows\"\r\n"},
		{name: "extra-recovery", old: "exit\r\n", replacement: "create partition primary size=1024\r\nexit\r\n", baselineRejects: true},
	} {
		source := generator
		if scenario.old != "" {
			wrapper := "\n" + signature + "\n" +
				"script := originalDiskpartScript(number, system, win)\n" +
				fmt.Sprintf("if strings.Count(script, %q) != 1 { panic(%q) }\n", scenario.old, "The mutation must replace exactly one command sequence") +
				fmt.Sprintf("return strings.Replace(script, %q, %q, 1)\n}\n", scenario.old, scenario.replacement)
			source = strings.Replace(source, signature, strings.Replace(signature, "diskpartScript", "originalDiskpartScript", 1), 1) + wrapper
		}

		args := testArgs(scratch, source, fixture)
		output, err := exec.Command("go", args...).CombinedOutput()
		must(os.WriteFile(filepath.Join(scratch, scenario.name+".log"), output, 0o600))
		reject := scenario.old != "" && (!baseline || scenario.baselineRejects)
		if reject {
			diagnostic := "diskpartScript commands:"
			if baseline {
				diagnostic = "script:"
			}
			if err == nil || !strings.Contains(string(output), "--- FAIL: TestDiskpartScript") ||
				!strings.Contains(string(output), diagnostic) {
				log.Fatalf("%s: expected a layout assertion failure, got %v\n%s", scenario.name, err, output)
			}
			fmt.Printf("%s: fixture rejected script\n", scenario.name)
			continue
		}
		if err != nil || !strings.Contains(string(output), "--- PASS: TestDiskpartScript") {
			log.Fatalf("%s: expected fixture to pass, got %v\n%s", scenario.name, err, output)
		}
		fmt.Printf("%s: fixture accepted script\n", scenario.name)
	}
}

func testArgs(scratch, source, fixture string) []string {
	args := []string{"test", "-v", "-count=1", "-run", "^TestDiskpartScript$"}
	replacement := filepath.Join(scratch, "diskpart.go")
	if runtime.GOOS == "windows" {
		original, err := filepath.Abs(generatorPath)
		must(err)
		overlay, err := json.Marshal(struct{ Replace map[string]string }{
			Replace: map[string]string{original: replacement},
		})
		must(err)
		overlayPath := filepath.Join(scratch, "overlay.json")
		must(os.WriteFile(overlayPath, overlay, 0o600))
		must(os.WriteFile(replacement, []byte(source), 0o600))
		return append(args, "-overlay", overlayPath, "./"+buildPackage)
	}

	fixtureFile := filepath.Join(scratch, "diskpart_test.go")
	must(os.WriteFile(replacement, []byte("package imagebuild\nimport (\"fmt\"; \"strings\")\n"+
		extract(source, "efiPartitionMiB", "originalDiskpartScript", "diskpartScript")), 0o600))
	imports := "\"strings\"; \"testing\""
	if strings.Contains(fixture, "slices.Equal") {
		imports += "; \"slices\""
	}
	must(os.WriteFile(fixtureFile, []byte("package imagebuild\nimport ("+imports+")\n"+
		extract(fixture, "TestDiskpartScript")), 0o600))
	return append(args, replacement, fixtureFile)
}

func extract(source string, names ...string) string {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "", source, 0)
	must(err)
	var declarations strings.Builder
	for _, node := range file.Decls {
		var name string
		switch node := node.(type) {
		case *ast.FuncDecl:
			name = node.Name.Name
		case *ast.GenDecl:
			if node.Tok == token.CONST {
				name = node.Specs[0].(*ast.ValueSpec).Names[0].Name
			}
		}
		for _, needed := range names {
			if name == needed {
				declarations.WriteString(source[fileSet.Position(node.Pos()).Offset:fileSet.Position(node.End()).Offset])
				declarations.WriteByte('\n')
			}
		}
	}
	return declarations.String()
}

func read(path string) string {
	data, err := os.ReadFile(path)
	must(err)
	return string(data)
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
