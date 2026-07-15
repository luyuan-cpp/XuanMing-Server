package biz

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
	"github.com/luyuancpp/pandora/services/runtime/player_locator/internal/data"
)

const placementOptimisticRetry = 5

type BeginPlacementInput struct {
	PlayerID        uint64
	ExpectedVersion uint64
	TargetRoute     locatorv1.PlacementRoute
	OperationID     string
	SourceMatchID   uint64
	ProofType       locatorv1.PlacementProofType
	ProofID         string
	LeaseDeadlineMs int64
	TargetMatchID   uint64
	ProofSignature  string
}

type BindPlacementInput struct {
	PlayerID        uint64
	Version         uint64
	OperationID     string
	TargetRoute     locatorv1.PlacementRoute
	PodName         string
	InstanceUID     string
	AssignmentID    string
	TargetMatchID   uint64
	InstanceEpoch   uint32
	AllocationID    string
	ReleaseTrack    string
}

type CommitPlacementInput struct {
	BindPlacementInput
	AdmissionID string
}

// PlacementUsecase owns the durable route state machine.
type PlacementUsecase struct {
	repo data.PlacementRepo
	now  func() time.Time
	proofVerifier PlacementProofVerifier
}

type PlacementProofVerifier interface {
	Verify(placement.Proof, string) bool
}

func NewPlacementUsecase(repo data.PlacementRepo, verifier ...PlacementProofVerifier) *PlacementUsecase {
	u := &PlacementUsecase{repo: repo, now: time.Now}
	if len(verifier) > 0 {
		u.proofVerifier = verifier[0]
	}
	return u
}

func (u *PlacementUsecase) Get(ctx context.Context, playerID uint64) (*locatorv1.PlayerPlacementStorageRecord, bool, error) {
	return u.repo.GetPlacement(ctx, playerID)
}

func (u *PlacementUsecase) Begin(ctx context.Context, in BeginPlacementInput) (*locatorv1.PlayerPlacementStorageRecord, error) {
	if in.PlayerID == 0 || in.ExpectedVersion == 0 || !placement.ValidOperationID(in.OperationID) ||
		(in.TargetRoute != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB &&
			in.TargetRoute != locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE) ||
		strings.TrimSpace(in.ProofID) == "" {
		return nil, errcode.New(errcode.ErrInvalidArg, "complete placement begin identity required")
	}
	if in.TargetRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB {
		battleExit := in.SourceMatchID > 0 &&
			(in.ProofType == locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_TERMINAL ||
				in.ProofType == locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_PLAYER_LEAVE)
		hubTransfer := in.SourceMatchID == 0 &&
			in.ProofType == locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_HUB_TRANSFER
		if !battleExit && !hubTransfer {
			return nil, errcode.New(errcode.ErrPermissionDeny, "HUB target requires terminal/leave or Hub-transfer proof")
		}
	} else if in.TargetMatchID == 0 || in.ProofType != locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_START {
		return nil, errcode.New(errcode.ErrPermissionDeny, "HUB to BATTLE requires authoritative match-start proof")
	}
	nowMs := u.now().UnixMilli()
	if in.LeaseDeadlineMs <= nowMs {
		return nil, errcode.New(errcode.ErrInvalidArg, "placement lease deadline must be in the future")
	}
	return u.repo.UpdatePlacement(ctx, in.PlayerID, placementOptimisticRetry,
		func(cur *locatorv1.PlayerPlacementStorageRecord, found bool) (*locatorv1.PlayerPlacementStorageRecord, error) {
			if !found {
				return nil, errcode.New(errcode.ErrLocatorNotFound, "placement is UNKNOWN")
			}
			if samePendingBegin(cur, in) {
				// A durable worker may lose the Begin response or be down longer than
				// the first lease.  Retrying the *same signed operation* is the only
				// writer allowed to renew it; without this renewal an expired PENDING
				// record could never reach Bind and would strand the player forever.
				if !u.verifyProof(cur.GetCurrentRoute(), in) {
					return nil, errcode.New(errcode.ErrPermissionDeny, "placement transition proof verification failed")
				}
				next := proto.Clone(cur).(*locatorv1.PlayerPlacementStorageRecord)
				if in.LeaseDeadlineMs > next.GetLeaseDeadlineMs() {
					next.LeaseDeadlineMs = in.LeaseDeadlineMs
					next.UpdatedAtMs = nowMs
				}
				return next, nil
			}
			if cur.GetVersion() != in.ExpectedVersion ||
				cur.GetTransitionState() != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE {
				return nil, errcode.New(errcode.ErrLocatorConflict, "placement begin expected stable source version")
			}
			if in.TargetRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB {
				if in.ProofType == locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_HUB_TRANSFER {
					if cur.GetCurrentRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB || cur.GetMatchId() != 0 {
						return nil, errcode.New(errcode.ErrLocatorConflict, "Hub transfer expected stable Hub source")
					}
				} else if cur.GetCurrentRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE || cur.GetMatchId() != in.SourceMatchID {
					return nil, errcode.New(errcode.ErrLocatorConflict, "placement begin expected active source Battle")
				}
			}
			if in.TargetRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE &&
				cur.GetCurrentRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB {
				return nil, errcode.New(errcode.ErrLocatorConflict, "placement begin expected stable Hub source")
			}
			if !u.verifyProof(cur.GetCurrentRoute(), in) {
				return nil, errcode.New(errcode.ErrPermissionDeny, "placement transition proof verification failed")
			}
			next := proto.Clone(cur).(*locatorv1.PlayerPlacementStorageRecord)
			next.TargetRoute = in.TargetRoute
			next.TransitionState = locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING
			next.Version++
			next.OperationId = in.OperationID
			next.SourceMatchId = in.SourceMatchID
			next.TargetMatchId = in.TargetMatchID
			next.DsPodName = ""
			next.DsInstanceUid = ""
			next.HubAssignmentId = ""
			next.DsInstanceEpoch = 0
			next.AllocationId = ""
			next.ReleaseTrack = ""
			next.UpdatedAtMs = nowMs
			next.LeaseDeadlineMs = in.LeaseDeadlineMs
			next.ProofType = in.ProofType
			next.ProofId = in.ProofID
			next.AdmissionId = ""
			return next, nil
		})
}

func samePendingBegin(cur *locatorv1.PlayerPlacementStorageRecord, in BeginPlacementInput) bool {
	return cur.GetVersion() == in.ExpectedVersion+1 && cur.GetOperationId() == in.OperationID &&
		cur.GetTransitionState() == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
		cur.GetTargetRoute() == in.TargetRoute &&
		cur.GetSourceMatchId() == in.SourceMatchID && cur.GetProofType() == in.ProofType &&
		cur.GetProofId() == in.ProofID && cur.GetTargetMatchId() == in.TargetMatchID
}

func (u *PlacementUsecase) Bind(ctx context.Context, in BindPlacementInput) (*locatorv1.PlayerPlacementStorageRecord, error) {
	if err := validatePlacementTarget(in); err != nil {
		return nil, err
	}
	nowMs := u.now().UnixMilli()
	return u.repo.UpdatePlacement(ctx, in.PlayerID, placementOptimisticRetry,
		func(cur *locatorv1.PlayerPlacementStorageRecord, found bool) (*locatorv1.PlayerPlacementStorageRecord, error) {
			if !found {
				return nil, errcode.New(errcode.ErrLocatorNotFound, "placement is UNKNOWN")
			}
			if sameCommittedTarget(cur, in) {
				return proto.Clone(cur).(*locatorv1.PlayerPlacementStorageRecord), nil
			}
			if !samePendingIdentity(cur, in) || (cur.GetLeaseDeadlineMs() > 0 && cur.GetLeaseDeadlineMs() <= nowMs) {
				return nil, errcode.New(errcode.ErrLocatorConflict, "stale or expired placement target binding")
			}
			if cur.GetDsPodName() != "" || cur.GetDsInstanceUid() != "" || cur.GetHubAssignmentId() != "" ||
				cur.GetAllocationId() != "" {
				if cur.GetDsPodName() == in.PodName && cur.GetDsInstanceUid() == in.InstanceUID &&
					cur.GetDsInstanceEpoch() == in.InstanceEpoch && cur.GetHubAssignmentId() == in.AssignmentID &&
					cur.GetAllocationId() == in.AllocationID && cur.GetReleaseTrack() == in.ReleaseTrack &&
					cur.GetTargetMatchId() == in.TargetMatchID {
					return proto.Clone(cur).(*locatorv1.PlayerPlacementStorageRecord), nil
				}
				return nil, errcode.New(errcode.ErrLocatorConflict, "placement operation already bound to another target")
			}
			next := proto.Clone(cur).(*locatorv1.PlayerPlacementStorageRecord)
			next.DsPodName = in.PodName
			next.DsInstanceUid = in.InstanceUID
			next.HubAssignmentId = in.AssignmentID
			next.TargetMatchId = in.TargetMatchID
			next.DsInstanceEpoch = in.InstanceEpoch
			next.AllocationId = in.AllocationID
			next.ReleaseTrack = in.ReleaseTrack
			next.UpdatedAtMs = nowMs
			return next, nil
		})
}

func (u *PlacementUsecase) Commit(ctx context.Context, in CommitPlacementInput) (*locatorv1.PlayerPlacementStorageRecord, error) {
	if err := validatePlacementTarget(in.BindPlacementInput); err != nil {
		return nil, err
	}
	if !validAdmissionID(in.AdmissionID) {
		return nil, errcode.New(errcode.ErrInvalidArg, "canonical admission_id UUIDv4 required")
	}
	nowMs := u.now().UnixMilli()
	return u.repo.UpdatePlacement(ctx, in.PlayerID, placementOptimisticRetry,
		func(cur *locatorv1.PlayerPlacementStorageRecord, found bool) (*locatorv1.PlayerPlacementStorageRecord, error) {
			if !found {
				return nil, errcode.New(errcode.ErrLocatorNotFound, "placement is UNKNOWN")
			}
			if sameCommittedTarget(cur, in.BindPlacementInput) {
				// Re-admission to the same authoritative target is safe even when a
				// reconnect generated a new admission_id. Placement fences route
				// identity (version/op/full target), while JTI/assignment/session
				// authorities own per-attempt replay and takeover semantics. Keep the
				// first admission_id as audit data and do not mutate the stable record.
				return proto.Clone(cur).(*locatorv1.PlayerPlacementStorageRecord), nil
			}
			if !samePendingIdentity(cur, in.BindPlacementInput) ||
				cur.GetDsPodName() != in.PodName || cur.GetDsInstanceUid() != in.InstanceUID ||
				cur.GetDsInstanceEpoch() != in.InstanceEpoch || cur.GetHubAssignmentId() != in.AssignmentID ||
				cur.GetAllocationId() != in.AllocationID || cur.GetReleaseTrack() != in.ReleaseTrack ||
				cur.GetTargetMatchId() != in.TargetMatchID ||
				(cur.GetLeaseDeadlineMs() > 0 && cur.GetLeaseDeadlineMs() <= nowMs) {
				return nil, errcode.New(errcode.ErrLocatorConflict, "placement admission lost final CAS")
			}
			next := proto.Clone(cur).(*locatorv1.PlayerPlacementStorageRecord)
			next.CurrentRoute = in.TargetRoute
			next.TargetRoute = locatorv1.PlacementRoute_PLACEMENT_ROUTE_UNSPECIFIED
			next.TransitionState = locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE
			if in.TargetRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE {
				next.MatchId = in.TargetMatchID
			} else {
				next.MatchId = 0
			}
			next.UpdatedAtMs = nowMs
			next.LeaseDeadlineMs = 0
			next.AdmissionId = in.AdmissionID
			return next, nil
		})
}

func validatePlacementTarget(in BindPlacementInput) error {
	if in.PlayerID == 0 || in.Version == 0 || !placement.ValidOperationID(in.OperationID) ||
		(in.TargetRoute != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB &&
			in.TargetRoute != locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE) {
		return errcode.New(errcode.ErrInvalidArg, "complete placement target identity required")
	}
	target := placement.Target{PodName: in.PodName, InstanceUID: in.InstanceUID,
		InstanceEpoch: in.InstanceEpoch, AssignmentID: in.AssignmentID,
		AllocationID: in.AllocationID, ReleaseTrack: in.ReleaseTrack}
	if in.TargetRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB {
		if !target.CompleteHub() || in.TargetMatchID != 0 || in.AllocationID != "" {
			return errcode.New(errcode.ErrInvalidArg, "complete Hub placement target required")
		}
	} else if !target.CompleteBattle() || in.TargetMatchID == 0 || in.AssignmentID != "" {
		return errcode.New(errcode.ErrInvalidArg, "complete Battle placement target required")
	}
	return nil
}

func samePendingIdentity(cur *locatorv1.PlayerPlacementStorageRecord, in BindPlacementInput) bool {
	return cur.GetVersion() == in.Version && cur.GetOperationId() == in.OperationID &&
		cur.GetTransitionState() == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
		cur.GetTargetRoute() == in.TargetRoute && cur.GetTargetMatchId() == in.TargetMatchID
}

func sameCommittedTarget(cur *locatorv1.PlayerPlacementStorageRecord, in BindPlacementInput) bool {
	return cur.GetVersion() == in.Version && cur.GetOperationId() == in.OperationID &&
		cur.GetTransitionState() == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE &&
		cur.GetCurrentRoute() == in.TargetRoute && cur.GetDsPodName() == in.PodName &&
		cur.GetDsInstanceUid() == in.InstanceUID && cur.GetDsInstanceEpoch() == in.InstanceEpoch &&
		cur.GetHubAssignmentId() == in.AssignmentID && cur.GetAllocationId() == in.AllocationID &&
		cur.GetReleaseTrack() == in.ReleaseTrack &&
		(in.TargetRoute != locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE || cur.GetMatchId() == in.TargetMatchID)
}

func (u *PlacementUsecase) verifyProof(source locatorv1.PlacementRoute, in BeginPlacementInput) bool {
	return u.proofVerifier != nil && u.proofVerifier.Verify(placement.Proof{
		PlayerID: in.PlayerID, ExpectedVersion: in.ExpectedVersion, SourceRoute: int32(source),
		TargetRoute: int32(in.TargetRoute), SourceMatchID: in.SourceMatchID,
		TargetMatchID: in.TargetMatchID, ProofType: int32(in.ProofType), ProofID: in.ProofID,
		OperationID: in.OperationID,
	}, in.ProofSignature)
}

type BootstrapPlacementInput struct {
	PlayerID uint64
	OperationID string
	ProofID string
	ProofSignature string
	LeaseDeadlineMs int64
}

func (u *PlacementUsecase) Bootstrap(ctx context.Context, in BootstrapPlacementInput) (*locatorv1.PlayerPlacementStorageRecord, error) {
	if in.PlayerID == 0 || !placement.ValidOperationID(in.OperationID) || strings.TrimSpace(in.ProofID) == "" ||
		in.LeaseDeadlineMs <= u.now().UnixMilli() {
		return nil, errcode.New(errcode.ErrInvalidArg, "complete bootstrap identity required")
	}
	proof := placement.Proof{PlayerID: in.PlayerID, TargetRoute: int32(locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB),
		ProofType: int32(locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_ACCOUNT_BOOTSTRAP),
		ProofID: in.ProofID, OperationID: in.OperationID}
	if u.proofVerifier == nil || !u.proofVerifier.Verify(proof, in.ProofSignature) {
		return nil, errcode.New(errcode.ErrPermissionDeny, "bootstrap proof verification failed")
	}
	nowMs := u.now().UnixMilli()
	return u.repo.UpdatePlacement(ctx, in.PlayerID, placementOptimisticRetry,
		func(cur *locatorv1.PlayerPlacementStorageRecord, found bool) (*locatorv1.PlayerPlacementStorageRecord, error) {
			if found {
				if cur.GetVersion() == 1 &&
					cur.GetProofType() == locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_ACCOUNT_BOOTSTRAP &&
					cur.GetProofId() == in.ProofID && cur.GetOperationId() == in.OperationID &&
					cur.GetTransitionState() == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
					cur.GetTargetRoute() == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB {
					next := proto.Clone(cur).(*locatorv1.PlayerPlacementStorageRecord)
					if in.LeaseDeadlineMs > next.GetLeaseDeadlineMs() {
						next.LeaseDeadlineMs = in.LeaseDeadlineMs
						next.UpdatedAtMs = nowMs
					}
					return next, nil
				}
				return nil, errcode.New(errcode.ErrLocatorConflict, "placement already exists")
			}
			return &locatorv1.PlayerPlacementStorageRecord{
				PlayerId: in.PlayerID, TargetRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
				TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
				Version: 1, OperationId: in.OperationID, UpdatedAtMs: nowMs,
				LeaseDeadlineMs: in.LeaseDeadlineMs,
				ProofType: locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_ACCOUNT_BOOTSTRAP,
				ProofId: in.ProofID,
			}, nil
		})
}

func validAdmissionID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.Version() == uuid.Version(4) &&
		id.Variant() == uuid.RFC4122 && id.String() == value
}
