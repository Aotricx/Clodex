// Package translate converts validated Anthropic Messages requests into the
// concrete ChatGPT Codex Responses wire protocol.
package translate

import (
	"encoding/base64"
	"strings"
)

const (
	reasoningSignaturePrefix = "clodex:codex:v1:"
	maxReasoningIDBytes      = 4 << 10
	maxEncryptedContentBytes = 8 << 20
)

// ReasoningReplay is the opaque upstream state carried through an Anthropic
// thinking signature. ID is preserved in the signature even though stateless
// Codex requests deliberately omit response item IDs.
type ReasoningReplay struct {
	ID               string
	EncryptedContent string
}

// EncodeReasoningSignature creates the versioned signature Claude Code will
// return unchanged on the next turn. False means the upstream values are
// empty or exceed the explicit envelope bounds.
func EncodeReasoningSignature(replay ReasoningReplay) (string, bool) {
	if replay.ID == "" || len(replay.ID) > maxReasoningIDBytes ||
		replay.EncryptedContent == "" || len(replay.EncryptedContent) > maxEncryptedContentBytes {
		return "", false
	}
	encodedID := base64.RawURLEncoding.EncodeToString([]byte(replay.ID))
	return reasoningSignaturePrefix + encodedID + ":" + replay.EncryptedContent, true
}

// DecodeReasoningSignature accepts only Clodex's bounded v1 envelope.
func DecodeReasoningSignature(signature string) (ReasoningReplay, bool) {
	payload, ok := strings.CutPrefix(signature, reasoningSignaturePrefix)
	if !ok || payload == "" || len(payload) > encodedReasoningIDLimit()+1+maxEncryptedContentBytes {
		return ReasoningReplay{}, false
	}
	encodedID, encrypted, ok := strings.Cut(payload, ":")
	if !ok || encodedID == "" || len(encodedID) > encodedReasoningIDLimit() ||
		encrypted == "" || len(encrypted) > maxEncryptedContentBytes {
		return ReasoningReplay{}, false
	}
	id, err := base64.RawURLEncoding.DecodeString(encodedID)
	if err != nil || len(id) == 0 || len(id) > maxReasoningIDBytes {
		return ReasoningReplay{}, false
	}
	return ReasoningReplay{ID: string(id), EncryptedContent: encrypted}, true
}

func encodedReasoningIDLimit() int {
	return (maxReasoningIDBytes + 2) / 3 * 4
}
