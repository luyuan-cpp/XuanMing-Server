// Package placement contains the shared, transport-independent contract for
// versioned player placement leases. The durable record itself lives in
// player_locator; callers only pass an exact version/operation binding.
package placement

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Mode controls the zero-downtime rollout of placement enforcement.
type Mode uint8

const (
	ModeOff Mode = iota
	ModeShadow
	ModeEnforce
)

// Wire enum values are kept here so proof producers do not need to import the
// locator transport package merely to build a signed canonical statement.
const (
	RouteUnknown int32 = 0
	RouteHub int32 = 1
	RouteBattle int32 = 2
	ProofAccountBootstrap int32 = 3
	ProofMatchTerminal int32 = 1
	ProofPlayerLeave int32 = 2
	ProofMatchStart int32 = 4
	ProofHubTransfer int32 = 5
)

func (m Mode) String() string {
	switch m {
	case ModeOff:
		return "off"
	case ModeShadow:
		return "shadow"
	case ModeEnforce:
		return "enforce"
	default:
		return "invalid"
	}
}

// ParseMode rejects misspellings instead of silently weakening enforcement.
func ParseMode(raw string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "off":
		return ModeOff, nil
	case "shadow":
		return ModeShadow, nil
	case "enforce":
		return ModeEnforce, nil
	default:
		return ModeOff, fmt.Errorf("placement mode %q invalid (want off|shadow|enforce)", raw)
	}
}

// Binding is copied into a Hub assignment and its signed DS ticket.
// Complete is deliberately all-or-nothing; partial bindings are never usable.
type Binding struct {
	Version       uint64
	OperationID   string
	SourceMatchID uint64
}

func (b Binding) Empty() bool {
	return b.Version == 0 && b.OperationID == "" && b.SourceMatchID == 0
}

func (b Binding) Complete() bool {
	return b.Version > 0 && ValidOperationID(b.OperationID)
}

func (b Binding) ValidateOptional() error {
	if b.Empty() || b.Complete() {
		return nil
	}
	return fmt.Errorf("placement binding must be all present or all absent")
}

func (b Binding) Equal(other Binding) bool {
	return b.Version == other.Version && b.OperationID == other.OperationID &&
		b.SourceMatchID == other.SourceMatchID
}

// ValidOperationID accepts only canonical lowercase RFC4122 UUIDv4 strings.
func ValidOperationID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.Version() == uuid.Version(4) &&
		id.Variant() == uuid.RFC4122 && id.String() == value
}

// Target is the exact Hub DS identity committed at final Admission.
type Target struct {
	PodName      string
	InstanceUID  string
	InstanceEpoch uint32
	AssignmentID string
	AllocationID string
	ReleaseTrack string
}

func (t Target) CompleteHub() bool {
	return strings.TrimSpace(t.PodName) != "" && strings.TrimSpace(t.InstanceUID) != "" &&
		t.InstanceEpoch > 0 && strings.TrimSpace(t.AssignmentID) != "" &&
		strings.TrimSpace(t.ReleaseTrack) != ""
}

func (t Target) CompleteBattle() bool {
	return strings.TrimSpace(t.PodName) != "" && strings.TrimSpace(t.InstanceUID) != "" &&
		t.InstanceEpoch > 0 && strings.TrimSpace(t.AllocationID) != "" &&
		strings.TrimSpace(t.ReleaseTrack) != ""
}

func (t Target) Equal(other Target) bool {
	return t.PodName == other.PodName && t.InstanceUID == other.InstanceUID &&
		t.InstanceEpoch == other.InstanceEpoch && t.AssignmentID == other.AssignmentID &&
		t.AllocationID == other.AllocationID && t.ReleaseTrack == other.ReleaseTrack
}
