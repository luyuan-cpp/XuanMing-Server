// Package placement now only carries the transport-independent DS instance
// identity primitives (Target, ValidOperationID) shared by the battle
// allocation saga and the battle abort contract. The versioned placement
// routing/lease/proof system that used to live here was removed (2026-07):
// player routing is derived from player_locator TTL leases + match state.
package placement

import (
	"strings"

	"github.com/google/uuid"
)

// ValidOperationID accepts only canonical lowercase RFC4122 UUIDv4 strings.
func ValidOperationID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.Version() == uuid.Version(4) &&
		id.Variant() == uuid.RFC4122 && id.String() == value
}

// Target is an exact DS instance identity tuple (Hub assignment or Battle
// allocation).  Same-name pod replacements never compare Equal because the
// InstanceUID/InstanceEpoch differ.
type Target struct {
	PodName       string // DS Pod 名称
	InstanceUID   string // 实例 UID，区分同名 Pod 的不同实例
	InstanceEpoch uint32 // 实例纪元，随实例重启递增
	AssignmentID  string // Hub 分配 ID
	AllocationID  string // Battle 分配 ID
	ReleaseTrack  string // 发布轨道(灰度/正式)
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
