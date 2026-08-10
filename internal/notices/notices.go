// Package notices exposes licenses and attributions bundled with Clodex.
package notices

import _ "embed"

// thirdPartyNotices is the byte-identical embedded copy of the canonical root
// THIRD_PARTY_NOTICES.md file.
//
//go:embed THIRD_PARTY_NOTICES.md
var thirdPartyNotices string

// Text returns the complete third-party notices document.
func Text() string {
	return thirdPartyNotices
}
