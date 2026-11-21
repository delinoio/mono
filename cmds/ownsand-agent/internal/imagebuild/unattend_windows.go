// SPDX-License-Identifier: Apache-2.0

package imagebuild

import (
	"bytes"
	"crypto/rand"
	"encoding/xml"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"text/template"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/hcsrt"
)

// Windows guest settings of the image.
const (
	// guestComputerName is the computer name of every Windows guest.
	guestComputerName = "sandbox"
	// guestLocalAccount is the local administrator that OOBE creates.
	// Nobody signs in with it: its password is random and never stored, and
	// the guest helper runs as LocalSystem. Windows 11 OOBE does not finish
	// without a local account.
	guestLocalAccount = "ownsand"
)

// passwordAlphabet has no XML-special characters, so the password needs no
// escaping, and covers the four character classes of the Windows password
// complexity rule.
const (
	passwordLower   = "abcdefghijkmnopqrstuvwxyz"
	passwordUpper   = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	passwordDigits  = "23456789"
	passwordSymbols = "!#%+-.=@^_~"
	passwordLength  = 24
)

// randomPassword returns a random password with every character class.
func randomPassword() (string, error) {
	classes := []string{passwordLower, passwordUpper, passwordDigits, passwordSymbols}
	all := strings.Join(classes, "")
	b := make([]byte, passwordLength)
	pick := func(set string) (byte, error) {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		if err != nil {
			return 0, err
		}
		return set[n.Int64()], nil
	}
	for i := range b {
		set := all
		if i < len(classes) {
			set = classes[i]
		}
		c, err := pick(set)
		if err != nil {
			return "", err
		}
		b[i] = c
	}
	// Shuffle, so the class characters are not always first.
	for i := len(b) - 1; i > 0; i-- {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return "", err
		}
		j := n.Int64()
		b[i], b[j] = b[j], b[i]
	}
	return string(b), nil
}

// unattendSettings are the inputs of the answer file.
type unattendSettings struct {
	// Language is the default language of the image (for example en-US).
	Language string
	// Password is the password of the built-in Administrator and of the
	// local account. It is never logged or stored outside the answer file,
	// which SetupComplete.cmd and the build delete.
	Password string
}

// unattendTemplate completes the specialize and oobeSystem passes of an
// offline-applied image without interaction: computer name, UTC time zone,
// the language settings (skips the region and keyboard pages), the EULA,
// online account, wireless and privacy pages, and the accounts.
var unattendTemplate = template.Must(template.New("unattend").Parse(`<?xml version="1.0" encoding="utf-8"?>
<unattend xmlns="urn:schemas-microsoft-com:unattend" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
  <settings pass="specialize">
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <ComputerName>{{.ComputerName}}</ComputerName>
      <TimeZone>UTC</TimeZone>
    </component>
  </settings>
  <settings pass="oobeSystem">
    <component name="Microsoft-Windows-International-Core" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <InputLocale>{{.Language}}</InputLocale>
      <SystemLocale>{{.Language}}</SystemLocale>
      <UILanguage>{{.Language}}</UILanguage>
      <UserLocale>{{.Language}}</UserLocale>
    </component>
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <OOBE>
        <HideEULAPage>true</HideEULAPage>
        <HideLocalAccountScreen>true</HideLocalAccountScreen>
        <HideOEMRegistrationScreen>true</HideOEMRegistrationScreen>
        <HideOnlineAccountScreens>true</HideOnlineAccountScreens>
        <HideWirelessSetupInOOBE>true</HideWirelessSetupInOOBE>
        <ProtectYourPC>3</ProtectYourPC>
      </OOBE>
      <UserAccounts>
        <AdministratorPassword>
          <Value>{{.Password}}</Value>
          <PlainText>true</PlainText>
        </AdministratorPassword>
        <LocalAccounts>
          <LocalAccount wcm:action="add">
            <Name>{{.Account}}</Name>
            <Group>Administrators</Group>
            <Password>
              <Value>{{.Password}}</Value>
              <PlainText>true</PlainText>
            </Password>
          </LocalAccount>
        </LocalAccounts>
      </UserAccounts>
      <TimeZone>UTC</TimeZone>
    </component>
  </settings>
</unattend>
`))

// validLanguage reports a language tag such as en-US or zh-Hant-TW.
func validLanguage(s string) bool {
	if s == "" || len(s) > 16 {
		return false
	}
	for _, c := range s {
		if !(c == '-' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// unattendXML returns the answer file.
func unattendXML(s unattendSettings) ([]byte, error) {
	if !validLanguage(s.Language) {
		return nil, fmt.Errorf("imagebuild: invalid image language %q", s.Language)
	}
	if s.Password == "" || strings.ContainsAny(s.Password, "<>&\"'") {
		return nil, errors.New("imagebuild: the generated password is not usable in the answer file")
	}
	var b bytes.Buffer
	err := unattendTemplate.Execute(&b, map[string]string{
		"ComputerName": guestComputerName, "Language": s.Language, "Password": s.Password, "Account": guestLocalAccount,
	})
	if err != nil {
		return nil, err
	}
	// The answer file must be well-formed XML; Setup fails late otherwise.
	if err := xml.Unmarshal(b.Bytes(), new(struct{})); err != nil {
		return nil, fmt.Errorf("imagebuild: the answer file is not valid XML: %w", err)
	}
	return b.Bytes(), nil
}

// guestServiceCommand is the binPath of the guest helper service.
func guestServiceCommand() string {
	return `"` + hcsrt.WindowsGuestBinary + `"` + " --listen " + hcsrt.WindowsGuestListen + " --grow-disk"
}

// setupCompleteCmd returns Windows\Setup\Scripts\SetupComplete.cmd. Setup
// runs it as LocalSystem after OOBE finished and before the sign-in screen,
// so the guest helper service exists only once setup is complete: the
// image build treats the first answer of the guest helper as the end of
// setup. The script registers the service (auto start, LocalSystem,
// restart on failure), starts it, deletes the answer file with its
// password and deletes itself.
func setupCompleteCmd() string {
	svc := hcsrt.WindowsGuestService
	// sc.exe binPath= takes the whole command line as one argument; the
	// inner quotes protect the space in "Program Files".
	binPath := strings.ReplaceAll(guestServiceCommand(), `"`, `\"`)
	lines := []string{
		"@echo off",
		`set LOG=%ProgramData%\ownsand-guest\setup.log`,
		`if not exist "%ProgramData%\ownsand-guest" mkdir "%ProgramData%\ownsand-guest"`,
		`echo SetupComplete start %DATE% %TIME% >> "%LOG%"`,
		`sc.exe create ` + svc + ` binPath= "` + binPath + `" start= auto obj= LocalSystem DisplayName= "OwnSand guest helper" >> "%LOG%" 2>&1`,
		`sc.exe description ` + svc + ` "Serves the OwnSand device agent over a Hyper-V socket." >> "%LOG%" 2>&1`,
		`sc.exe failure ` + svc + ` reset= 86400 actions= restart/1000/restart/2000/restart/5000 >> "%LOG%" 2>&1`,
		`sc.exe failureflag ` + svc + ` 1 >> "%LOG%" 2>&1`,
		`sc.exe start ` + svc + ` >> "%LOG%" 2>&1`,
		`del /f /q "%WINDIR%\Panther\unattend.xml" >> "%LOG%" 2>&1`,
		`del /f /q "%WINDIR%\Panther\Unattend\unattend.xml" >> "%LOG%" 2>&1`,
		`echo SetupComplete end %DATE% %TIME% >> "%LOG%"`,
		`del /f /q "%~f0"`,
	}
	return strings.Join(lines, "\r\n") + "\r\n"
}
