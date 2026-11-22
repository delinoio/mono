// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
)

func TestValidateName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"plain", "Acme", false},
		{"non-ASCII", "회사 ✓", false},
		{"replacement character as text", "A�Z", false},
		{"only the replacement character", "�", false},
		{"exactly 100 bytes", strings.Repeat("a", 100), false},
		{"101 bytes", strings.Repeat("a", 101), true},
		{"empty", "", true},
		{"blank", " \t ", true},
		{"C0 NUL", "A\x00Z", true},
		{"C0 US", "A\x1fZ", true},
		{"C0 newline", "A\nZ", true},
		{"DEL", "A\x7fZ", true},
		{"C1 lower bound", "A\u0080Z", true},
		{"C1 NEL", "A\u0085Z", true},
		{"C1 upper bound", "A\u009fZ", true},
		{"first character after C1", "A Z", false},
		{"malformed UTF-8", "A\xffZ", true},
		{"truncated multibyte sequence", "A\xe2\x82", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateName("name", tt.input)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("validateName(%q) = %v, want nil", tt.input, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateName(%q) = nil, want invalid_argument", tt.input)
			}
			if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
				t.Fatalf("validateName(%q) code = %v, want invalid_argument", tt.input, got)
			}
		})
	}
}
