// SPDX-License-Identifier: FSL-1.1-ALv2

package work

import "strings"

var powerShellSingleQuotes = strings.NewReplacer("'", "''", "‘", "‘‘", "’", "’’", "‚", "‚‚", "‛", "‛‛")

// QuotePowerShell returns one single-quoted PowerShell string literal.
// PowerShell treats the four typographic single quotes as delimiters too.
func QuotePowerShell(s string) string {
	return "'" + powerShellSingleQuotes.Replace(s) + "'"
}
