package data

import (
	"bytes"
	"fmt"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/releasetrack"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
)

// validateBattleStorageWrite is the mechanical write-side counterpart of the
// rollout preflight.  Once the strict image is enabled, no Redis writer may
// publish a BattleStorageRecord that names any part of a physical GameServer
// without the complete ABA-safe tuple, including pod_uid.
//
// allocation_uncertain deliberately carries no physical identity.  Its
// reconciler may atomically advance it to one complete exact identity, but a
// partial tuple is never durable.
func validateBattleStorageWrite(record *dsv1.BattleStorageRecord) error {
	if record == nil {
		return fmt.Errorf("nil battle storage record")
	}
	if record.GetMatchId() == 0 || !canonicalBattleAllocationID(record.GetAllocationId()) {
		return fmt.Errorf("battle storage record requires match_id and canonical UUIDv4 allocation_id")
	}
	if len(record.ProtoReflect().GetUnknown()) != 0 {
		return fmt.Errorf("new battle storage record cannot contain protobuf unknown fields")
	}

	switch record.GetState() {
	case "allocating", BattleStateAllocationUncertain,
		BattleStateAllocationReconcileEmptyTombstone:
		if !battlePhysicalIdentityEmpty(record) {
			return fmt.Errorf("battle state %q cannot carry physical GameServer identity", record.GetState())
		}
	case "abandoned":
		if !battlePhysicalIdentityEmpty(record) {
			if err := validateExactBattlePhysicalIdentity(record); err != nil {
				return err
			}
		}
	case "warming", "ready", "running", "ended",
		BattleStateAllocationReconcileReleasePending,
		BattleStatePreactiveReleasePending,
		BattleStateAllocationAbortPending:
		if err := validateExactBattlePhysicalIdentity(record); err != nil {
			return err
		}
	default:
		return fmt.Errorf("battle storage record has unsupported state %q", record.GetState())
	}
	return nil
}

func validateExistingBattleStorageShape(record *dsv1.BattleStorageRecord) error {
	if record == nil {
		return fmt.Errorf("nil battle storage record")
	}
	known := proto.Clone(record).(*dsv1.BattleStorageRecord)
	known.ProtoReflect().SetUnknown(nil)
	return validateBattleStorageWrite(known)
}

func validateExactBattlePhysicalIdentity(record *dsv1.BattleStorageRecord) error {
	if !canonicalBattleIdentityValue(record.GetDsPodName()) {
		return fmt.Errorf("battle state %q requires canonical ds_pod_name", record.GetState())
	}
	if !canonicalBattleIdentityValue(record.GetGameserverUid()) {
		return fmt.Errorf("battle state %q requires canonical gameserver_uid", record.GetState())
	}
	if !canonicalBattleIdentityValue(record.GetPodUid()) {
		return fmt.Errorf("battle state %q requires canonical pod_uid", record.GetState())
	}
	if !releasetrack.Valid(record.GetReleaseTrack()) {
		return fmt.Errorf("battle state %q requires canonical release_track", record.GetState())
	}
	return nil
}

func battlePhysicalIdentityEmpty(record *dsv1.BattleStorageRecord) bool {
	return record.GetDsPodName() == "" &&
		record.GetDsAddr() == "" &&
		record.GetGameserverUid() == "" &&
		record.GetPodUid() == "" &&
		record.GetReleaseTrack() == "" &&
		record.GetInstanceEpoch() == 0
}

func canonicalBattleIdentityValue(value string) bool {
	return value != "" && strings.TrimSpace(value) == value &&
		!strings.ContainsFunc(value, func(r rune) bool {
			return unicode.IsSpace(r) || unicode.IsControl(r)
		})
}

// legacyBattleMissingPodUID identifies the one rolling-upgrade shape which is
// readable but no longer generally writable: an otherwise exact physical
// identity produced before pod_uid was persisted.  Its only legal write is an
// exact same-record backfill of pod_uid.
func legacyBattleMissingPodUID(record *dsv1.BattleStorageRecord) bool {
	if record == nil || record.GetPodUid() != "" ||
		!canonicalBattleIdentityValue(record.GetDsPodName()) ||
		!canonicalBattleIdentityValue(record.GetGameserverUid()) ||
		!releasetrack.Valid(record.GetReleaseTrack()) {
		return false
	}
	switch record.GetState() {
	case "warming", "ready", "running", "ended", "abandoned",
		BattleStateAllocationReconcileReleasePending,
		BattleStatePreactiveReleasePending,
		BattleStateAllocationAbortPending:
		return true
	default:
		return false
	}
}

func validateBattleStorageTransition(previous, next *dsv1.BattleStorageRecord) error {
	if next == nil {
		return fmt.Errorf("nil next battle storage record")
	}
	if previous == nil {
		return validateBattleStorageWrite(next)
	}
	if previous.GetMatchId() != next.GetMatchId() ||
		previous.GetAllocationId() != next.GetAllocationId() {
		return fmt.Errorf("battle storage transition changed match/allocation identity")
	}

	if legacyBattleMissingPodUID(previous) {
		if !canonicalBattleIdentityValue(next.GetPodUid()) {
			return fmt.Errorf("legacy battle physical identity may only backfill pod_uid")
		}
		if !bytes.Equal(previous.ProtoReflect().GetUnknown(), next.ProtoReflect().GetUnknown()) {
			return fmt.Errorf("legacy battle pod_uid backfill changed protobuf unknown bytes")
		}
		withoutPodUID := proto.Clone(next).(*dsv1.BattleStorageRecord)
		withoutPodUID.PodUid = ""
		if !proto.Equal(previous, withoutPodUID) {
			return fmt.Errorf("legacy battle pod_uid backfill changed another field")
		}
		return validateExistingBattleStorageShape(next)
	}

	if err := validateExistingBattleStorageShape(previous); err != nil {
		return fmt.Errorf("unsafe existing battle storage record is not writable: %w", err)
	}
	if previous.GetPodUid() != "" && previous.GetPodUid() != next.GetPodUid() {
		return fmt.Errorf("battle storage transition changed immutable pod_uid")
	}
	if !bytes.Equal(previous.ProtoReflect().GetUnknown(), next.ProtoReflect().GetUnknown()) {
		return fmt.Errorf("battle storage transition changed protobuf unknown fields")
	}
	return validateExistingBattleStorageShape(next)
}

func canonicalBattleAllocationID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed != uuid.Nil && parsed.Version() == uuid.Version(4) &&
		parsed.Variant() == uuid.RFC4122 &&
		parsed.String() == value
}

func marshalBattleTransition(previous, next *dsv1.BattleStorageRecord) ([]byte, error) {
	if err := validateBattleStorageTransition(previous, next); err != nil {
		return nil, err
	}
	return proto.Marshal(next)
}
