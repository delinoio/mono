// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newRelease refuses a prefix that is not a well-formed https URL with a
// host, or that carries a query or fragment, before the tool writes a
// manifest: the asset name is appended to the prefix, so a query would hide
// the name and a hostless URL cannot be downloaded.
func TestNewReleaseRejectsMalformedURLPrefixes(t *testing.T) {
	bad := []string{
		"",
		"https://",
		"https:///releases",
		"https:///",
		"https://:443/releases",
		"https://user@/releases",
		"http://example.test/releases",
		"example.test/releases",
		"https:example.test/releases",
		"https://example .test/releases",
		"https://example.test/re leases",
		"https://example.test/%zz",
		"https://example.test:port/releases",
		"https://example.test/releases?channel=stable",
		"https://example.test/releases/?channel=stable",
		"https://example.test/releases?",
		"https://example.test/releases/?",
		"https://example.test/releases#frag",
		"https://example.test/releases#",
		"https://example.test/releases/#",
		"https://example.test?",
		"https://example.test#",
	}
	for _, prefix := range bad {
		f := newFixture(t)
		art := f.artifacts(t, "a", "x", PlatformLinuxAmd64)
		out := filepath.Join(f.dir, "out.json")
		err := f.run(t, "add-release", "--version", "1.2.3", "--binary", "ownsand-agent", "--platforms", "linux-amd64",
			"--artifacts", art, "--url-prefix", prefix, "--out", out)
		if err == nil {
			t.Errorf("url prefix %q accepted", prefix)
		}
		if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
			t.Errorf("a rejected url prefix %q wrote %s", prefix, out)
		}
	}
}

func TestNewReleaseKeepsValidURLPrefixes(t *testing.T) {
	cases := map[string]string{
		"https://example.test/releases":                                             "https://example.test/releases/",
		"https://example.test/releases/":                                            "https://example.test/releases/",
		"https://example.test":                                                      "https://example.test/",
		"https://example.test:8443/releases":                                        "https://example.test:8443/releases/",
		"https://github.com/delinoio/mono/releases/download/ownsand-agent%401.2.3":  "https://github.com/delinoio/mono/releases/download/ownsand-agent%401.2.3/",
		"https://github.com/delinoio/mono/releases/download/ownsand-agent%401.2.3/": "https://github.com/delinoio/mono/releases/download/ownsand-agent%401.2.3/",
	}
	for prefix, wantBase := range cases {
		f := newFixture(t)
		art := f.artifacts(t, "a", "x", PlatformLinuxAmd64)
		r, err := newRelease(releaseInput{
			Version:     "1.2.3",
			Binary:      "ownsand-agent",
			Platforms:   []Platform{PlatformLinuxAmd64},
			ArtifactDir: art,
			URLPrefix:   prefix,
		})
		if err != nil {
			t.Errorf("url prefix %q rejected: %v", prefix, err)
			continue
		}
		want := wantBase + assetName("ownsand-agent", PlatformLinuxAmd64)
		if len(r.Artifacts) != 1 || r.Artifacts[0].URL != want {
			t.Errorf("url prefix %q gave %+v, want URL %q", prefix, r.Artifacts, want)
		}
	}
}

// decodeManifest, which `sign` runs before it signs, refuses an artifact URL
// that is hostless or malformed, and still accepts a complete URL with a
// query.
func TestDecodeManifestChecksArtifactURLs(t *testing.T) {
	manifestWith := func(url string) []byte {
		return []byte(`{"schema":1,"releases":[{"version":"1.2.3","default":true,"artifacts":[{"platform":"linux-amd64","url":"` + url + `","sha256":"` + strings.Repeat("a", 64) + `"}]}],"revoked_versions":[]}`)
	}
	bad := []string{
		"",
		"https://",
		"https:///a",
		"https://:443/a",
		"https://user@/a",
		"http://example.test/a",
		"https:example.test/a",
		"https://example .test/a",
		"https://example.test/%zz",
		"https://example.test:port/a",
	}
	for _, u := range bad {
		if _, err := decodeManifest(manifestWith(u)); err == nil {
			t.Errorf("decodeManifest accepted artifact URL %q", u)
		}
	}
	good := []string{
		"https://example.test/a",
		"https://example.test/ownsand-agent%401.2.3/ownsand-agent-linux-amd64",
		"https://example.test/a?token=1&x=y",
		"https://example.test:8443/a?",
	}
	for _, u := range good {
		if _, err := decodeManifest(manifestWith(u)); err != nil {
			t.Errorf("decodeManifest rejected artifact URL %q: %v", u, err)
		}
	}
}

// `sign` must not emit a signature for a manifest with a hostless artifact
// URL.
func TestSignRefusesHostlessArtifactURL(t *testing.T) {
	f := newFixture(t)
	manifestPath := filepath.Join(f.dir, "m.json")
	body := `{"schema":1,"releases":[{"version":"1.2.3","default":true,"artifacts":[{"platform":"linux-amd64","url":"https:///a","sha256":"` + strings.Repeat("a", 64) + `"}]}],"revoked_versions":[]}`
	if err := os.WriteFile(manifestPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	sigPath := manifestPath + ".sig"
	if err := f.run(t, "sign", "--manifest", manifestPath, "--out", sigPath); err == nil {
		t.Fatal("sign accepted a hostless artifact URL")
	}
	if _, err := os.Stat(sigPath); !os.IsNotExist(err) {
		t.Fatalf("a rejected manifest got a signature file at %s", sigPath)
	}
}
