// Package poduidpreflight implements the read-only Redis release gate used
// before enabling strict Model-B authority. It never writes Redis: its only
// purpose is to prove that every battle record which already names an exact
// GameServer identity also carries the Pod UID needed for ABA-safe release.
package poduidpreflight

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/google/uuid"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
)

const (
	CategoryExactIdentity       = "exact_identity"
	CategoryAllocationUncertain = "allocation_uncertain"
	CategoryNoPhysicalIdentity  = "no_physical_identity"
	CategoryUnsafe              = "unsafe"
)

// Classification is deliberately pure so release tooling and tests use the
// same conservative state/identity decision without contacting Kubernetes.
type Classification struct {
	Category string
	Reasons  []string
}

// ClassifyBattle classifies one decoded canonical battle record. An empty
// Reasons slice means the record is safe for strict Model-B activation.
//
// allocation_uncertain is intentionally not called an exact identity: before
// reconciliation it has only allocation_id and may not yet have a GameServer.
// The durable uncertain reconciler owns that state. Once any physical identity
// field appears, the tuple must be complete and include pod_uid.
func ClassifyBattle(keyMatchID uint64, rec *dsv1.BattleStorageRecord) Classification {
	result := Classification{Category: CategoryUnsafe}
	if rec == nil {
		result.Reasons = []string{"record is nil"}
		return result
	}
	// This release gate is compiled against one exact storage schema.  Go's
	// protobuf decoder deliberately preserves fields that this binary does not
	// know, but treating such a record as safe would silently classify state the
	// gate cannot inspect.  A newer writer must ship a newer gate first.
	if len(rec.ProtoReflect().GetUnknown()) != 0 {
		result.Reasons = append(result.Reasons,
			"battle record contains unknown protobuf fields that this release gate cannot audit")
	}
	if keyMatchID == 0 {
		result.Reasons = append(result.Reasons, "battle key contains match_id=0")
	}
	if rec.GetMatchId() != keyMatchID {
		result.Reasons = append(result.Reasons, fmt.Sprintf(
			"record match_id=%d does not match key match_id=%d", rec.GetMatchId(), keyMatchID))
	}
	if !validAllocationID(rec.GetAllocationId()) {
		result.Reasons = append(result.Reasons, "battle record has non-canonical UUIDv4 allocation_id")
	}

	switch rec.GetState() {
	case "allocating":
		result.Category = CategoryNoPhysicalIdentity
		result.Reasons = append(result.Reasons, requireNoPhysicalIdentity(rec, "allocating")...)
	case "allocation_uncertain":
		result.Category = CategoryAllocationUncertain
		result.Reasons = append(result.Reasons, requireNoPhysicalIdentity(rec, "allocation_uncertain")...)
	case "allocation_reconcile_empty_tombstone":
		result.Category = CategoryNoPhysicalIdentity
		result.Reasons = append(result.Reasons,
			requireNoPhysicalIdentity(rec, "allocation_reconcile_empty_tombstone")...)
	case "abandoned":
		if physicalIdentityEmpty(rec) {
			// Authoritative allocation-id reconciliation may prove that no
			// GameServer exists and leave an empty permanent terminal fence.
			result.Category = CategoryNoPhysicalIdentity
		} else {
			result.Category = CategoryExactIdentity
			result.Reasons = append(result.Reasons, requireExactPhysicalIdentity(rec, "abandoned")...)
		}
	case "warming", "ready", "running", "ended",
		"allocation_reconcile_release_pending", "preactive_release_pending",
		"allocation_abort_pending":
		result.Category = CategoryExactIdentity
		result.Reasons = append(result.Reasons, requireExactPhysicalIdentity(rec, rec.GetState())...)
	default:
		result.Reasons = append(result.Reasons, fmt.Sprintf(
			"unknown canonical battle state %q", rec.GetState()))
	}
	if len(result.Reasons) != 0 {
		result.Category = CategoryUnsafe
	}
	return result
}

func validAllocationID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed != uuid.Nil && parsed.Version() == uuid.Version(4) &&
		parsed.Variant() == uuid.RFC4122 && parsed.String() == value
}

func requireNoPhysicalIdentity(rec *dsv1.BattleStorageRecord, state string) []string {
	if physicalIdentityEmpty(rec) {
		return nil
	}
	return []string{fmt.Sprintf(
		"%s carries a partial or unexpected physical GameServer identity", state)}
}

func requireExactPhysicalIdentity(rec *dsv1.BattleStorageRecord, state string) []string {
	var reasons []string
	if !canonicalIdentityValue(rec.GetDsPodName()) {
		reasons = append(reasons, fmt.Sprintf("%s exact identity has empty ds_pod_name", state))
	}
	if !canonicalIdentityValue(rec.GetGameserverUid()) {
		reasons = append(reasons, fmt.Sprintf("%s exact identity has empty gameserver_uid", state))
	}
	if rec.GetReleaseTrack() != "stable" && rec.GetReleaseTrack() != "canary" {
		reasons = append(reasons, fmt.Sprintf("%s exact identity has invalid release_track", state))
	}
	if !canonicalIdentityValue(rec.GetPodUid()) {
		reasons = append(reasons, fmt.Sprintf(
			"%s exact allocation identity is missing pod_uid", state))
	}
	return reasons
}

func canonicalIdentityValue(value string) bool {
	return value != "" && strings.TrimSpace(value) == value &&
		!strings.ContainsFunc(value, func(r rune) bool {
			return unicode.IsSpace(r) || unicode.IsControl(r)
		})
}

// physicalIdentityEmpty excludes allocation_id: allocation_id is the durable
// idempotency fence and is expected even before a GameServer has been found.
func physicalIdentityEmpty(rec *dsv1.BattleStorageRecord) bool {
	return rec.GetDsPodName() == "" &&
		rec.GetDsAddr() == "" &&
		rec.GetGameserverUid() == "" &&
		rec.GetPodUid() == "" &&
		rec.GetReleaseTrack() == "" &&
		rec.GetInstanceEpoch() == 0
}
