// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"bytes"
	"encoding/xml"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/hcsrt"
)

func TestSelectEdition(t *testing.T) {
	list := []wimImage{
		{1, "Windows Server 2025 Standard Evaluation"},
		{2, "Windows Server 2025 Standard Evaluation (Desktop Experience)"},
		{4, "Windows Server 2025 Datacenter Evaluation (Desktop Experience)"},
	}
	for _, tc := range []struct {
		edition string
		want    int
		ok      bool
	}{
		{"", 1, true},
		{"2", 2, true},
		{" 4 ", 4, true},
		{"3", 0, false},
		{"Windows Server 2025 Datacenter Evaluation (Desktop Experience)", 4, true},
		{"windows server 2025 standard evaluation", 1, true},
		{"Windows 11 Pro", 0, false},
	} {
		got, err := selectEdition(list, tc.edition)
		if (err == nil) != tc.ok || (tc.ok && got.Index != tc.want) {
			t.Errorf("selectEdition(%q) = %+v, %v", tc.edition, got, err)
		}
		if err != nil && !strings.Contains(err.Error(), "4: Windows Server 2025 Datacenter") {
			t.Errorf("error %q does not list the editions", err)
		}
	}
}

func TestParseImageList(t *testing.T) {
	one, err := parseImageList([]byte(`{"ImageIndex":1,"ImageName":"Windows 11 Pro"}` + "\r\n"))
	if err != nil || len(one) != 1 || one[0].Name != "Windows 11 Pro" {
		t.Fatalf("single image %+v %v", one, err)
	}
	two, err := parseImageList([]byte(`[{"ImageIndex":1,"ImageName":"A"},{"ImageIndex":2,"ImageName":"B"}]`))
	if err != nil || len(two) != 2 || two[1].Index != 2 {
		t.Fatalf("two images %+v %v", two, err)
	}
	if _, err := parseImageList([]byte(`[]`)); err == nil {
		t.Fatal("empty list accepted")
	}
}

func TestGuestMinimums(t *testing.T) {
	for typ, want := range map[string][2]int64{"Client": {2, 4096}, "Server": {1, 2048}, "Server Core": {1, 1024}} {
		cpus, mem := guestMinimums(typ)
		if int64(cpus) != want[0] || mem != want[1] {
			t.Errorf("%s: %d vCPUs %d MiB", typ, cpus, mem)
		}
	}
}

func TestRandomPassword(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		p, err := randomPassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != passwordLength || seen[p] {
			t.Fatalf("password length %d or repeated", len(p))
		}
		seen[p] = true
		var lower, upper, digit, symbol bool
		for _, c := range p {
			switch {
			case unicode.IsLower(c):
				lower = true
			case unicode.IsUpper(c):
				upper = true
			case unicode.IsDigit(c):
				digit = true
			default:
				symbol = true
			}
		}
		if !lower || !upper || !digit || !symbol || strings.ContainsAny(p, `<>&"'`) {
			t.Fatal("password misses a character class or has an XML-special character")
		}
	}
}

func TestUnattendXML(t *testing.T) {
	const password = "Ab3#secret-test-value"
	b, err := unattendXML(unattendSettings{Language: "en-US", Password: password})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Settings []struct {
			Pass       string `xml:"pass,attr"`
			Components []struct {
				Name         string `xml:"name,attr"`
				Arch         string `xml:"processorArchitecture,attr"`
				ComputerName string `xml:"ComputerName"`
				UILanguage   string `xml:"UILanguage"`
				OOBE         struct {
					HideEULAPage             bool `xml:"HideEULAPage"`
					HideOnlineAccountScreens bool `xml:"HideOnlineAccountScreens"`
					ProtectYourPC            int  `xml:"ProtectYourPC"`
				} `xml:"OOBE"`
				AdminPassword string `xml:"UserAccounts>AdministratorPassword>Value"`
				LocalAccount  string `xml:"UserAccounts>LocalAccounts>LocalAccount>Name"`
			} `xml:"component"`
		} `xml:"settings"`
	}
	if err := xml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	passes := map[string]bool{}
	for _, s := range doc.Settings {
		passes[s.Pass] = true
		for _, c := range s.Components {
			if c.Arch != "amd64" {
				t.Errorf("component %s architecture %q", c.Name, c.Arch)
			}
			switch {
			case s.Pass == "specialize" && c.ComputerName != "sandbox":
				t.Errorf("computer name %q", c.ComputerName)
			case s.Pass == "oobeSystem" && c.Name == "Microsoft-Windows-Shell-Setup":
				if !c.OOBE.HideEULAPage || !c.OOBE.HideOnlineAccountScreens || c.OOBE.ProtectYourPC != 3 {
					t.Errorf("OOBE settings %+v", c.OOBE)
				}
				if c.AdminPassword != password || c.LocalAccount != guestLocalAccount {
					t.Error("accounts are not set")
				}
			case s.Pass == "oobeSystem" && c.Name == "Microsoft-Windows-International-Core" && c.UILanguage != "en-US":
				t.Errorf("UI language %q", c.UILanguage)
			}
		}
	}
	if !passes["specialize"] || !passes["oobeSystem"] || len(passes) != 2 {
		t.Errorf("passes %v", passes)
	}
	if _, err := unattendXML(unattendSettings{Language: "en-US<x>", Password: password}); err == nil {
		t.Error("invalid language accepted")
	}
	if _, err := unattendXML(unattendSettings{Language: "en-US", Password: "a<b"}); err == nil {
		t.Error("XML-special password accepted")
	}
}

// TestBuildLogsHoldNoPassword runs the production answer-file install step
// against a temporary directory with a synthetic password and checks that
// its log events, the generated SetupComplete.cmd and every other log line
// never carry the password. It needs no Windows image, elevation or VM.
func TestBuildLogsHoldNoPassword(t *testing.T) {
	const password = "Synthetic-Pw-9f3a1c7e!"
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	root := t.TempDir()
	guest := bytes.Repeat([]byte{0xAB}, 1234)
	guestPath := filepath.Join(t.TempDir(), "ownsand-guest.exe")
	if err := os.WriteFile(guestPath, guest, 0o600); err != nil {
		t.Fatal(err)
	}
	newPassword := func() (string, error) { return password, nil }
	if err := installSetupFiles(logger, root, guestPath, "en-US", newPassword); err != nil {
		t.Fatal(err)
	}
	logs := buf.String()
	if !strings.Contains(logs, "answer file, SetupComplete.cmd and guest helper installed") ||
		!strings.Contains(logs, "guest_binary_bytes=1234") {
		t.Errorf("install event missing or wrong guest size: %s", logs)
	}
	if strings.Contains(logs, password) {
		t.Errorf("log holds the password: %s", logs)
	}
	script, err := os.ReadFile(filepath.Join(root, "Windows", "Setup", "Scripts", "SetupComplete.cmd"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(script), password) {
		t.Error("SetupComplete.cmd holds the password")
	}
	if string(script) != setupCompleteCmd() {
		t.Errorf("SetupComplete.cmd differs from setupCompleteCmd:\n%s", script)
	}
	answer, err := os.ReadFile(filepath.Join(root, "Windows", "Panther", "unattend.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(answer), password) {
		t.Error("unattend.xml lacks the synthetic password")
	}
	installed, err := os.ReadFile(filepath.Join(root, hcsrt.WindowsGuestBinary[len(`C:\`):]))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(installed, guest) {
		t.Error("guest helper copy differs from the source")
	}
}

func TestSetupCompleteCmd(t *testing.T) {
	s := setupCompleteCmd()
	for _, want := range []string{
		`sc.exe create ownsand-guest binPath= "\"C:\Program Files\OwnSand\ownsand-guest.exe\" --listen hvsock://00000400-facb-11e6-bd58-64006a7986d3 --grow-disk" start= auto obj= LocalSystem`,
		"sc.exe failure ownsand-guest reset= 86400 actions= restart/",
		"sc.exe failureflag ownsand-guest 1",
		"sc.exe start ownsand-guest",
		`del /f /q "%WINDIR%\Panther\unattend.xml"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("SetupComplete.cmd lacks %q:\n%s", want, s)
		}
	}
	if !strings.Contains(s, "\r\n") {
		t.Error("SetupComplete.cmd needs CRLF line ends")
	}
}

func TestDiskpartScript(t *testing.T) {
	s := diskpartScript(3, "S", "W")
	i, j := strings.Index(s, "create partition efi"), strings.Index(s, "create partition primary")
	if !strings.HasPrefix(s, "select disk 3\r\n") || i < 0 || j < i || strings.Contains(s[j:], "create partition") && strings.Count(s[j:], "create partition") > 1 {
		t.Fatalf("script:\n%s", s)
	}
	if !strings.Contains(s, "convert gpt") || !strings.Contains(s, "assign letter=W") {
		t.Fatalf("script:\n%s", s)
	}
}
