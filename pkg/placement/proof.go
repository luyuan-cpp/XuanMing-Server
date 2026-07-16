package placement

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// Proof is the immutable transition statement signed by an authoritative
// producer (account creation, match durable worker, or battle terminal/leave worker).
type Proof struct {
	PlayerID        uint64
	ExpectedVersion uint64
	SourceRoute     int32
	TargetRoute     int32
	SourceMatchID   uint64
	TargetMatchID   uint64
	ProofType       int32
	ProofID         string
	OperationID     string
}

// TargetUnavailableReason is an allocator assertion about an exact PENDING
// target. A lease timeout by itself is never authority to retarget.
type TargetUnavailableReason int32

const (
	TargetUnavailableUnknown TargetUnavailableReason = iota
	TargetUnavailableInstanceTerminated
	TargetUnavailableReservationExpiredUnused
	TargetUnavailableAllocationRevoked
)

// TargetUnavailableProof authorizes one exact old-target -> new-target CAS.
// Its domain is separate from transition Proof, so signatures cannot be
// replayed across Begin and Retarget even when a route-scoped key is shared.
type TargetUnavailableProof struct {
	PlayerID               uint64
	PlacementVersion       uint64
	OperationID            string
	TargetRoute            int32
	TargetMatchID          uint64
	ExpectedTarget         Target
	ReplacementVersion     uint64
	ReplacementOperationID string
	ReplacementTarget      Target
	ProofType              int32
	Reason                 TargetUnavailableReason
	ProofID                string
}

// Source-departure proof types live outside PlacementProofType's logical
// transition namespace.  Keeping distinct numeric values and a distinct HMAC
// domain prevents a terminal/transfer signature from being replayed as proof
// that a physical PlayerController has left its source DS.
const (
	ProofHubDeparture    int32 = 101
	ProofBattleDeparture int32 = 102
)

// SourceDepartureProof confirms physical absence of the immutable source
// captured by BeginPlacementTransition.  It binds the current PENDING
// operation as well as the complete source lineage and DS identity, so an ACK
// for an old ticket, retarget, match, or same-name replacement cannot unlock a
// later Admission.
type SourceDepartureProof struct {
	PlayerID               uint64
	PlacementVersion       uint64
	OperationID            string
	TargetRoute            int32
	TargetMatchID          uint64
	SourcePlacementVersion uint64
	SourceOperationID      string
	SourceRoute            int32
	SourceMatchID          uint64
	SourceTarget           Target
	ProofType              int32
	ProofID                string
}

func (p SourceDepartureProof) canonical() string {
	return strings.Join([]string{
		"pandora-placement-source-departure-v1",
		strconv.FormatUint(p.PlayerID, 10), strconv.FormatUint(p.PlacementVersion, 10),
		p.OperationID, strconv.FormatInt(int64(p.TargetRoute), 10),
		strconv.FormatUint(p.TargetMatchID, 10),
		strconv.FormatUint(p.SourcePlacementVersion, 10), p.SourceOperationID,
		strconv.FormatInt(int64(p.SourceRoute), 10), strconv.FormatUint(p.SourceMatchID, 10),
		p.SourceTarget.PodName, p.SourceTarget.InstanceUID,
		strconv.FormatUint(uint64(p.SourceTarget.InstanceEpoch), 10),
		p.SourceTarget.AssignmentID, p.SourceTarget.AllocationID, p.SourceTarget.ReleaseTrack,
		strconv.FormatInt(int64(p.ProofType), 10), p.ProofID,
	}, "\n")
}

func (p TargetUnavailableProof) canonical() string {
	return strings.Join([]string{
		"pandora-placement-target-unavailable-v1",
		strconv.FormatUint(p.PlayerID, 10), strconv.FormatUint(p.PlacementVersion, 10),
		p.OperationID, strconv.FormatInt(int64(p.TargetRoute), 10),
		strconv.FormatUint(p.TargetMatchID, 10),
		p.ExpectedTarget.PodName, p.ExpectedTarget.InstanceUID,
		strconv.FormatUint(uint64(p.ExpectedTarget.InstanceEpoch), 10),
		p.ExpectedTarget.AssignmentID, p.ExpectedTarget.AllocationID, p.ExpectedTarget.ReleaseTrack,
		strconv.FormatUint(p.ReplacementVersion, 10), p.ReplacementOperationID,
		p.ReplacementTarget.PodName, p.ReplacementTarget.InstanceUID,
		strconv.FormatUint(uint64(p.ReplacementTarget.InstanceEpoch), 10),
		p.ReplacementTarget.AssignmentID, p.ReplacementTarget.AllocationID,
		p.ReplacementTarget.ReleaseTrack, strconv.FormatInt(int64(p.ProofType), 10),
		strconv.FormatInt(int64(p.Reason), 10), p.ProofID,
	}, "\n")
}

func (p Proof) canonical() string {
	return strings.Join([]string{
		"pandora-placement-transition-v1",
		strconv.FormatUint(p.PlayerID, 10), strconv.FormatUint(p.ExpectedVersion, 10),
		strconv.FormatInt(int64(p.SourceRoute), 10), strconv.FormatInt(int64(p.TargetRoute), 10),
		strconv.FormatUint(p.SourceMatchID, 10), strconv.FormatUint(p.TargetMatchID, 10),
		strconv.FormatInt(int64(p.ProofType), 10), p.ProofID, p.OperationID,
	}, "\n")
}

// ProofSigner is shared by transition authorities. Secrets must be delivered
// through deployment secret management; empty/short secrets are rejected.
type ProofSigner struct{ secret []byte }

func NewProofSigner(secret string) (*ProofSigner, error) {
	if len(secret) < 32 {
		return nil, fmt.Errorf("placement proof secret must be at least 32 bytes")
	}
	return &ProofSigner{secret: []byte(secret)}, nil
}

func (s *ProofSigner) Sign(p Proof) string {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(p.canonical()))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *ProofSigner) Verify(p Proof, signature string) bool {
	provided, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(p.canonical()))
	return hmac.Equal(provided, mac.Sum(nil))
}

func (s *ProofSigner) SignTargetUnavailable(p TargetUnavailableProof) string {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(p.canonical()))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *ProofSigner) VerifyTargetUnavailable(p TargetUnavailableProof, signature string) bool {
	provided, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(p.canonical()))
	return hmac.Equal(provided, mac.Sum(nil))
}

func (s *ProofSigner) SignSourceDeparture(p SourceDepartureProof) string {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(p.canonical()))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *ProofSigner) VerifySourceDeparture(p SourceDepartureProof, signature string) bool {
	provided, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(p.canonical()))
	return hmac.Equal(provided, mac.Sum(nil))
}

// ProofKeyring enforces least privilege at the locator verifier: each proof
// type has an independent key, so compromising login cannot mint MATCH_START or
// terminal/leave authority and compromising a battle worker cannot bootstrap accounts.
type ProofKeyring struct {
	byType map[int32]*ProofSigner
}

func NewProofKeyring(secrets map[int32]string) (*ProofKeyring, error) {
	k := &ProofKeyring{byType: make(map[int32]*ProofSigner, len(secrets))}
	for proofType, secret := range secrets {
		if secret == "" {
			continue
		}
		signer, err := NewProofSigner(secret)
		if err != nil {
			return nil, fmt.Errorf("placement proof type %d: %w", proofType, err)
		}
		k.byType[proofType] = signer
	}
	return k, nil
}

func (k *ProofKeyring) Verify(p Proof, signature string) bool {
	if k == nil {
		return false
	}
	signer := k.byType[p.ProofType]
	return signer != nil && signer.Verify(p, signature)
}

func (k *ProofKeyring) VerifyTargetUnavailable(p TargetUnavailableProof, signature string) bool {
	if k == nil {
		return false
	}
	signer := k.byType[p.ProofType]
	return signer != nil && signer.VerifyTargetUnavailable(p, signature)
}

func (k *ProofKeyring) VerifySourceDeparture(p SourceDepartureProof, signature string) bool {
	if k == nil {
		return false
	}
	signer := k.byType[p.ProofType]
	return signer != nil && signer.VerifySourceDeparture(p, signature)
}
