// hub_authoritative.go 是 Model B「Redis 唯一授权权威」的**原子线性化写路径**
// (decision-revisit-ds-callback-auth §7,审核二轮 CE1/CE2/CE4/CE6/CE8)。
//
// 核心:授权记录 pandora:hub:auth:{pod} 与分片镜像 pandora:hub:shard:{pod} 共享 {pod} hashtag,
// **同 slot**,因此可在单个 WATCH/MULTI/EXEC 事务里同时读改两把键。本文件把「校验凭据 → promote
// pending→active → 把心跳应用到分片镜像并投影 active 元组」和「校验授权 active → 原子占座」各自
// 压缩成**一个事务**,消灭上一版「先 promote 再单独写分片」「先查授权再单独 reserveSeat」的两段式
// 竞态窗口(半激活 / TOCTOU 误分配)。
//
// read-modify-write 一律默认 proto.Unmarshal(不 DiscardUnknown,不变量 §17)。
package data

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	authpkg "github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/releasetrack"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
)

// ActivateHeartbeatInput 是 ActivateHeartbeat 的心跳负载(与凭据身份分开,便于 biz 组装)。
type ActivateHeartbeatInput struct {
	PlayerCount int32
	PlayerIDs   []uint64
	MaxPlayers  uint32
	State       string
	TsMs        int64
	AuthTTL     time.Duration // 授权键 TTL(CE8:必须用 authTTL,不能用 shardTTL 缩短授权寿命)
	ShardTTL    time.Duration // 分片镜像键 TTL
}

// ActivateResult 是 ActivateHeartbeat 的结果。
type ActivateResult struct {
	// Accepted:本次心跳完成了 pending→active 原子激活(线性化点首个合法心跳)。
	Accepted bool
	// ShardFound:分片镜像存在且本次已在同事务内被应用心跳 + 投影 active 元组。
	// false = 分片镜像缺失(孤儿 / 早于拓扑种子):**未 promote、未写任何键**,biz 需先 reconcile 拓扑再重试,
	// 保证 promote 与 warming→ready 恒同事务(杜绝半激活)。
	ShardFound bool
	// ShardState:分片镜像应用心跳后的最新状态(供 biz 下发 drain/stop 指令);ShardFound=false 时为空。
	ShardState string
	// 当前 active 凭据元组(回显给 DS + 供审计)。
	ActiveGen     uint64
	ActiveJTI     string
	InstanceUID   string
	ProtocolEpoch uint32
	WriterEpoch   uint32
}

// ReserveResult 是 ReserveRoutableSeat / CheckRoutable 的结果。
type ReserveResult struct {
	// OK:目标分片当前可路由(授权 active 且分片 ready 且元组一致且心跳新鲜且未满)。
	// ReserveRoutableSeat 下 OK=true 表示已原子 seat++;CheckRoutable 下仅表示可路由(未占座)。
	OK bool
	// Reason:OK=false 的原因(仅日志用,不外露客户端)。
	Reason string
	// 当前 active 凭据元组(供归属记录钉元组 / 比对实例漂移)。
	ActiveGen     uint64
	ActiveJTI     string
	InstanceUID   string
	ProtocolEpoch uint32
	WriterEpoch   uint32
	// 分片路由信息(供 AssignHub/TransferHub 组装归属与票据,避免再读一次)。
	ShardID      uint32
	HubAddr      string
	Region       string
	PlayerCount  int32
	Capacity     int32
	ReleaseTrack string
}

// QuarantineResult 区分唯一权威吊销与派生投影 drain。AuthQuarantined=true 即表示
// 泄露凭据已失效；ProjectionDrained=false 只表示 shard 缺失/漂移，绝不能反过来阻止吊销。
type QuarantineResult struct {
	AuthQuarantined   bool
	ProjectionDrained bool
}

// ActivateHeartbeat 见 HubAuthRepo 接口注释。单事务(authKey+shardKey 同 slot):
//
//	校验凭据(uid/epoch/相位/匹配 pending 或 active)→ 分片存在?
//	  ├─ 否 → ShardFound=false,不 promote、不写键(biz reconcile 后重试)
//	  └─ 是 → promote(若匹配 pending) + applyHeartbeatToShard + 投影 active 元组 → 一次 EXEC 写两键
func (r *RedisHubAuthRepo) ActivateHeartbeat(ctx context.Context, pod string, id CredentialIdentity, in ActivateHeartbeatInput) (ActivateResult, error) {
	if id.Gen == 0 || id.JTI == "" || id.InstanceUID == "" || id.ProtocolEpoch == 0 || id.TokenSHA256 == "" ||
		id.Kid == "" || id.WriterEpoch != authpkg.DSAuthWriterEpochV2 {
		return ActivateResult{}, errAuthStale
	}
	if in.PlayerCount < 0 || in.MaxPlayers == 0 || len(in.PlayerIDs) != int(in.PlayerCount) {
		return ActivateResult{}, errcode.New(errcode.ErrInvalidArg, "hub heartbeat count/max_players invalid")
	}
	seenPlayers := make(map[uint64]struct{}, len(in.PlayerIDs))
	for _, playerID := range in.PlayerIDs {
		if playerID == 0 {
			return ActivateResult{}, errcode.New(errcode.ErrInvalidArg, "hub heartbeat player_ids contains zero")
		}
		if _, exists := seenPlayers[playerID]; exists {
			return ActivateResult{}, errcode.New(errcode.ErrInvalidArg, "hub heartbeat player_ids contains duplicate")
		}
		seenPlayers[playerID] = struct{}{}
	}
	aKey, sKey := authKey(pod), shardKey(pod)
	watchKeys := append([]string{aKey, sKey}, capacityLedgerKeys(pod)...)
	// 权威心跳时刻只取服务端接收时间。请求 ts_ms 仅可用于遥测，绝不能决定 TTL/新鲜度，
	// 否则一个未来时间戳可让失联 DS 长期保持可分配。
	serverNowMs := time.Now().UnixMilli()
	var out ActivateResult
	for attempt := 0; attempt < hubAuthCASRetries; attempt++ {
		var bizErr error
		out = ActivateResult{}
		txErr := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			// ① 授权记录必须存在。
			ab, gerr := tx.Get(ctx, aKey).Bytes()
			if gerr == redis.Nil {
				bizErr = errAuthStale
				return bizErr
			}
			if gerr != nil {
				return gerr
			}
			auth := &hubv1.HubShardAuthStorageRecord{}
			if uerr := proto.Unmarshal(ab, auth); uerr != nil {
				return uerr
			}
			if auth.PodName != pod || !hubAuthRecordV2Exact(auth) || id.WriterEpoch != authpkg.DSAuthWriterEpochV2 {
				bizErr = errAuthStale
				return bizErr
			}
			// ② 相位锁定(QUARANTINED/TERMINATING)一律拒(CE9-iii)。
			if phaseLocked(auth.Phase) {
				bizErr = errAuthStale
				return bizErr
			}
			// ③ 记录级身份:uid/epoch 必须匹配(gen 单独不安全)。
			if auth.InstanceUid != id.InstanceUID || auth.ProtocolEpoch != id.ProtocolEpoch {
				bizErr = errAuthStale
				return bizErr
			}
			// ④ 匹配 pending(线性化点) / active(幂等) / 都不匹配(stale)。
			promote := false
			switch {
			case routableCredentialComplete(auth.Pending, auth, serverNowMs) && credMatches(auth.Pending, id):
				promote = true
			case routableCredentialComplete(auth.Active, auth, serverNowMs) && credMatches(auth.Active, id):
				// 幂等:已是 active。
			default:
				bizErr = errAuthStale
				return bizErr
			}
			// ⑤ 分片镜像必须存在,才能把 promote 与 warming→ready 绑同一事务。
			sb, serr := tx.Get(ctx, sKey).Bytes()
			if serr == redis.Nil {
				out.ShardFound = false // 不 promote、不写键:交 biz reconcile 后重试
				return nil
			}
			if serr != nil {
				return serr
			}
			shard, uerr := unmarshalShard(pod, sb)
			if uerr != nil {
				return uerr
			}
			// MaxPlayers 是 DS 运行时 GameSession 的真实值。必须在 promote、心跳时间、
			// ready/state、ledger cleanup 等任何副作用之前与 allocator capacity 精确相等。
			if shard.GetCapacity() <= 0 || int64(in.MaxPlayers) != int64(shard.GetCapacity()) {
				bizErr = errcode.New(errcode.ErrInvalidState,
					"hub heartbeat max_players=%d does not match capacity=%d", in.MaxPlayers, shard.GetCapacity())
				return bizErr
			}
			ledger, lerr := loadHubCapacityLedger(ctx, tx, pod, shard.GetCapacity())
			if lerr != nil {
				return lerr
			}
			// Heartbeat 只清 reservation 绝对过期、旧格式 session 到期和 UID 漂移；
			// player_ids 缺席绝不能释放 connected ownership。
			pruneHubCapacityLedger(ledger, pod, auth.GetInstanceUid(), auth.GetProtocolEpoch(),
				authpkg.DSAuthWriterEpochV2, serverNowMs)
			if lerr := syncShardCapacityProjection(shard, ledger); lerr != nil {
				return lerr
			}
			// ⑥ promote(若匹配 pending):active=pending,清 pending,phase=ACTIVE。
			if promote {
				auth.Active = auth.Pending
				auth.Pending = nil
				auth.PendingStartedMs = 0
				auth.DeliveredRv = ""
				auth.Phase = hubv1.HubAuthPhase_HUB_AUTH_PHASE_ACTIVE
				out.Accepted = true
			}
			auth.UpdatedAtMs = serverNowMs
			auth.LastActiveHeartbeatMs = serverNowMs
			// ⑦ 应用心跳到分片镜像(warming→ready 等)+ 投影 active 元组(与 promote 同事务)。
			applyHeartbeatStateToShard(shard, in.State, serverNowMs)
			shard.ReportedConnectedCount = in.PlayerCount
			shard.ReportedMaxPlayers = in.MaxPlayers
			shard.LastVerifiedGen = auth.Active.Gen
			shard.LastVerifiedJti = auth.Active.Jti
			shard.GameserverUid = auth.InstanceUid
			shard.AuthEpoch = auth.ProtocolEpoch
			shard.LastVerifiedWriterEpoch = auth.Active.WriterEpoch
			authPayload, merr := proto.Marshal(auth)
			if merr != nil {
				return merr
			}
			shardPayload, merr := marshalShard(shard)
			if merr != nil {
				return merr
			}
			// ⑧ 一次 EXEC 写 auth+shard+ledger(各自 TTL:CE8 授权键用 authTTL,
			// 分片键用 shardTTL；ledger retention 按单项绝对 expiry)。
			_, perr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				if err := writeHubCapacityLedger(ctx, pipe, pod, ledger); err != nil {
					return err
				}
				pipe.Set(ctx, aKey, authPayload, in.AuthTTL)
				pipe.Set(ctx, sKey, shardPayload, in.ShardTTL)
				return nil
			})
			if perr != nil {
				return perr
			}
			out.ShardFound = true
			out.ShardState = shard.State
			out.ActiveGen = auth.Active.Gen
			out.ActiveJTI = auth.Active.Jti
			out.InstanceUID = auth.InstanceUid
			out.ProtocolEpoch = auth.ProtocolEpoch
			out.WriterEpoch = auth.Active.WriterEpoch
			return nil
		}, watchKeys...)
		if txErr == nil {
			if out.ShardFound {
				// 全局索引(与 {pod} 不同 slot,独立命令,幂等):心跳高频,失败下次即补。
				_ = r.rdb.SAdd(ctx, shardsSetKey, pod).Err()
				_ = r.rdb.ZAdd(ctx, activeKey, redis.Z{Score: float64(serverNowMs), Member: pod}).Err()
			}
			return out, nil
		}
		if bizErr != nil {
			return ActivateResult{}, bizErr
		}
		if txErr == redis.TxFailedErr {
			casConflictBackoff(ctx, attempt)
			continue
		}
		return ActivateResult{}, txErr
	}
	return ActivateResult{}, errcode.New(errcode.ErrUnavailable, "hub activate heartbeat %s: cas retry exhausted", pod)
}

// ReserveRoutableSeat 见 HubAuthRepo 接口注释。单事务(authKey+shardKey 同 slot):同时确认授权
// active + 分片 ready + 元组一致 + 心跳新鲜 + 未满,然后原子 seat++。任一条件不满足 → OK=false(零变更)。
func (r *RedisHubAuthRepo) ReserveRoutableSeat(ctx context.Context, pod string, nowMs, maxHeartbeatAgeMs int64, shardTTL time.Duration) (ReserveResult, error) {
	return ReserveResult{Reason: "deprecated-use-reserve-assignment"},
		errcode.New(errcode.ErrInvalidState, "integer hub seat reservation is disabled")
}

// CheckRoutable 是 ReserveRoutableSeat 的只读版本(不 seat++,不写键)。
func (r *RedisHubAuthRepo) CheckRoutable(ctx context.Context, pod string, nowMs, maxHeartbeatAgeMs int64) (ReserveResult, error) {
	return r.routable(ctx, pod, nowMs, maxHeartbeatAgeMs, 0, false)
}

// QuarantineExpected 把紧急吊销与普通 ROTATING 分开。它不接受“按 pod 名盲吊销”：
// 调用方必须提交当前完整 active 身份，防旧运维请求误隔离同名重建后的新 GameServer。
func (r *RedisHubAuthRepo) QuarantineExpected(
	ctx context.Context,
	pod string,
	expected CredentialIdentity,
	authTTL, shardTTL time.Duration,
) (QuarantineResult, error) {
	if pod == "" || expected.InstanceUID == "" || expected.ProtocolEpoch == 0 || expected.Gen == 0 ||
		expected.JTI == "" || expected.Kid == "" || expected.TokenSHA256 == "" ||
		expected.WriterEpoch != authpkg.DSAuthWriterEpochV2 || authTTL <= 0 || shardTTL <= 0 {
		return QuarantineResult{}, errcode.New(errcode.ErrInvalidArg, "hub quarantine requires full expected credential and ttls")
	}
	aKey, sKey := authKey(pod), shardKey(pod)
	for attempt := 0; attempt < hubAuthCASRetries; attempt++ {
		result := QuarantineResult{}
		txErr := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			authRaw, err := tx.Get(ctx, aKey).Bytes()
			if err == redis.Nil {
				return nil
			}
			if err != nil {
				return err
			}
			authRecord := &hubv1.HubShardAuthStorageRecord{}
			if err := proto.Unmarshal(authRaw, authRecord); err != nil {
				return err
			}
			if authRecord.GetPodName() != pod || !hubAuthRecordV2Exact(authRecord) ||
				expected.WriterEpoch != authpkg.DSAuthWriterEpochV2 ||
				authRecord.GetInstanceUid() != expected.InstanceUID ||
				authRecord.GetProtocolEpoch() != expected.ProtocolEpoch || authRecord.GetActive() == nil ||
				!credMatches(authRecord.GetActive(), expected) ||
				(authRecord.GetPhase() != hubv1.HubAuthPhase_HUB_AUTH_PHASE_ACTIVE &&
					authRecord.GetPhase() != hubv1.HubAuthPhase_HUB_AUTH_PHASE_ROTATING &&
					authRecord.GetPhase() != hubv1.HubAuthPhase_HUB_AUTH_PHASE_QUARANTINED) {
				return nil
			}
			var shard *hubv1.HubShardStorageRecord
			shardRaw, shardErr := tx.Get(ctx, sKey).Bytes()
			if shardErr == redis.Nil {
				shard = nil
			}
			if shardErr != nil {
				if shardErr != redis.Nil {
					return shardErr
				}
			}
			if shardErr == nil {
				var decodeErr error
				shard, decodeErr = unmarshalShard(pod, shardRaw)
				if decodeErr != nil {
					return decodeErr
				}
			}
			authRecord.Phase = hubv1.HubAuthPhase_HUB_AUTH_PHASE_QUARANTINED
			authRecord.Pending = nil
			authRecord.PendingStartedMs = 0
			authRecord.DeliveredRv = ""
			authRecord.UpdatedAtMs = time.Now().UnixMilli()
			authPayload, err := proto.Marshal(authRecord)
			if err != nil {
				return err
			}
			var shardPayload []byte
			projectionMatches := hubProjectionMatchesCredential(authRecord, shard, expected)
			if projectionMatches {
				if shard.State != "stopping" {
					shard.State = "draining"
				}
				if shard.DrainingSinceMs == 0 {
					shard.DrainingSinceMs = authRecord.UpdatedAtMs
				}
				shardPayload, err = marshalShard(shard)
				if err != nil {
					return err
				}
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				// 紧急吊销 tombstone 必须持久；有限 authTTL 会在 allocator 停机后
				// 自动丢失，并允许仍存活的同 UID GameServer 被重新 Init/Stage。
				pipe.Set(ctx, aKey, authPayload, 0)
				if shardPayload != nil {
					pipe.Set(ctx, sKey, shardPayload, shardTTL)
				}
				return nil
			})
			if err == nil {
				result.AuthQuarantined = true
				result.ProjectionDrained = projectionMatches
			}
			return err
		}, aKey, sKey)
		if txErr == redis.TxFailedErr {
			casConflictBackoff(ctx, attempt)
			continue
		}
		return result, txErr
	}
	return QuarantineResult{}, errcode.New(errcode.ErrUnavailable, "hub quarantine %s: cas retry exhausted", pod)
}

func hubProjectionMatchesCredential(
	authRecord *hubv1.HubShardAuthStorageRecord,
	shard *hubv1.HubShardStorageRecord,
	expected CredentialIdentity,
) bool {
	active := authRecord.GetActive()
	return authRecord != nil && shard != nil && active != nil &&
		hubAuthRecordV2Exact(authRecord) && active.GetWriterEpoch() == authpkg.DSAuthWriterEpochV2 &&
		authRecord.GetPodName() == shard.GetHubPodName() &&
		authRecord.GetInstanceUid() == expected.InstanceUID &&
		authRecord.GetProtocolEpoch() == expected.ProtocolEpoch &&
		shard.GetGameserverUid() == expected.InstanceUID && shard.GetAuthEpoch() == expected.ProtocolEpoch &&
		active.GetGen() == expected.Gen && active.GetJti() == expected.JTI &&
		active.GetWriterEpoch() == expected.WriterEpoch &&
		shard.GetLastVerifiedGen() == expected.Gen && shard.GetLastVerifiedJti() == expected.JTI &&
		shard.GetLastVerifiedWriterEpoch() == expected.WriterEpoch
}

func (r *RedisHubAuthRepo) routable(ctx context.Context, pod string, nowMs, maxHeartbeatAgeMs int64, shardTTL time.Duration, reserve bool) (ReserveResult, error) {
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	aKey, sKey := authKey(pod), shardKey(pod)
	var out ReserveResult
	for attempt := 0; attempt < hubAuthCASRetries; attempt++ {
		out = ReserveResult{}
		txErr := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			ab, gerr := tx.Get(ctx, aKey).Bytes()
			if gerr == redis.Nil {
				out.Reason = "auth-missing"
				return nil
			}
			if gerr != nil {
				return gerr
			}
			auth := &hubv1.HubShardAuthStorageRecord{}
			if uerr := proto.Unmarshal(ab, auth); uerr != nil {
				return uerr
			}
			if auth.PodName != pod {
				out.Reason = "auth-pod-mismatch"
				return nil
			}
			// 授权门:phase∈{ACTIVE,ROTATING}(轮换期 active 仍有效)、active 完整未过期。
			if auth.Phase != hubv1.HubAuthPhase_HUB_AUTH_PHASE_ACTIVE &&
				auth.Phase != hubv1.HubAuthPhase_HUB_AUTH_PHASE_ROTATING {
				out.Reason = "phase-not-active"
				return nil
			}
			if !routableCredentialComplete(auth.Active, auth, nowMs) {
				out.Reason = "no-active"
				return nil
			}
			// 分片门。
			sb, serr := tx.Get(ctx, sKey).Bytes()
			if serr == redis.Nil {
				out.Reason = "shard-missing"
				return nil
			}
			if serr != nil {
				return serr
			}
			shard, uerr := unmarshalShard(pod, sb)
			if uerr != nil {
				return uerr
			}
			if reason := modelBRoutableReason(auth, shard, pod, nowMs, maxHeartbeatAgeMs, nil); reason != "" {
				out.Reason = reason
				return nil
			}
			if shard.State != "ready" {
				out.Reason = "shard-not-ready"
				return nil
			}
			releaseTrack := shard.GetReleaseTrack()
			if releaseTrack == "" {
				releaseTrack = releasetrack.Stable // additive rollout migration for pre-track records
			}
			if !releasetrack.Valid(releaseTrack) {
				out.Reason = "shard-release-track-invalid"
				return nil
			}
			// active == shard.last_verified:确保分片镜像正是被当前 active 凭据投影的那份(把授权与镜像钉死)。
			if shard.LastVerifiedGen != auth.Active.Gen || shard.LastVerifiedJti != auth.Active.Jti {
				out.Reason = "shard-not-verified-by-active"
				return nil
			}
			if shard.GameserverUid != auth.InstanceUid || shard.AuthEpoch != auth.ProtocolEpoch {
				out.Reason = "shard-instance-mismatch"
				return nil
			}
			// 心跳新鲜度只认 auth 记录里由 Model B 原子激活路径写入的服务端时间。
			// legacy writer 只能改 shard.last_heartbeat_ms，无法借此越过 activation fence。
			if auth.LastActiveHeartbeatMs <= 0 || auth.LastActiveHeartbeatMs > nowMs {
				out.Reason = "heartbeat-invalid"
				return nil
			}
			if shard.LastVerifiedWriterEpoch != auth.Active.WriterEpoch {
				out.Reason = "shard-writer-epoch-mismatch"
				return nil
			}
			if maxHeartbeatAgeMs > 0 && nowMs-auth.LastActiveHeartbeatMs > maxHeartbeatAgeMs {
				out.Reason = "heartbeat-stale"
				return nil
			}
			// 通过授权 + 路由门,填回元组 + 分片信息。
			out.ActiveGen = auth.Active.Gen
			out.ActiveJTI = auth.Active.Jti
			out.InstanceUID = auth.InstanceUid
			out.ProtocolEpoch = auth.ProtocolEpoch
			out.WriterEpoch = auth.Active.WriterEpoch
			out.ShardID = shard.ShardId
			out.HubAddr = shard.HubAddr
			out.Region = shard.Region
			out.Capacity = shard.Capacity
			out.ReleaseTrack = releaseTrack
			if reserve {
				if shard.Capacity <= 0 || shard.PlayerCount < 0 || shard.PlayerCount >= shard.Capacity {
					out.Reason = "shard-full"
					return nil
				}
				shard.PlayerCount++
				shard.ReleaseTrack = releaseTrack
				payload, merr := marshalShard(shard)
				if merr != nil {
					return merr
				}
				_, perr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
					pipe.Set(ctx, sKey, payload, shardTTL)
					return nil
				})
				if perr != nil {
					return perr
				}
			} else {
				// 只读检查也必须执行一次 MULTI/EXEC，才能让 WATCH 在两次读取之间发生变化时
				// 返回 TxFailedErr；只 WATCH 不 EXEC 不具备快照一致性。
				if _, perr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
					pipe.Get(ctx, aKey)
					pipe.Get(ctx, sKey)
					return nil
				}); perr != nil {
					return perr
				}
			}
			out.PlayerCount = shard.PlayerCount
			out.OK = true
			return nil
		}, aKey, sKey)
		if txErr == nil {
			return out, nil
		}
		if txErr == redis.TxFailedErr {
			casConflictBackoff(ctx, attempt)
			continue
		}
		return ReserveResult{}, txErr
	}
	return ReserveResult{}, errcode.New(errcode.ErrUnavailable, "hub reserve routable %s: cas retry exhausted", pod)
}

// ReleaseRoutableSeat 仅在当前 active 与 expected 四元组、分片投影仍完全一致时退一个座位。
// UID 重建/轮换后旧归属的清理不会误减新实例计数；前置不符返回 false 且零变更。
func (r *RedisHubAuthRepo) ReleaseRoutableSeat(ctx context.Context, pod string, expected CredentialIdentity, shardTTL time.Duration) (bool, error) {
	if expected.InstanceUID == "" || expected.ProtocolEpoch == 0 || expected.Gen == 0 || expected.JTI == "" ||
		expected.WriterEpoch != authpkg.DSAuthWriterEpochV2 {
		return false, nil
	}
	aKey, sKey := authKey(pod), shardKey(pod)
	for attempt := 0; attempt < hubAuthCASRetries; attempt++ {
		released := false
		txErr := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			ab, err := tx.Get(ctx, aKey).Bytes()
			if err == redis.Nil {
				return nil
			}
			if err != nil {
				return err
			}
			auth := &hubv1.HubShardAuthStorageRecord{}
			if err := proto.Unmarshal(ab, auth); err != nil {
				return err
			}
			if !hubAuthRecordV2Exact(auth) || expected.WriterEpoch != authpkg.DSAuthWriterEpochV2 ||
				auth.InstanceUid != expected.InstanceUID || auth.ProtocolEpoch != expected.ProtocolEpoch ||
				auth.Active == nil || auth.Active.Gen != expected.Gen || auth.Active.Jti != expected.JTI ||
				auth.Active.WriterEpoch != expected.WriterEpoch {
				return nil
			}
			sb, err := tx.Get(ctx, sKey).Bytes()
			if err == redis.Nil {
				return nil
			}
			if err != nil {
				return err
			}
			shard, err := unmarshalShard(pod, sb)
			if err != nil {
				return err
			}
			if shard.GameserverUid != expected.InstanceUID || shard.AuthEpoch != expected.ProtocolEpoch ||
				shard.LastVerifiedGen != expected.Gen || shard.LastVerifiedJti != expected.JTI {
				return nil
			}
			if shard.LastVerifiedWriterEpoch != expected.WriterEpoch {
				return nil
			}
			if shard.PlayerCount > 0 {
				shard.PlayerCount--
			}
			payload, err := marshalShard(shard)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, sKey, payload, shardTTL)
				return nil
			})
			if err == nil {
				released = true
			}
			return err
		}, aKey, sKey)
		if txErr == redis.TxFailedErr {
			casConflictBackoff(ctx, attempt)
			continue
		}
		if txErr != nil {
			return false, txErr
		}
		return released, nil
	}
	return false, errcode.New(errcode.ErrUnavailable, "hub release routable %s: cas retry exhausted", pod)
}

// ReleaseAssignmentSeat 只允许同一 GameServer UID/epoch 的当前 V2 active 投影退座。
// assignment key 与 {pod} slot 不同，调用方先 CAS 删除归属；本事务不再要求旧 gen/jti
// 等于当前 active，从而允许 CAS 窗口内的普通凭据轮换，同时 UID 重建仍零变更。
func (r *RedisHubAuthRepo) releaseAssignmentSeatLegacy(
	ctx context.Context,
	pod string,
	expected AssignmentInstanceIdentity,
	shardTTL time.Duration,
) (bool, error) {
	if pod == "" || expected.InstanceUID == "" || expected.ProtocolEpoch == 0 ||
		expected.WriterEpoch != authpkg.DSAuthWriterEpochV2 || shardTTL <= 0 {
		return false, nil
	}
	aKey, sKey := authKey(pod), shardKey(pod)
	for attempt := 0; attempt < hubAuthCASRetries; attempt++ {
		released := false
		txErr := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			aRaw, err := tx.Get(ctx, aKey).Bytes()
			if err == redis.Nil {
				return nil
			}
			if err != nil {
				return err
			}
			sRaw, err := tx.Get(ctx, sKey).Bytes()
			if err == redis.Nil {
				return nil
			}
			if err != nil {
				return err
			}
			authRecord := &hubv1.HubShardAuthStorageRecord{}
			if err := proto.Unmarshal(aRaw, authRecord); err != nil {
				return err
			}
			shard, err := unmarshalShard(pod, sRaw)
			if err != nil {
				return err
			}
			active := authRecord.GetActive()
			if !hubAuthRecordV2Exact(authRecord) || active == nil ||
				authRecord.GetPodName() != pod || authRecord.GetInstanceUid() != expected.InstanceUID ||
				authRecord.GetProtocolEpoch() != expected.ProtocolEpoch ||
				active.GetInstanceUid() != expected.InstanceUID || active.GetProtocolEpoch() != expected.ProtocolEpoch ||
				active.GetGen() == 0 || active.GetJti() == "" || active.GetKid() == "" ||
				active.GetTokenSha256() == "" || active.GetWriterEpoch() != expected.WriterEpoch ||
				authRecord.GetHighWaterGen() < active.GetGen() ||
				shard.GetGameserverUid() != expected.InstanceUID || shard.GetAuthEpoch() != expected.ProtocolEpoch ||
				shard.GetLastVerifiedGen() != active.GetGen() || shard.GetLastVerifiedJti() != active.GetJti() ||
				shard.GetLastVerifiedWriterEpoch() != expected.WriterEpoch {
				return nil
			}
			if shard.PlayerCount > 0 {
				shard.PlayerCount--
			}
			payload, err := marshalShard(shard)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, sKey, payload, shardTTL)
				return nil
			})
			if err == nil {
				released = true
			}
			return err
		}, aKey, sKey)
		if txErr == redis.TxFailedErr {
			casConflictBackoff(ctx, attempt)
			continue
		}
		if txErr != nil {
			return false, txErr
		}
		return released, nil
	}
	return false, errcode.New(errcode.ErrUnavailable, "hub assignment release %s: cas retry exhausted", pod)
}

func routableCredentialComplete(cred *hubv1.HubDSCredential, auth *hubv1.HubShardAuthStorageRecord, nowMs int64) bool {
	return cred != nil && auth != nil && auth.PodName != "" && auth.InstanceUid != "" && auth.ProtocolEpoch > 0 &&
		cred.Gen > 0 && cred.Jti != "" && cred.ExpMs > uint64(nowMs) && cred.Kid != "" &&
		cred.TokenSha256 != "" && cred.InstanceUid == auth.InstanceUid && cred.ProtocolEpoch == auth.ProtocolEpoch &&
		hubAuthRecordV2Exact(auth) && cred.WriterEpoch == authpkg.DSAuthWriterEpochV2
}
