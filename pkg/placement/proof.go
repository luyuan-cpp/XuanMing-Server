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

func (p Proof) canonical() string {
	return strings.Join([]string{
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
