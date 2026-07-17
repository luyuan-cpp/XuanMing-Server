// match_client.go — login → matchmaker gRPC 客户端封装(P0 修复 2026-07-15)。
//
// 目的:关闭"locator 30s 租约被当唯一权威"的窗口(codex P0-2/P0-3/P0-4)。
// locator 是 presence 投影(带 TTL,可蒸发);matchmaker 的 player claim + match
// 记录才是"玩家是否属于一场活跃对局"的耐久事实(claim 由 ReleaseMatch 显式释放)。
// login 在 presence 未命中 BATTLE 时,再查一次 matchmaker 只读权威兜底,防止:
//   - READY 与 locator 投影之间的窗口(notifyBattle 之前/失败)把玩家误路由回 Hub;
//   - locator TTL 恰好蒸发时对局仍活跃,玩家 Hub/Battle 双在场。
//
// 设计(复刻 hub_client.go 弱依赖模式):
//   - data 层暴露 MatchContextResolver 接口,biz 只依赖接口
//   - addr 未配 → main 注入 nil,biz 检查 nil 走 presence-only(dev/local 兼容)
package data

import (
	"context"

	"google.golang.org/grpc"

	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	matchv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/match/v1"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/internalrpcauth"
)

// PlayerMatchAuthority 是 ResolvePlayerMatchContext 的最小 client 视角产出。
type PlayerMatchAuthority struct {
	// State: matchmaker 权威三态。
	//   UNSPECIFIED = 读取错误/索引漂移(fail-closed,B1 下可重试)
	//   NONE        = 明确无活跃撮合/对局
	//   ACTIVE      = 有活跃 claim(排队/确认/分配/READY)
	State matchv1.PlayerMatchContextState
	Stage matchv1.PlayerMatchResumeStage
	MatchID      uint64
	BattleDSAddr string
	// GameMode 是撮合命名空间的 canonical 值(如 5v5_ranked / pve_coop),来自 matchmaker
	// 持久记录(ticket/match 的 game_mode 字段)。冷启动客户端要用它恢复
	// x-pandora-game-mode 路由头;绝不允许 login 侧按 PVE/PVP 硬编码猜测。
	GameMode string
}

// MatchContextResolver 给 login.biz 查询玩家在 matchmaker 侧的耐久对局归属。
type MatchContextResolver interface {
	ResolvePlayerMatchContext(ctx context.Context, playerID uint64) (PlayerMatchAuthority, error)
}

// GrpcMatchContextResolver 实现 MatchContextResolver,内嵌 grpc client。
type GrpcMatchContextResolver struct {
	conn   *grpc.ClientConn
	client matchv1.MatchServiceClient
	// signer 是 login→matchmaker 内部东西向鉴权(pkg/internalrpcauth):matchmaker 侧
	// ResolvePlayerMatchContext 强制校验 login 服务身份 HMAC + Redis nonce 防重放。
	// nil = 不签名(仅容忍 matchmaker 未启用 resume auth 的裸 dev 环境;启用环境会被
	// ERR_PERMISSION_DENY 拒),main 装配时对 addr 已配但 secret 缺失打启动告警。
	signer *internalrpcauth.Signer
}

// NewGrpcMatchContextResolver 用现成的 *grpc.ClientConn 包出 resolver。
// 调用方负责 conn 生命周期管理(main.go defer conn.Close())。
// signer 可为 nil(见字段注释)。
func NewGrpcMatchContextResolver(conn *grpc.ClientConn, signer *internalrpcauth.Signer) *GrpcMatchContextResolver {
	return &GrpcMatchContextResolver{
		conn:   conn,
		client: matchv1.NewMatchServiceClient(conn),
		signer: signer,
	}
}

// ResolvePlayerMatchContext 调 matchmaker 只读权威(零副作用,不改任何撮合状态)。
// 每次调用签一份新鲜的 request-bound 凭证(方法+player_id+时间戳+一次性 nonce)。
func (r *GrpcMatchContextResolver) ResolvePlayerMatchContext(
	ctx context.Context, playerID uint64,
) (PlayerMatchAuthority, error) {
	if r.signer != nil {
		signed, serr := r.signer.SignContext(ctx,
			matchv1.MatchService_ResolvePlayerMatchContext_FullMethodName, playerID)
		if serr != nil {
			return PlayerMatchAuthority{}, errcode.NewCause(errcode.ErrInternal, serr,
				"sign matchmaker resume-auth credential")
		}
		ctx = signed
	}
	resp, err := r.client.ResolvePlayerMatchContext(ctx,
		&matchv1.ResolvePlayerMatchContextRequest{PlayerId: playerID})
	if err != nil {
		return PlayerMatchAuthority{}, errcode.New(errcode.ErrInternal,
			"matchmaker ResolvePlayerMatchContext rpc: %v", err)
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return PlayerMatchAuthority{}, errcode.New(errcode.Code(resp.GetCode()),
			"matchmaker ResolvePlayerMatchContext code=%d", resp.GetCode())
	}
	return PlayerMatchAuthority{
		State:        resp.GetState(),
		Stage:        resp.GetStage(),
		MatchID:      resp.GetMatchId(),
		BattleDSAddr: resp.GetBattleDsAddr(),
		GameMode:     resp.GetGameMode(),
	}, nil
}
