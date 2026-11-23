// SPDX-License-Identifier: FSL-1.1-ALv2

package update

import "testing"

// A malformed manifest entry must never match a valid version: before the
// shared version parser rejected empty pre-release identifiers, "1.2.3-rc."
// compared equal to "1.2.3-rc".
func TestRevokedIgnoresMalformedEntries(t *testing.T) {
	m := &Manifest{RevokedVersions: []string{"1.2.3-rc.", "1.2.3-.rc", "1.2.3-rc.01", "1.2.3-rc_1", "1.2.3-rc+1"}}
	for _, v := range []string{"1.2.3-rc", "1.2.3-rc.1", "1.2.3-rc.01", "1.2.3"} {
		if m.Revoked(v) {
			t.Errorf("Revoked(%q) matched a malformed revoked entry", v)
		}
	}
	m.RevokedVersions = append(m.RevokedVersions, "1.2.3-rc")
	if !m.Revoked("1.2.3-rc") || !m.Revoked("v1.2.3-rc") {
		t.Error("a valid revoked entry no longer matches")
	}
	if m.Revoked("1.2.3-rc.") || m.Revoked("1.2.3-rc.1") {
		t.Error("a malformed or different version matched the valid revoked entry")
	}
}

func TestArtifactIgnoresMalformedReleaseVersions(t *testing.T) {
	art := Artifact{Platform: "linux-amd64", URL: "https://example.test/a"}
	m := &Manifest{Releases: []Release{
		{Version: "1.2.3-rc.", Artifacts: []Artifact{art}},
		{Version: "1.2.3-rc.01", Artifacts: []Artifact{art}},
	}}
	for _, v := range []string{"1.2.3-rc", "1.2.3-rc.1", "1.2.3-rc.01"} {
		if _, err := m.Artifact(v, "linux-amd64"); err == nil {
			t.Errorf("Artifact(%q) matched a malformed release entry", v)
		}
	}
	m.Releases = append(m.Releases, Release{Version: "1.2.3-rc", Artifacts: []Artifact{art}})
	if _, err := m.Artifact("1.2.3-rc", "linux-amd64"); err != nil {
		t.Errorf("Artifact of a valid release failed: %v", err)
	}
}
