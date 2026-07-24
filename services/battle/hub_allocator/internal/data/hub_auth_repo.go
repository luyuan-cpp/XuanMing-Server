// hub_auth_repo.go 是 Model B「Redis 唯一授权权威」的授权记录数据层
// (decision-revisit-ds-callback-auth §7)。
//
// Redis key 模板:
//
//	pandora:hub:auth:{<hub_pod_name>}  → HubShardAuthStorageRecord proto bytes
//
// 与分片镜像 pandora:hub:shard:{pod}、代际计数器 pandora:hub:tokengen:{pod} 同 {pod}
// hashtag,保证同 slot 可事务;所有状态迁移走 WATCH/MULTI/EXEC 乐观锁(与 hub_repo 一致,
// 不用 Lua,便于 miniredis 测试)。read-modify-write 用默认 proto.Unmarshal(不 DiscardUnknown,
// 不变量 §17:滚动更新期旧副本回写不得静默丢新字段)。
package data

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/errcode"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
)

func authKey(pod string) string { return fmt.Sprintf("pandora:hub:auth:{%s}", pod) }

// CredentialIdentity 是 DS 心跳携带的、已验签的完整凭据身份(gen/jti/instance_uid/
// protocol_epoch/kid/writer_epoch)+ token_sha256 完整性绑定。Model B 用它做 promote/validate 匹配:
// gen 单独不安全(计数器 TTL 复位可致 gen 复用),必须联合 uid+epoch+jti 才能唯一锁定。
type CredentialIdentity struct {
	Gen           uint64
	JTI           string
	InstanceUID   string
	ProtocolEpoch uint32
	TokenSHA256   string
	Kid           string
	WriterEpoch   uint32
}

// AssignmentInstanceIdentity 是赢得 assignment CAS 后退座所需的稳定实例身份。
// 凭据 gen/jti 可在“校验 assignment→CAS 删除”之间正常轮换；UID/epoch/writer 不可变。
type AssignmentInstanceIdentity struct {
	PlayerID             uint64
	AssignmentID         string
	InstanceUID          string
	ProtocolEpoch        uint32
	WriterEpoch          uint32
	PlacementVersion     uint64
	PlacementOperationID string
	SourceMatchID        uint64
}

// ReleaseAssignmentSeatResult distinguishes an idempotent replay from an
// identity conflict. A bool cannot safely drive a durable cleanup worker:
// false previously meant both “already absent” and “a different owner exists”.
type ReleaseAssignmentSeatResult struct {
	Released      bool
	AlreadyAbsent bool
	Conflict      bool
	// DepartureRequired means the exact identity is a live connected owner.
	// Capacity-ledger cleanup is not physical eviction proof and must not delete
	// it; the source DS must kick the matching connection and ACK Departure (or
	// its GameServer UID must be authoritatively torn down).
	DepartureRequired bool
}

// AssignmentSeatSnapshot is the read-only physical-owner view used by the
// durable cleanup coordinator. Exactly one of the four state booleans is true.
// Admission identity is populated only for Connected.
type AssignmentSeatSnapshot struct {
	Reserved               bool
	Connected              bool
	AlreadyAbsent          bool
	Conflict               bool
	ReservationExpiresAtMs int64
	AdmissionID            string
	AdmissionSeq           uint64
}

// ReservationIdentity 是逐 assignment 容量 lease 的稳定身份。凭据 gen/jti 会平滑轮换，
// reservation/session 只绑定不会在同一 GameServer 生命周期内变化的 UID/epoch/writer。
type ReservationIdentity struct {
	PlayerID      uint64
	AssignmentID  string
	InstanceUID   string
	ProtocolEpoch uint32
	WriterEpoch   uint32
	// Placement* is optional for a first-time capacity reservation, because the
	// assignment/placement saga is persisted after the seat is selected.  It is
	// mandatory at the business layer whenever an existing connected assignment
	// is re-signed: the successor lease is keyed by this exact version+operation
	// so a ticket for another placement operation cannot consume it.
	PlacementVersion      uint64
	PlacementOperationID  string
	SourceMatchID         uint64
	ExpiresAtMs           int64
	AssignmentExpiresAtMs int64
}

// AdmissionResult 是 Hub DS 真接纳 ACK 的幂等结果。
type AdmissionResult struct {
	Admitted          bool
	AlreadyAdmitted   bool
	Conflict          bool
	ReservedCount     int32
	ConnectedCount    int32
	CapacityOccupancy int32
}

// DepartureResult 是 Hub DS exact Logout ACK 的结果。Conflict=true 表示同 assignment
// 已由另一个 admission_id 接管；旧连接晚到 Logout 必须零副作用停止重试。
type DepartureResult struct {
	Departed          bool
	AlreadyDeparted   bool
	Conflict          bool
	ReservedCount     int32
	ConnectedCount    int32
	CapacityOccupancy int32
}

// errAuthStale:Model B 授权校验失败(无授权记录 / uid|epoch 不符 / gen 非当前 / 凭据不匹配)。
// 用 ErrUnauthorized(=8)对 DS 呈现明确鉴权拒绝码,fail-closed:记录零变更。
var errAuthStale = errcode.New(errcode.ErrUnauthorized, "hub ds credential not authoritative")

// 同一 Hub 的高并发 Assign/Transfer/Heartbeat 会争用 auth+shard 两键；冲突重读不重放
// 外部副作用，使用足够预算让真实并发收敛，耗尽仍 fail-closed(返回可重试的
// ErrUnavailable:预算耗尽只说明瞬时争用,不是内部不变量被破坏)。
// 每次冲突后必须经 casConflictBackoff 退避,不得紧循环重试。
const hubAuthCASRetries = 64

// casConflictBackoff 在 WATCH/CAS 乐观并发冲突后按指数 + 抖动退避,再进入下一次重试。
// 零间隔紧循环在高并发争用同 {pod}/同玩家键时会互相踩踏:每次 EXEC 成功都会打断其余
// 全部在途 WATCH 事务,落后者可能连续输掉全部预算(-race/慢盘等减速环境尤甚;
// 2026-07-21 存量 flake:32 并发 AssignHub 把 64 次预算全部冲突耗尽后报 exhausted)。
// 首次冲突立即重试保住低争用延迟;此后自 1ms 指数升至 16ms 封顶,叠加 [-50%,+50%)
// 抖动打散同拍;ctx 取消/超时立即停止等待,由外层事务错误路径收尾。
func casConflictBackoff(ctx context.Context, attempt int) {
	if attempt <= 0 {
		return
	}
	shift := attempt - 1
	if shift > 4 {
		shift = 4
	}
	base := time.Millisecond << shift
	delay := base/2 + rand.N(base)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// phaseLocked 判定授权相位是否已锁定(不再接受新 stage / promote / 分配):
// QUARANTINED(紧急吊销)与 TERMINATING(实例下线中)一律拒绝写入侧与授权侧副作用,
// 只能经显式受控 purge/recreate 恢复；普通 InitAuth（包括换 UID）也不得绕过 tombstone。
func phaseLocked(p hubv1.HubAuthPhase) bool {
	return p == hubv1.HubAuthPhase_HUB_AUTH_PHASE_QUARANTINED ||
		p == hubv1.HubAuthPhase_HUB_AUTH_PHASE_TERMINATING
}

// HubAuthRepo 是 Model B 授权记录数据层抽象。biz 层只依赖此接口。
type HubAuthRepo interface {
	// GetAuth 读授权记录。not found → (nil, false, nil)。
	GetAuth(ctx context.Context, pod string) (*hubv1.HubShardAuthStorageRecord, bool, error)
	// InitAuth 确保授权记录存在并绑定当前 DS 实例身份(instanceUID):
	//   - 不存在 → 建 BOOTSTRAP(绑 uid,epoch=1,high_water_gen=0)。
	//   - 存在但 uid 变了(换 DS 实例)→ 复位:清 active/pending,phase=BOOTSTRAP,绑新 uid,
	//     protocol_epoch 递增(抗计数器复位重放),high_water_gen 保留(单调不回退)。
	//   - 存在且 uid 相同 → 幂等不动。
	// 返回最新记录(供签发方读 protocol_epoch / high_water_gen)。
	InitAuth(ctx context.Context, pod, instanceUID string, authTTL time.Duration) (*hubv1.HubShardAuthStorageRecord, error)
	// StagePending 暂存 pending 凭据(WATCH CAS):要求 uid/epoch/gen/jti/kid/hash/exp/writer_epoch 完整且未过期、
	// 授权记录存在、uid/epoch 匹配、cred.gen > high_water_gen 且 > active.gen(若有)。成功后 pending=cred,
	// high_water_gen=cred.gen,phase=ROTATING(已有 active)/保持 BOOTSTRAP(无 active)。
	StagePending(ctx context.Context, pod string, cred *hubv1.HubDSCredential, authTTL time.Duration) (*hubv1.HubShardAuthStorageRecord, error)
	// MarkDelivered 记 expected pending 已 PATCH 投递到某 GameServer resourceVersion。
	// expected 必须包含 uid/epoch/gen/jti/kid/hash/exp/writer_epoch 完整 tuple,且事务提交时仍与当前 pending
	// 完全一致;旧 PATCH 响应晚到时不得把 delivered_rv 写到更高代际 pending。
	MarkDelivered(ctx context.Context, pod string, expected *hubv1.HubDSCredential, rv string, authTTL time.Duration) error
	// ActivateHeartbeat 是 Model B 唯一线性化点(审核二轮 CE1/CE2/CE4/CE6/CE8):DS 携带已验签凭据
	// 身份心跳时,在 authKey + shardKey **同 {pod} slot 单事务**里原子完成:①校验凭据(uid/epoch/相位/
	// 匹配 pending 或 active);②匹配 pending 则 promote pending→active;③把本次心跳应用到分片镜像
	// (warming→ready、player_count、state、last_heartbeat)并把 active 元组投影进 last_verified/uid/epoch。
	// promote 与分片写在同一 EXEC,杜绝「promote 成功但分片写失败/进程崩溃」的半激活(CE4)。
	// 分片镜像不存在(孤儿 / 早于拓扑种子)→ ActivateResult.ShardFound=false 且**不 promote**,由 biz 先
	// reconcile 拓扑再重试,保证 promote 与 ready 始终同事务。stale/相位锁定/都不匹配 → errAuthStale(零变更)。
	// authTTL/shardTTL 分别用于两键(CE8:授权记录不被 shardTTL 缩短)。
	ActivateHeartbeat(ctx context.Context, pod string, id CredentialIdentity, in ActivateHeartbeatInput) (ActivateResult, error)
	// ReserveAssignment 在 auth+shard+reservation/session 同 {pod} slot 事务中清理到期 lease、
	// 核验 active/projection/MaxPlayers/心跳与容量，再按 (player,assignment,UID,epoch,writer)
	// 创建或刷新逐 reservation；若同 assignment 已是 connected ownership 则幂等成功且不重复计数。
	ReserveAssignment(ctx context.Context, pod string, reservation ReservationIdentity, nowMs, maxHeartbeatAgeMs int64, shardTTL time.Duration) (ReserveResult, error)
	// AcknowledgeAdmission 由持当前 active callback credential 的 Hub DS 在真接纳后调用；
	// reservation->connected ownership 与容量投影在同槽一次 EXEC 完成，响应丢失重试幂等。
	AcknowledgeAdmission(ctx context.Context, pod string, credential CredentialIdentity, reservation ReservationIdentity,
		admissionID string, admissionSeq uint64, nowMs int64, shardTTL time.Duration) (AdmissionResult, error)
	// AcknowledgeDeparture 仅删除完整 (player,assignment,admission,pod,UID,epoch,writer)
	// identity 对应的 connected ownership。同 assignment 新 admission 已接管时返回 Conflict 且零副作用。
	AcknowledgeDeparture(ctx context.Context, pod string, credential CredentialIdentity, reservation ReservationIdentity,
		admissionID string, admissionSeq uint64, nowMs int64, shardTTL time.Duration) (DepartureResult, error)
	// ReleaseAssignmentSeat 只回收尚未 Admission 的 reservation。live connected owner
	// 返回 false，必须等 source DS exact Departure/UID teardown；Redis 退座不等于物理退出。
	ReleaseAssignmentSeat(ctx context.Context, pod string, expected AssignmentInstanceIdentity, shardTTL time.Duration) (bool, error)
	// ReleaseAssignmentSeatExact 是 durable cleanup/outbox 使用的状态版本。只有 Released 或
	// AlreadyAbsent 可推进；Conflict fail-closed，DepartureRequired 必须下发 eviction 并等待物理 proof。
	ReleaseAssignmentSeatExact(ctx context.Context, pod string, expected AssignmentInstanceIdentity,
		shardTTL time.Duration) (ReleaseAssignmentSeatResult, error)
	// InspectAssignmentSeat never mutates the ledger. A connected result is the
	// exact source-DS eviction order; its absence must be proven later by
	// AcknowledgeDeparture or confirmed UID teardown, never by this reader.
	InspectAssignmentSeat(ctx context.Context, pod string,
		expected AssignmentInstanceIdentity) (AssignmentSeatSnapshot, error)
	// RemoveCapacityLedger 删除已确认回收分片的 reservation/session 派生键；只由 RemoveShard 后调用。
	RemoveCapacityLedger(ctx context.Context, pod string) error
	// CheckRoutable 是 ReserveRoutableSeat 的只读版本(不 seat++):幂等重签 / 复用已有归属时校验目标
	// 分片当前是否可路由并取回当前 active 元组,供比对归属记录钉的元组是否仍等于当前 active(实例漂移即失效)。
	CheckRoutable(ctx context.Context, pod string, nowMs, maxHeartbeatAgeMs int64) (ReserveResult, error)
	// QuarantineExpected 是泄露凭据的紧急 fail-closed 路径。只有 expected 仍精确等于
	// 当前 active tuple 才把 auth 锁为 QUARANTINED，并在同槽事务把 shard 置 draining。
	QuarantineExpected(context.Context, string, CredentialIdentity, time.Duration, time.Duration) (QuarantineResult, error)
}

// HubInstanceTeardownProofRepo is an optional exact-UID proof store used by
// the topology reconciler.  A proof is minted only after an unfiltered
// GameServer+Pod observation confirms that the expected UID is physically
// gone.  It is intentionally separate from HubAuthRepo so older test fakes
// remain fail-closed (they simply cannot manufacture teardown proof).
type HubInstanceTeardownProofRepo interface {
	RecordInstanceTeardownProof(ctx context.Context, pod, instanceUID string, proofTTL time.Duration) error
	HasInstanceTeardownProof(ctx context.Context, pod, instanceUID string) (bool, error)
}

// RedisHubAuthRepo 是基于 go-redis/v9 的 HubAuthRepo 实现。
type RedisHubAuthRepo struct {
	rdb redis.UniversalClient
	// fence:写者继任 fencing token 源(writer_fence.go;nil = 未启用,保持原行为)。
	fence WriterFence
}

// NewRedisHubAuthRepo 构造 RedisHubAuthRepo。
func NewRedisHubAuthRepo(rdb redis.UniversalClient) *RedisHubAuthRepo {
	return &RedisHubAuthRepo{rdb: rdb}
}

// SetWriterFence 注入写者继任 fencing(Model B 生产由 main 注入;见 writer_fence.go)。
func (r *RedisHubAuthRepo) SetWriterFence(f WriterFence) { r.fence = f }

func (r *RedisHubAuthRepo) GetAuth(ctx context.Context, pod string) (*hubv1.HubShardAuthStorageRecord, bool, error) {
	b, err := r.rdb.Get(ctx, authKey(pod)).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	rec := &hubv1.HubShardAuthStorageRecord{}
	if uerr := proto.Unmarshal(b, rec); uerr != nil {
		return nil, false, fmt.Errorf("unmarshal hub auth %s: %w", pod, uerr)
	}
	return rec, true, nil
}

func (r *RedisHubAuthRepo) InitAuth(ctx context.Context, pod, instanceUID string, authTTL time.Duration) (*hubv1.HubShardAuthStorageRecord, error) {
	if instanceUID == "" {
		return nil, errcode.New(errcode.ErrInvalidArg, "hub auth init requires instance uid")
	}
	key := authKey(pod)
	var out *hubv1.HubShardAuthStorageRecord
	for attempt := 0; attempt < hubAuthCASRetries; attempt++ {
		txErr := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			advanceFence, ferr := guardWriterFence(ctx, tx, pod, r.fence)
			if ferr != nil {
				return ferr
			}
			rec := &hubv1.HubShardAuthStorageRecord{}
			b, gerr := tx.Get(ctx, key).Bytes()
			switch {
			case gerr == redis.Nil:
				// 首建:BOOTSTRAP。
				rec.PodName = pod
				rec.InstanceUid = instanceUID
				rec.ProtocolEpoch = 1
				rec.Phase = hubv1.HubAuthPhase_HUB_AUTH_PHASE_BOOTSTRAP
				rec.RequiredWriterEpoch = auth.DSAuthWriterEpochV2
			case gerr != nil:
				return gerr
			default:
				if uerr := proto.Unmarshal(b, rec); uerr != nil {
					return uerr
				}
				// future writer 的权威记录只能由对应未来二进制处理。V2 不得在
				// InitAuth 中复位实例、刷新 TTL 或覆盖 unknown fields。
				if rec.RequiredWriterEpoch != auth.DSAuthWriterEpochV2 || !hubStoredCredentialEpochsV2(rec) {
					return errAuthStale
				}
				// QUARANTINED/TERMINATING 是显式运维 tombstone。普通 Fleet 对账不能
				// 因同 UID、换 UID 或刷新 TTL 将它复活；恢复只能走受控 purge/recreate。
				if phaseLocked(rec.Phase) {
					return errAuthStale
				}
				if rec.InstanceUid != instanceUID {
					// 换实例:复位,epoch 递增(抗计数器复位重放),high_water_gen 单调保留。
					rec.PodName = pod
					rec.InstanceUid = instanceUID
					rec.ProtocolEpoch++
					if rec.ProtocolEpoch == 0 {
						rec.ProtocolEpoch = 1
					}
					rec.Phase = hubv1.HubAuthPhase_HUB_AUTH_PHASE_BOOTSTRAP
					rec.Active = nil
					rec.Pending = nil
					rec.PendingStartedMs = 0
					rec.DeliveredRv = ""
				}
			}
			rec.UpdatedAtMs = time.Now().UnixMilli()
			payload, merr := proto.Marshal(rec)
			if merr != nil {
				return merr
			}
			if _, perr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				advanceFence(pipe)
				pipe.Set(ctx, key, payload, authTTL)
				return nil
			}); perr != nil {
				return perr
			}
			out = rec
			return nil
		}, fencedWatchKeys([]string{key}, pod, r.fence)...)
		if txErr == nil {
			return out, nil
		}
		if txErr == redis.TxFailedErr {
			casConflictBackoff(ctx, attempt)
			continue
		}
		return nil, txErr
	}
	return nil, errcode.New(errcode.ErrUnavailable, "hub auth init %s: cas retry exhausted", pod)
}

func (r *RedisHubAuthRepo) StagePending(ctx context.Context, pod string, cred *hubv1.HubDSCredential, authTTL time.Duration) (*hubv1.HubShardAuthStorageRecord, error) {
	if verr := validateStoredCredential(cred, time.Now().UnixMilli()); verr != nil {
		return nil, verr
	}
	key := authKey(pod)
	var out *hubv1.HubShardAuthStorageRecord
	for attempt := 0; attempt < hubAuthCASRetries; attempt++ {
		var bizErr error
		txErr := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			advanceFence, ferr := guardWriterFence(ctx, tx, pod, r.fence)
			if ferr != nil {
				return ferr
			}
			b, gerr := tx.Get(ctx, key).Bytes()
			if gerr == redis.Nil {
				bizErr = errAuthStale // 未 InitAuth
				return bizErr
			}
			if gerr != nil {
				return gerr
			}
			rec := &hubv1.HubShardAuthStorageRecord{}
			if uerr := proto.Unmarshal(b, rec); uerr != nil {
				return uerr
			}
			if phaseLocked(rec.Phase) {
				bizErr = errAuthStale // QUARANTINED/TERMINATING:相位锁定,拒绝新 pending(CE9-iii)
				return bizErr
			}
			if rec.InstanceUid != cred.InstanceUid || rec.ProtocolEpoch != cred.ProtocolEpoch {
				bizErr = errAuthStale // 实例/纪元不符
				return bizErr
			}
			if !hubAuthRecordV2Exact(rec) || cred.WriterEpoch != auth.DSAuthWriterEpochV2 {
				bizErr = errAuthStale
				return bizErr
			}
			if cred.Gen <= rec.HighWaterGen {
				bizErr = errAuthStale // gen 必须严格高于历史水位(杜绝复用/回退)
				return bizErr
			}
			if rec.Active != nil && cred.Gen <= rec.Active.Gen {
				bizErr = errAuthStale
				return bizErr
			}
			// 保存克隆,避免调用方在 StagePending 返回后继续修改同一 proto 指针。
			rec.Pending = proto.Clone(cred).(*hubv1.HubDSCredential)
			rec.HighWaterGen = cred.Gen
			rec.PendingStartedMs = time.Now().UnixMilli()
			rec.DeliveredRv = ""
			if rec.Active != nil {
				rec.Phase = hubv1.HubAuthPhase_HUB_AUTH_PHASE_ROTATING
			} else {
				rec.Phase = hubv1.HubAuthPhase_HUB_AUTH_PHASE_BOOTSTRAP
			}
			rec.UpdatedAtMs = time.Now().UnixMilli()
			payload, merr := proto.Marshal(rec)
			if merr != nil {
				return merr
			}
			if _, perr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				advanceFence(pipe)
				pipe.Set(ctx, key, payload, authTTL)
				return nil
			}); perr != nil {
				return perr
			}
			out = rec
			return nil
		}, fencedWatchKeys([]string{key}, pod, r.fence)...)
		if txErr == nil {
			return out, nil
		}
		if bizErr != nil {
			return nil, bizErr
		}
		if txErr == redis.TxFailedErr {
			casConflictBackoff(ctx, attempt)
			continue
		}
		return nil, txErr
	}
	return nil, errcode.New(errcode.ErrUnavailable, "hub auth stage %s: cas retry exhausted", pod)
}

func (r *RedisHubAuthRepo) MarkDelivered(ctx context.Context, pod string, expected *hubv1.HubDSCredential, rv string, authTTL time.Duration) error {
	if verr := validateStoredCredential(expected, time.Now().UnixMilli()); verr != nil {
		return verr
	}
	if rv == "" {
		return errcode.New(errcode.ErrInvalidArg, "hub auth mark delivered requires resource version")
	}
	key := authKey(pod)
	for attempt := 0; attempt < hubAuthCASRetries; attempt++ {
		var bizErr error
		txErr := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			advanceFence, ferr := guardWriterFence(ctx, tx, pod, r.fence)
			if ferr != nil {
				return ferr
			}
			b, gerr := tx.Get(ctx, key).Bytes()
			if gerr == redis.Nil {
				bizErr = errAuthStale
				return bizErr
			}
			if gerr != nil {
				return gerr
			}
			rec := &hubv1.HubShardAuthStorageRecord{}
			if uerr := proto.Unmarshal(b, rec); uerr != nil {
				return uerr
			}
			if phaseLocked(rec.Phase) || !hubAuthRecordV2Exact(rec) ||
				expected.WriterEpoch != auth.DSAuthWriterEpochV2 ||
				rec.InstanceUid != expected.InstanceUid ||
				rec.ProtocolEpoch != expected.ProtocolEpoch ||
				!storedCredentialEqual(rec.Pending, expected) {
				bizErr = errAuthStale
				return bizErr
			}
			rec.DeliveredRv = rv
			rec.UpdatedAtMs = time.Now().UnixMilli()
			payload, merr := proto.Marshal(rec)
			if merr != nil {
				return merr
			}
			_, perr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				advanceFence(pipe)
				pipe.Set(ctx, key, payload, authTTL)
				return nil
			})
			return perr
		}, fencedWatchKeys([]string{key}, pod, r.fence)...)
		if txErr == nil {
			return nil
		}
		if bizErr != nil {
			return bizErr
		}
		if txErr == redis.TxFailedErr {
			casConflictBackoff(ctx, attempt)
			continue
		}
		return txErr
	}
	return errcode.New(errcode.ErrUnavailable, "hub auth mark delivered %s: cas retry exhausted", pod)
}

// validateStoredCredential 拒绝不完整或已过期的权威凭据。JWT 本身只在投递/中间件处验签,
// Redis 记录至少必须完整保存能唯一识别凭据的 uid/epoch/gen/jti/kid/hash/exp/writer_epoch。
func validateStoredCredential(c *hubv1.HubDSCredential, nowMs int64) error {
	if c == nil || c.Gen == 0 || c.Jti == "" || c.InstanceUid == "" || c.ProtocolEpoch == 0 ||
		c.Kid == "" || c.TokenSha256 == "" || c.ExpMs == 0 || c.WriterEpoch == 0 {
		return errcode.New(errcode.ErrInvalidArg, "hub auth credential requires uid/epoch/gen/jti/kid/hash/exp/writer_epoch")
	}
	if c.WriterEpoch != auth.DSAuthWriterEpochV2 {
		return errAuthStale
	}
	if c.ExpMs <= uint64(nowMs) {
		return errcode.New(errcode.ErrInvalidArg, "hub auth credential already expired")
	}
	return nil
}

// storedCredentialEqual 比较当前协议已知的完整凭据字段。不要用 proto.Equal:滚动更新期间
// 新版本可能追加 unknown fields,旧副本必须保留但不能因不认识新字段而错误覆盖/拒绝同一 tuple。
func storedCredentialEqual(a, b *hubv1.HubDSCredential) bool {
	return a != nil && b != nil &&
		a.Gen == b.Gen && a.Jti == b.Jti && a.ExpMs == b.ExpMs && a.Kid == b.Kid &&
		a.InstanceUid == b.InstanceUid && a.ProtocolEpoch == b.ProtocolEpoch &&
		a.TokenSha256 == b.TokenSha256 && a.WriterEpoch == b.WriterEpoch
}

// credMatches 判定心跳凭据身份与已存凭据是否同一份。hash 不允许缺失后降级为只比 gen+jti;
// uid/epoch 同时绑定凭据内字段和记录级字段,防损坏记录或计数器回退造成身份混淆。
func credMatches(c *hubv1.HubDSCredential, id CredentialIdentity) bool {
	if validateStoredCredential(c, time.Now().UnixMilli()) != nil ||
		id.Gen == 0 || id.JTI == "" || id.InstanceUID == "" || id.ProtocolEpoch == 0 || id.TokenSHA256 == "" ||
		id.Kid == "" || id.WriterEpoch != auth.DSAuthWriterEpochV2 {
		return false
	}
	if c.Gen != id.Gen || c.Jti != id.JTI || c.InstanceUid != id.InstanceUID || c.ProtocolEpoch != id.ProtocolEpoch ||
		c.Kid != id.Kid || c.WriterEpoch != id.WriterEpoch {
		return false
	}
	return c.TokenSha256 == id.TokenSHA256
}

func hubStoredCredentialEpochsV2(rec *hubv1.HubShardAuthStorageRecord) bool {
	return rec != nil &&
		(rec.Active == nil || rec.Active.WriterEpoch == auth.DSAuthWriterEpochV2) &&
		(rec.Pending == nil || rec.Pending.WriterEpoch == auth.DSAuthWriterEpochV2)
}

func hubAuthRecordV2Exact(rec *hubv1.HubShardAuthStorageRecord) bool {
	return rec != nil && rec.RequiredWriterEpoch == auth.DSAuthWriterEpochV2 &&
		hubStoredCredentialEpochsV2(rec)
}
