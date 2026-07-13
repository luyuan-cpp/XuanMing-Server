// ticket.go — DSTicket 签发 / 校验用例(W3 ①,2026-06-05)。
//
// 不变量(CLAUDE.md §9):
//   - §3 DS 票据短时效:本用例签的 ticket 默认 exp 5min
//   - §4 DS 崩溃必有补偿:本用例不维护 ticket 状态(无状态),DS 崩溃由 player_locator + hub_allocator 补
//   - §6 MMR 计算在 battle_result(DS 不可信):本用例签的 ticket 只代表"准入",DS 内业务不能信任 ticket 之外的玩家数据
//
// W3 ②(2026-06-05)真实化:
//   - VerifyDSTicket 通过签名后,调 jtiRepo.MarkUsed(jti, ds_ticket_ttl) → SETNX 防重放
//   - SETNX 失败映射 ErrLoginTicketReplayed(同一票据被多个 DS 重复 Verify)
//   - IssueDSTicket 仍只签发(不预占 jti,节省一次 redis 写)
//
// 本用例只做"签 / 验",IssueDSTicket 的输入校验(session 是否在线、target_id 是否合法 DS pod)由调用方做。

package biz

import (
	"context"
	"slices"

	"github.com/google/uuid"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/cellroute"
	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"

	"github.com/luyuancpp/pandora/services/account/login/internal/data"
)

// DSTicketResult 是 IssueDSTicket 的产出。
type DSTicketResult struct {
	Ticket      string
	JTI         string
	ExpiresAtMs int64
	PlayerID    uint64
	// BattleDSAddr 仅供 login reconnect 内部使用，来自 roster authorizer 的同一 Redis 快照。
	// 公共 IssueDSTicket(battle) 响应仍不返回地址（正常地址来自 matchmaker）。
	BattleDSAddr string
}

// DSTicketClaims 是 VerifyDSTicket 的产出(透传 auth.DSTicketClaims 的核心字段,
// service 层翻译成 proto LoginService.DSTicket message)。
type DSTicketClaims struct {
	PlayerID    uint64
	MatchID     uint64
	DSType      string
	JTI         string
	IssuedAtMs  int64
	ExpiresAtMs int64
	// RegionID / CellID 是票据绑定的玩家路由落点(scale-cellular-20m.md §3.3)。
	// 单 Cell / dev 票据为 0。DS 侧据此校验票据 Cell == 本 DS 所在 Cell,防跨单元串号。
	RegionID uint32
	CellID   uint32
	// RoleID 是票据携带的玩家已选角色(选角权威化 2026-07-08)。0 = 未携带。
	RoleID uint32
	// Hub DSTicket 的当前归属/DS active 凭据绑定。battle/legacy 票据为零值。
	DSPodName       string
	DSInstanceUID   string
	DSProtocolEpoch uint32
	DSCredentialGen uint64
	DSCredentialJTI string
	HubAssignmentID string
	DSWriterEpoch   uint32
}

// TicketUsecase 处理 DSTicket 的签发 / 校验。
//
// W3 ①:HS256 + 5min exp;jti 用 uuid v4。
// W3 ②:jtiRepo 非空时,VerifyDSTicket 通过后 SETNX,防止同一 jti 被多个 DS 重放。
type TicketUsecase struct {
	signer            *auth.Signer
	verifier          *auth.Verifier
	jtiRepo           data.TicketJTIRepo // 可空(dev 不接 redis 时):跳过防重放,只验签
	assignmentChecker data.HubAssignmentChecker
	battleAuthorizer  data.BattleTicketAuthorizer
	// requireHubAssignmentBinding 是滚动激活栅栏。false 接受旧的全空绑定 hub 票；true
	// 后只接受 hub_allocator 签发且与 Redis 当前 assignment 精确一致的完整绑定票。
	requireHubAssignmentBinding bool

	// router 是确定性 region/cell 路由器(scale-cellular-20m.md §3.3)。
	// 可为 nil:单 Cell / dev 部署不路由,签出票据 region/cell = 0。多 Cell 部署由 main
	// 经 SetCellRouter 注入。nil-safe,不阻断签票。
	router *cellroute.Router
}

// NewTicketUsecase 构造用例。
func NewTicketUsecase(signer *auth.Signer, verifier *auth.Verifier, jtiRepo data.TicketJTIRepo) *TicketUsecase {
	return &TicketUsecase{signer: signer, verifier: verifier, jtiRepo: jtiRepo}
}

// SetHubAssignmentBindingPolicy 在服务启动、对外监听前注入 Hub 归属校验器与激活栅栏。
func (u *TicketUsecase) SetHubAssignmentBindingPolicy(require bool, checker data.HubAssignmentChecker) {
	u.requireHubAssignmentBinding = require
	u.assignmentChecker = checker
}

// SetBattleTicketAuthorizer 注入 battle 票签发前的 player↔match roster 权威门。
// 未注入时 battle 签票 fail-closed；Hub 签票不受此门影响。
func (u *TicketUsecase) SetBattleTicketAuthorizer(authorizer data.BattleTicketAuthorizer) {
	u.battleAuthorizer = authorizer
}

// SetCellRouter 注入确定性 region/cell 路由器(可选,多 Cell 部署用)。
//
// nil-safe:不调用 / 传 nil 时签出票据 region/cell = 0(单 Cell / dev 语义)。
// 用 setter 而非构造参数,避免单 Cell 阶段调用点被迫改签名(与 LoginUsecase.SetCellRouter 一致)。
func (u *TicketUsecase) SetCellRouter(r *cellroute.Router) {
	u.router = r
}

// routeRegionCell 算玩家落点;router 为 nil(单 Cell / dev)或 Route 报错时降级为 0/0,不阻断签票。
func (u *TicketUsecase) routeRegionCell(ctx context.Context, playerID uint64) (regionID, cellID uint32) {
	if u.router == nil {
		return 0, 0
	}
	loc, err := u.router.Route(playerID)
	if err != nil {
		plog.With(ctx).Warnw("msg", "cellroute_failed", "err", err, "player_id", playerID)
		return 0, 0
	}
	return loc.RegionID, loc.CellID
}

// IssueDSTicket 给指定 player 签 hub / battle DS 票据。
//
// dsType: "hub" / "battle"
// targetID: hub 为 0;battle 必须填 match_id
// playerID: 已通过 session 校验(本用例不再二次解 session_token,只信调用方)
//
// 失败返回 *errcode.Error。
func (u *TicketUsecase) IssueDSTicket(ctx context.Context, playerID uint64, dsType string, targetID uint64) (*DSTicketResult, error) {
	if playerID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "playerID must be > 0")
	}
	var ds auth.DSType
	switch dsType {
	case string(auth.DSTypeHub):
		ds = auth.DSTypeHub
	case string(auth.DSTypeBattle):
		ds = auth.DSTypeBattle
	default:
		return nil, errcode.New(errcode.ErrInvalidArg, "dsType must be hub|battle, got %q", dsType)
	}
	if ds == auth.DSTypeBattle && targetID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "battle DSTicket requires match_id (targetID)")
	}
	if ds == auth.DSTypeBattle {
		regionID, cellID := u.routeRegionCell(ctx, playerID)
		return u.IssueBattleDSTicketAtCell(ctx, playerID, targetID, regionID, cellID)
	}
	if ds == auth.DSTypeHub && u.requireHubAssignmentBinding {
		return nil, errcode.New(errcode.ErrUnavailable,
			"hub DSTicket must be issued by hub_allocator while assignment binding is required")
	}

	// 算玩家路由落点并签进票据(§3.3 防跨单元串号);单 Cell / dev → 0/0。
	regionID, cellID := u.routeRegionCell(ctx, playerID)
	return u.issueDSTicketAtCell(ctx, playerID, ds, targetID, regionID, cellID)
}

// IssueBattleDSTicketAtCell 是所有 login 侧 Battle 签票路径的唯一入口。公共
// IssueDSTicket 与登录断线重连都必须先经过同一个 player↔match roster 权威门，
// 防止重连路径因只相信 locator 而重新引入“知道 match_id 即可拿票”的旁路。
func (u *TicketUsecase) IssueBattleDSTicketAtCell(
	ctx context.Context,
	playerID, matchID uint64,
	regionID, cellID uint32,
) (*DSTicketResult, error) {
	if playerID == 0 || matchID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "battle ticket requires player and match")
	}
	if u == nil || u.battleAuthorizer == nil {
		return nil, errcode.New(errcode.ErrUnavailable, "battle ticket roster authority unavailable")
	}
	target, err := u.battleAuthorizer.AuthorizeBattleTicket(ctx, playerID, matchID)
	if err != nil {
		return nil, err
	}
	if target.DSAddr == "" {
		return nil, errcode.New(errcode.ErrUnavailable, "battle ticket target address unavailable")
	}
	result, err := u.issueDSTicketAtCell(ctx, playerID, auth.DSTypeBattle, matchID, regionID, cellID)
	if err != nil {
		return nil, err
	}
	result.BattleDSAddr = target.DSAddr
	return result, nil
}

func (u *TicketUsecase) issueDSTicketAtCell(
	ctx context.Context,
	playerID uint64,
	ds auth.DSType,
	targetID uint64,
	regionID, cellID uint32,
) (*DSTicketResult, error) {
	h := plog.With(ctx)
	jti := uuid.NewString()
	tok, expMs, err := u.signer.SignDSTicketWithCell(playerID, ds, targetID, regionID, cellID, jti)
	if err != nil {
		h.Errorw("msg", "sign_ds_ticket_failed", "err", err, "player_id", playerID, "ds_type", string(ds))
		return nil, errcode.New(errcode.ErrInternal, "sign ds ticket failed: %v", err)
	}

	h.Infow("msg", "ds_ticket_issued",
		"player_id", playerID, "ds_type", string(ds), "target_id", targetID,
		"jti", jti, "exp_ms", expMs, "region_id", regionID, "cell_id", cellID)

	return &DSTicketResult{
		Ticket:      tok,
		JTI:         jti,
		ExpiresAtMs: expMs,
		PlayerID:    playerID,
	}, nil
}

// VerifyDSTicket 校验 ticket 签名 + exp + iss + aud,然后(W3 ②)SETNX jti 防重放。
//
// dsPodName 当前仅写日志,W3+ 接 DS 注册表后用于"票据 target_id == pod 自报 id" 二次校验。
func (u *TicketUsecase) VerifyDSTicket(ctx context.Context, ticket, dsPodName string) (*DSTicketClaims, error) {
	return u.verifyDSTicket(ctx, ticket, dsPodName, "", nil)
}

// VerifyDSTicketForAdmission 是 Redis authority 的在线入场入口。
// caller 必须已经由 service 依次完成 DSCallbackGuard 验签与 Redis active checker；本方法
// 再把玩家票据 claims 与 caller active binding 精确比对，最后才以 admission owner 幂等消费 jti。
func (u *TicketUsecase) VerifyDSTicketForAdmission(
	ctx context.Context,
	ticket, dsPodName, admissionID string,
	admission data.DSAdmissionBinding,
) (*DSTicketClaims, error) {
	if admissionID == "" {
		return nil, errcode.New(errcode.ErrInvalidArg, "admission_id is required")
	}
	if !admission.Complete() {
		return nil, errcode.New(errcode.ErrUnauthorized, "ds admission binding is incomplete")
	}
	return u.verifyDSTicket(ctx, ticket, dsPodName, admissionID, &admission)
}

func (u *TicketUsecase) verifyDSTicket(
	ctx context.Context,
	ticket, dsPodName, admissionID string,
	admission *data.DSAdmissionBinding,
) (*DSTicketClaims, error) {
	h := plog.With(ctx)

	claims, err := u.verifier.VerifyDSTicket(ticket)
	if err != nil {
		h.Warnw("msg", "verify_ds_ticket_failed", "err", err, "ds_pod", dsPodName)
		return nil, err
	}
	var (
		admissionRepo  data.AdmissionTicketJTIRepo
		markerStatus   data.AdmissionMarkerStatus
		attemptOwner   string
		credentialHash string
	)
	if admission != nil {
		var ok bool
		admissionRepo, ok = u.jtiRepo.(data.AdmissionTicketJTIRepo)
		if !ok || admissionRepo == nil || claims.ID == "" {
			return nil, errcode.New(errcode.ErrUnavailable, "ticket admission replay authority unavailable")
		}
		var ownerErr error
		attemptOwner, ownerErr = admission.AdmissionAttemptOwner(admissionID)
		if ownerErr == nil {
			credentialHash, ownerErr = admission.AcceptedCredentialHash()
		}
		if ownerErr != nil {
			return nil, errcode.NewCause(errcode.ErrInvalidArg, ownerErr, "invalid admission marker owner")
		}
		markerStatus, err = admissionRepo.PeekAdmission(ctx, claims.ID, attemptOwner)
		if err != nil {
			return nil, err
		}
		if markerStatus == data.AdmissionMarkerConflict {
			return nil, errcode.New(errcode.ErrLoginTicketReplayed, "ticket already belongs to another admission")
		}
		if markerStatus == data.AdmissionMarkerMissing {
			err = validateTicketAdmissionStrict(claims, dsPodName, *admission)
		} else {
			err = validateTicketAdmissionRetry(claims, dsPodName, *admission)
		}
		if err != nil {
			h.Warnw("msg", "ds_ticket_admission_binding_rejected", "err", err,
				"player_id", claims.PlayerID(), "ds_pod", dsPodName, "ds_type", claims.DSType)
			return nil, err
		}
		if claims.DSType == string(auth.DSTypeHub) {
			admissionChecker, ok := u.assignmentChecker.(data.AdmissionHubAssignmentChecker)
			if !ok || admissionChecker == nil {
				return nil, errcode.New(errcode.ErrUnavailable, "hub admission assignment checker unavailable")
			}
			stable := hubBindingFromClaims(claims)
			active := hubBindingFromAdmission(*admission, claims.HubAssignmentID)
			if err := admissionChecker.CheckCurrentAdmission(ctx, claims.PlayerID(), stable, active,
				markerStatus == data.AdmissionMarkerMissing); err != nil {
				return nil, err
			}
		}
	} else if claims.DSType == string(auth.DSTypeHub) {
		// off/legacy 保留原有票内 binding 兼容策略。
		binding := hubBindingFromClaims(claims)
		if binding.Complete() {
			if err := u.checkHubAssignment(ctx, claims.PlayerID(), dsPodName, binding); err != nil {
				return nil, err
			}
		} else if !binding.Empty() {
			return nil, errcode.New(errcode.ErrLoginTicketInvalid, "hub ticket has incomplete assignment binding")
		} else if u.requireHubAssignmentBinding {
			return nil, errcode.New(errcode.ErrLoginTicketInvalid, "hub ticket missing required assignment binding")
		}
	}

	// W3 ②:防重放。legacy/off 保持原始单次 SETNX；Redis authority 每次都用 Lua
	// 原子确认短幂等窗：missing→marker，existing same-attempt 仅确认、不覆盖/不续 TTL；
	// 这也封住 Peek 后验证期间刚好跨出 replay_until 的竞态。
	if admission != nil {
		status, markErr := admissionRepo.MarkUsedByAdmission(
			ctx, claims.ID, attemptOwner, credentialHash, u.verifier.DSTicketTTL())
		if markErr != nil {
			h.Warnw("msg", "ds_ticket_admission_replay_blocked",
				"jti", claims.ID, "player_id", claims.PlayerID(), "ds_pod", dsPodName, "err", markErr)
			return nil, markErr
		}
		if status != data.AdmissionMarkerCreated && status != data.AdmissionMarkerExisting {
			return nil, errcode.New(errcode.ErrLoginTicketReplayed, "ticket admission marker conflict")
		}
	} else if admission == nil && u.jtiRepo != nil && claims.ID != "" {
		if err := u.jtiRepo.MarkUsed(ctx, claims.ID, u.verifier.DSTicketTTL()); err != nil {
			h.Warnw("msg", "ds_ticket_replay_blocked",
				"jti", claims.ID, "player_id", claims.PlayerID(), "ds_pod", dsPodName, "err", err)
			return nil, err
		}
	}

	h.Infow("msg", "ds_ticket_verified",
		"player_id", claims.PlayerID(),
		"ds_type", claims.DSType, "match_id", claims.MatchID,
		"jti", claims.ID, "ds_pod", dsPodName)

	out := &DSTicketClaims{
		PlayerID:        claims.PlayerID(),
		MatchID:         claims.MatchID,
		DSType:          claims.DSType,
		JTI:             claims.ID,
		RegionID:        claims.RegionID,
		CellID:          claims.CellID,
		RoleID:          claims.RoleID,
		DSPodName:       claims.DSPodName,
		DSInstanceUID:   claims.DSInstanceUID,
		DSProtocolEpoch: claims.DSProtocolEpoch,
		DSCredentialGen: claims.DSCredentialGen,
		DSCredentialJTI: claims.DSCredentialJTI,
		HubAssignmentID: claims.HubAssignmentID,
		DSWriterEpoch:   claims.DSWriterEpoch,
	}
	if claims.IssuedAt != nil {
		out.IssuedAtMs = claims.IssuedAt.UnixMilli()
	}
	if claims.ExpiresAt != nil {
		out.ExpiresAtMs = claims.ExpiresAt.UnixMilli()
	}
	return out, nil
}

// validateTicketAdmissionStrict 用于 marker 不存在的首次准入，必须在 MarkUsed 前把
// 玩家票内完整 Hub credential 或 Battle match 与本次 caller active 精确绑定。
// Hub 票还需把票内 assignment/active tuple 与调用者 active 精确绑定；Battle 票没有
// assignment 字段，必须把 ticket.match_id 与 caller active.match_id 精确绑定。
func validateTicketAdmissionStrict(claims *auth.DSTicketClaims, dsPodName string, admission data.DSAdmissionBinding) error {
	if claims == nil || !admission.Complete() || dsPodName == "" || dsPodName != admission.PodName ||
		claims.DSType != string(admission.DSType) {
		return errcode.New(errcode.ErrLoginTicketInvalid, "ds ticket caller type or pod mismatch")
	}
	switch admission.DSType {
	case auth.DSTypeHub:
		if claims.MatchID != 0 || admission.MatchID != 0 ||
			claims.DSPodName != admission.PodName || claims.DSInstanceUID != admission.InstanceUID ||
			claims.DSProtocolEpoch != admission.ProtocolEpoch || claims.DSCredentialGen != admission.CredentialGen ||
			claims.DSCredentialJTI != admission.CredentialJTI || claims.DSWriterEpoch != admission.WriterEpoch {
			return errcode.New(errcode.ErrLoginTicketInvalid, "hub ticket does not match caller active credential")
		}
	case auth.DSTypeBattle:
		if claims.MatchID == 0 || claims.MatchID != admission.MatchID ||
			!slices.Contains(admission.PlayerIDs, claims.PlayerID()) {
			return errcode.New(errcode.ErrLoginTicketInvalid, "battle ticket match does not match caller active credential")
		}
	default:
		return errcode.New(errcode.ErrLoginTicketInvalid, "ds ticket admission type invalid")
	}
	return nil
}

// validateTicketAdmissionRetry 仅在 Redis 已存在同 attempt_owner marker 时使用。
// 普通 token 轮换允许 gen/jti/exp/kid/hash 变化；稳定身份(type/match/pod/UID/instance
// epoch/writer)与 Hub assignment_id 仍必须一致。caller 当前 active/projection 已由 service 先验。
func validateTicketAdmissionRetry(claims *auth.DSTicketClaims, dsPodName string, admission data.DSAdmissionBinding) error {
	if claims == nil || !admission.Complete() || dsPodName == "" || dsPodName != admission.PodName ||
		claims.DSType != string(admission.DSType) {
		return errcode.New(errcode.ErrLoginTicketInvalid, "ds ticket retry caller type or pod mismatch")
	}
	switch admission.DSType {
	case auth.DSTypeHub:
		if claims.MatchID != 0 || admission.MatchID != 0 || claims.HubAssignmentID == "" ||
			claims.DSPodName != admission.PodName || claims.DSInstanceUID != admission.InstanceUID ||
			claims.DSProtocolEpoch != admission.ProtocolEpoch || claims.DSWriterEpoch != admission.WriterEpoch {
			return errcode.New(errcode.ErrLoginTicketInvalid, "hub ticket retry stable identity mismatch")
		}
	case auth.DSTypeBattle:
		if claims.MatchID == 0 || claims.MatchID != admission.MatchID ||
			!slices.Contains(admission.PlayerIDs, claims.PlayerID()) {
			return errcode.New(errcode.ErrLoginTicketInvalid, "battle ticket retry match mismatch")
		}
	default:
		return errcode.New(errcode.ErrLoginTicketInvalid, "ds ticket retry admission type invalid")
	}
	return nil
}

func hubBindingFromClaims(claims *auth.DSTicketClaims) data.HubAssignmentBinding {
	if claims == nil {
		return data.HubAssignmentBinding{}
	}
	return data.HubAssignmentBinding{
		PodName: claims.DSPodName, InstanceUID: claims.DSInstanceUID,
		ProtocolEpoch: claims.DSProtocolEpoch, CredentialGen: claims.DSCredentialGen,
		CredentialJTI: claims.DSCredentialJTI, AssignmentID: claims.HubAssignmentID,
		WriterEpoch: claims.DSWriterEpoch,
	}
}

func hubBindingFromAdmission(admission data.DSAdmissionBinding, assignmentID string) data.HubAssignmentBinding {
	return data.HubAssignmentBinding{
		PodName: admission.PodName, InstanceUID: admission.InstanceUID,
		ProtocolEpoch: admission.ProtocolEpoch, CredentialGen: admission.CredentialGen,
		CredentialJTI: admission.CredentialJTI, AssignmentID: assignmentID,
		WriterEpoch: admission.WriterEpoch, ExpMs: admission.ExpMs, Kid: admission.Kid,
		TokenSHA256: admission.TokenSHA256,
	}
}

func (u *TicketUsecase) checkHubAssignment(
	ctx context.Context,
	playerID uint64,
	dsPodName string,
	binding data.HubAssignmentBinding,
) error {
	if !binding.Complete() {
		return errcode.New(errcode.ErrLoginTicketInvalid, "hub ticket assignment binding incomplete")
	}
	if dsPodName == "" || dsPodName != binding.PodName {
		return errcode.New(errcode.ErrUnauthorized, "hub ticket target pod mismatch")
	}
	if u.assignmentChecker == nil {
		return errcode.New(errcode.ErrUnavailable, "hub assignment checker unavailable")
	}
	return u.assignmentChecker.CheckCurrent(ctx, playerID, binding)
}
