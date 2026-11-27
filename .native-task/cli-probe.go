package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

type savedCredentials struct {
	SessionID    string `json:"session_id"`
	RefreshToken string `json:"refresh_token"`
	AccessToken  string `json:"access_token"`
}

func run(name, binDir, fixtureDir string) error {
	var apiCalls atomic.Int32
	issuer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oidc/device/auth":
			fmt.Fprint(w, `{"device_code":"fixture-device","user_code":"TEST-CODE","verification_uri":"http://localhost/fixture","expires_in":60,"interval":0}`)
		case "/oidc/token":
			switch {
			case r.FormValue("grant_type") == "urn:ietf:params:oauth:grant-type:device_code":
				fmt.Fprint(w, `{"access_token":"fixture-device-access","refresh_token":"fixture-device-refresh","expires_in":3600}`)
			case r.FormValue("refresh_token") == "fixture-device-refresh":
				fmt.Fprint(w, `{"access_token":"fixture-expiring-access","refresh_token":"fixture-expiring-refresh","expires_in":1}`)
			case r.FormValue("refresh_token") == "fixture-expiring-refresh":
				fmt.Fprint(w, `{"access_token":"fixture-current-access","refresh_token":"fixture-current-refresh","expires_in":3600}`)
			default:
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"invalid_grant"}`)
			}
		case "/delino." + name + ".v1.OrganizationService/ListOrganizations":
			if r.Header.Get("Authorization") != "Bearer fixture-current-access" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			apiCalls.Add(1)
			w.Header().Set("Content-Type", "application/proto")
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	issuer.Config.Protocols = new(http.Protocols)
	issuer.Config.Protocols.SetHTTP1(true)
	issuer.Config.Protocols.SetUnencryptedHTTP2(true)
	issuer.Start()
	defer issuer.Close()

	binary := filepath.Join(binDir, name)
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		return err
	}
	dir := filepath.Join(fixtureDir, name)
	prefix := strings.ToUpper(name) + "_"
	env := make([]string, 0, len(os.Environ())+5)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(value), prefix) {
			env = append(env, value)
		}
	}
	env = append(env,
		prefix+"CONFIG_DIR="+dir,
		prefix+"API_URL="+issuer.URL,
		prefix+"LOGTO_ENDPOINT="+issuer.URL,
		prefix+"LOGTO_APP_ID=fixture-app",
	)
	cli := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if strings.Contains(string(out), "-refresh") || strings.Contains(string(out), "-access") {
			return "", errors.New("CLI output contained a synthetic token")
		}
		return string(out), err
	}

	path := filepath.Join(dir, "credentials.json")
	read := func() (savedCredentials, error) {
		var stored savedCredentials
		b, err := os.ReadFile(path)
		if err != nil {
			return stored, err
		}
		err = json.Unmarshal(b, &stored)
		return stored, err
	}
	if _, err := cli("login"); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	before, err := read()
	if err != nil || before.SessionID == "" || before.RefreshToken != "fixture-expiring-refresh" || before.AccessToken != "fixture-expiring-access" {
		return errors.New("login did not save the complete initial credentials")
	}
	if out, err := cli("org", "list"); err != nil {
		return fmt.Errorf("org list: %w: %s", err, strings.TrimSpace(out))
	}
	after, err := read()
	if err != nil || after.SessionID != before.SessionID || after.RefreshToken != "fixture-current-refresh" || after.AccessToken != "fixture-current-access" || apiCalls.Load() != 1 {
		return errors.New("authenticated request did not publish and use the complete rotation")
	}
	if runtime.GOOS != "windows" {
		for p, want := range map[string]os.FileMode{path: 0o600, dir: 0o700} {
			got, err := os.Stat(p)
			if err != nil || got.Mode().Perm() != want {
				return errors.New("credential file or directory permissions are incorrect")
			}
		}
	}
	if _, err := cli("logout"); err != nil {
		return fmt.Errorf("logout: %w", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return errors.New("logout did not remove the credential file")
	}
	if out, err := cli("org", "list"); err == nil || !strings.Contains(out, "login") || apiCalls.Load() != 1 {
		return errors.New("signed-out request did not stop with login guidance")
	}
	fmt.Printf("%s: login, complete refresh rotation, authenticated request, logout and signed-out request passed\n", name)
	return nil
}

func probe(binDir string) error {
	fixtureDir, err := os.MkdirTemp(filepath.Dir(binDir), "cli-fixture-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(fixtureDir)
	for _, cli := range []string{"ownsand", "sandrun"} {
		if err := run(cli, binDir, fixtureDir); err != nil {
			return fmt.Errorf("%s: %w", cli, err)
		}
	}
	return nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: cli-probe <binary-directory>")
		os.Exit(2)
	}
	if err := probe(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
