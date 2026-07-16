// Package battleabort defines the canonical body bound to the authenticated
// Matchmaker -> DS allocator pre-admission allocation abort RPC.
package battleabort

import (
	"bytes"
	"encoding/binary"
	"strings"
	"unicode"

	"github.com/luyuancpp/pandora/pkg/placement"
)

const canonicalDomain = "pandora-battle-allocation-abort-v1"

type Request struct {
	MatchID     uint64
	OperationID string
	Target      placement.Target
}

func (r Request) Complete() bool {
	return r.MatchID != 0 && placement.ValidOperationID(r.OperationID) && ValidTarget(r.Target)
}

// ValidTarget is the shared canonical identity gate for both the signed abort
// body and the allocator's durable teardown/lifecycle proof. Keeping one
// validator prevents a marker with a shape that no authenticated abort could
// ever name from being accepted as terminal authority.
func ValidTarget(target placement.Target) bool {
	return target.CompleteBattle() && target.AssignmentID == "" &&
		validCanonicalField(target.PodName, 253) &&
		validCanonicalField(target.InstanceUID, 128) &&
		validCanonicalField(target.AllocationID, 128) &&
		(target.ReleaseTrack == "stable" || target.ReleaseTrack == "canary")
}

func (r Request) Canonical() []byte {
	// Every variable-width field is length-prefixed, making this encoding
	// injective even for hostile bytes. A newline join is not safe here because
	// field-boundary shifts can produce the same signed body.
	var out bytes.Buffer
	writeCanonicalString(&out, canonicalDomain)
	_ = binary.Write(&out, binary.BigEndian, r.MatchID)
	writeCanonicalString(&out, r.OperationID)
	writeCanonicalString(&out, r.Target.PodName)
	writeCanonicalString(&out, r.Target.InstanceUID)
	_ = binary.Write(&out, binary.BigEndian, r.Target.InstanceEpoch)
	writeCanonicalString(&out, r.Target.AllocationID)
	writeCanonicalString(&out, r.Target.ReleaseTrack)
	return out.Bytes()
}

func writeCanonicalString(out *bytes.Buffer, value string) {
	_ = binary.Write(out, binary.BigEndian, uint32(len(value)))
	_, _ = out.WriteString(value)
}

func validCanonicalField(value string, maxBytes int) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxBytes {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}
