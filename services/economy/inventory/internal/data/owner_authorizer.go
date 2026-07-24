// owner_authorizer.go — 背包域五要件② owner 授权(2026-07-22,bag-domain.md phase 2)。
//
// 逐写查询 owner authority(§9.22)权威记录,校验调用方声称的 (player, owner_epoch):
//   - epoch 必须等于当前记录(旧 epoch = 失租旧 owner 迟到写,拒);
//   - phase 必须 ADMITTED(PENDING = 屏障未开,新 DS 只可预载,不得产生业务写);
//   - owner_type 必须 HUB/BATTLE(NONE = 无 owner,离线改包必须走邮件,§2);
//   - 实例租约必须在效(lease_deadline > now;失租 DS 的写一律拒,§9.22 fencing)。
//
// 查询失败 / 结果不确定 → ErrUnavailable fail-closed(禁冒充有权,§9.22);
// 校验不过 → ErrBagEpochFenced(调用方停写重查,语义与存储侧 CAS fencing 一致)。
//
// 不缓存:授权必须基于当前权威(§9.22 缓存不参与权威写决策);journal 批量写天然摊薄查询量。
package data

import (
	"context"
	"time"

	"google.golang.org/grpc"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/grpcclient"
	plog "github.com/luyuancpp/pandora/pkg/log"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	ownerv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/owner/v1"
)

// GrpcOwnerAuthorizer 用 owner 服务 gRPC client 实现 biz.OwnerAuthorizer。
type GrpcOwnerAuthorizer struct {
	conn *grpc.ClientConn
	cli  ownerv1.OwnerServiceClient
	// now 可注入(测试);默认 time.Now。
	now func() time.Time
}

// NewGrpcOwnerAuthorizer 直连 owner 服务 endpoint(host:port,内网 insecure)。
func NewGrpcOwnerAuthorizer(ownerAddr string) *GrpcOwnerAuthorizer {
	conn := grpcclient.MustDialInsecure(ownerAddr)
	return &GrpcOwnerAuthorizer{conn: conn, cli: ownerv1.NewOwnerServiceClient(conn), now: time.Now}
}

// Close 关闭底层连接。
func (a *GrpcOwnerAuthorizer) Close() error {
	if a.conn != nil {
		return a.conn.Close()
	}
	return nil
}

// AuthorizeOwnerWrite 实现 biz.OwnerAuthorizer(判定表见接口注释)。
// callerPod 非空 = 五要件①已验身份,record.target 必须与之全等;callerPod 空(guard
// off/dev)时必须以 claimedEpoch 作证明(=当前 epoch 才放行),两者都缺 → fail-closed 拒。
func (a *GrpcOwnerAuthorizer) AuthorizeOwnerWrite(ctx context.Context, playerID, claimedEpoch uint64, callerPod, callerUID string) (uint64, error) {
	// bag 写的 owner 授权 fencing 拒绝(§9.22 五要件②)是 stale writer / 屏障未开 / 失租的直接证据,
	// 但都返回业务码 ErrBagEpochFenced/ErrUnauthorized(不被 access log 中间件当故障)→ 显式 WARN 留证。
	// ErrUnavailable(owner 服务不可达)不在此打:它是 server fault,已由 access log 中间件升 ERROR。
	logReject := func(reason string, extra ...any) {
		plog.With(ctx).Warnw(append([]any{"msg", "bag_owner_authz_rejected",
			"player_id", playerID, "reason", reason}, extra...)...)
	}
	resp, err := a.cli.QueryOwner(ctx, &ownerv1.QueryOwnerRequest{PlayerId: playerID})
	if err != nil {
		return 0, errcode.New(errcode.ErrUnavailable, "owner query player=%d: %v", playerID, err)
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return 0, errcode.New(errcode.ErrUnavailable, "owner query player=%d code=%d", playerID, resp.GetCode())
	}
	rec := resp.GetRecord()
	if rec.GetOwnerType() != ownerv1.OwnerType_OWNER_TYPE_HUB &&
		rec.GetOwnerType() != ownerv1.OwnerType_OWNER_TYPE_BATTLE {
		logReject("no_active_owner", "owner_type", int32(rec.GetOwnerType()))
		return 0, errcode.New(errcode.ErrBagEpochFenced,
			"player=%d has no active owner (type=%d)", playerID, rec.GetOwnerType())
	}
	switch {
	case callerPod != "":
		// 五要件①身份在手:owner 记录必须指向调用方实例(pod+uid 全等;旧 owner 迟到写必拒)。
		if rec.GetTarget().GetPodName() != callerPod || rec.GetTarget().GetInstanceUid() != callerUID {
			logReject("target_mismatch", "caller_pod", callerPod, "caller_uid", callerUID,
				"owner_pod", rec.GetTarget().GetPodName(), "owner_uid", rec.GetTarget().GetInstanceUid())
			return 0, errcode.New(errcode.ErrBagEpochFenced,
				"owner target mismatch player=%d caller=%s/%s owner=%s/%s", playerID,
				callerPod, callerUID, rec.GetTarget().GetPodName(), rec.GetTarget().GetInstanceUid())
		}
	case claimedEpoch != 0:
		// 无验签身份(ds_auth off):退化为 epoch 证明(调用方必须预知当前 epoch)。
	default:
		logReject("missing_identity_and_epoch")
		return 0, errcode.New(errcode.ErrUnauthorized,
			"bag write requires DS credential identity or explicit owner_epoch (player=%d)", playerID)
	}
	if claimedEpoch != 0 && rec.GetOwnerEpoch() != claimedEpoch {
		logReject("epoch_mismatch", "claimed_epoch", claimedEpoch, "current_epoch", rec.GetOwnerEpoch())
		return 0, errcode.New(errcode.ErrBagEpochFenced,
			"owner epoch mismatch player=%d claim=%d current=%d", playerID, claimedEpoch, rec.GetOwnerEpoch())
	}
	if rec.GetPhase() != ownerv1.OwnerPhase_OWNER_PHASE_ADMITTED {
		logReject("not_admitted", "phase", int32(rec.GetPhase()))
		return 0, errcode.New(errcode.ErrBagEpochFenced,
			"owner not admitted player=%d phase=%d", playerID, rec.GetPhase())
	}
	if rec.GetLeaseDeadlineMs() <= a.now().UnixMilli() {
		logReject("lease_expired", "lease_deadline_ms", rec.GetLeaseDeadlineMs())
		return 0, errcode.New(errcode.ErrBagEpochFenced,
			"owner lease expired player=%d deadline=%d", playerID, rec.GetLeaseDeadlineMs())
	}
	return rec.GetOwnerEpoch(), nil
}
