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

	"github.com/google/uuid"

	"github.com/luyuancpp/pandora/pkg/auth"
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
}

// TicketUsecase 处理 DSTicket 的签发 / 校验。
//
// W3 ①:HS256 + 5min exp;jti 用 uuid v4。
// W3 ②:jtiRepo 非空时,VerifyDSTicket 通过后 SETNX,防止同一 jti 被多个 DS 重放。
type TicketUsecase struct {
	signer   *auth.Signer
	verifier *auth.Verifier
	jtiRepo  data.TicketJTIRepo // 可空(dev 不接 redis 时):跳过防重放,只验签
}

// NewTicketUsecase 构造用例。
func NewTicketUsecase(signer *auth.Signer, verifier *auth.Verifier, jtiRepo data.TicketJTIRepo) *TicketUsecase {
	return &TicketUsecase{signer: signer, verifier: verifier, jtiRepo: jtiRepo}
}

// IssueDSTicket 给指定 player 签 hub / battle DS 票据。
//
// dsType: "hub" / "battle"
// targetID: hub 为 0;battle 必须填 match_id
// playerID: 已通过 session 校验(本用例不再二次解 session_token,只信调用方)
//
// 失败返回 *errcode.Error。
func (u *TicketUsecase) IssueDSTicket(ctx context.Context, playerID uint64, dsType string, targetID uint64) (*DSTicketResult, error) {
	h := plog.With(ctx)

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

	jti := uuid.NewString()
	tok, expMs, err := u.signer.SignDSTicket(playerID, ds, targetID, jti)
	if err != nil {
		h.Errorw("msg", "sign_ds_ticket_failed", "err", err, "player_id", playerID, "ds_type", dsType)
		return nil, errcode.New(errcode.ErrInternal, "sign ds ticket failed: %v", err)
	}

	h.Infow("msg", "ds_ticket_issued",
		"player_id", playerID, "ds_type", dsType, "target_id", targetID,
		"jti", jti, "exp_ms", expMs)

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
	h := plog.With(ctx)

	claims, err := u.verifier.VerifyDSTicket(ticket)
	if err != nil {
		h.Warnw("msg", "verify_ds_ticket_failed", "err", err, "ds_pod", dsPodName)
		return nil, err
	}

	// W3 ②:防重放(SETNX pandora:ticket:<jti> EX 5min)
	if u.jtiRepo != nil && claims.ID != "" {
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
		PlayerID: claims.PlayerID(),
		MatchID:  claims.MatchID,
		DSType:   claims.DSType,
		JTI:      claims.ID,
	}
	if claims.IssuedAt != nil {
		out.IssuedAtMs = claims.IssuedAt.UnixMilli()
	}
	if claims.ExpiresAt != nil {
		out.ExpiresAtMs = claims.ExpiresAt.UnixMilli()
	}
	return out, nil
}
