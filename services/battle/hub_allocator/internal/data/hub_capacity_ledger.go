// hub_capacity_ledger.go 实现 Hub 的逐 reservation / connected ownership 容量权威。
//
// 四把 ledger key 与 auth/shard 共用 {pod} hashtag，可在一次 WATCH/MULTI/EXEC 中完成：
//
//	pandora:hub:reservations:{pod}         HASH assignment_id -> HubReservationStorageRecord
//	pandora:hub:reservation-expiry:{pod}  ZSET assignment_id -> expires_at_ms
//	pandora:hub:sessions:{pod}             HASH assignment_id -> HubConnectedOwnershipStorageRecord
//	pandora:hub:session-expiry:{pod}       ZSET assignment_id -> expires_at_ms
//
// player_count 只由 session+reservation 条数派生；Heartbeat 的 reported count/list 只写
// 审计字段，绝不能覆盖 reservation 或推断连接离场。connected ownership 新格式没有时间 TTL，
// 只由 exact Departure 或已确认的 UID teardown 删除；Release/Transfer 只能下发物理 eviction
// 并等待该 proof，不能直接删 ledger 冒充 Pawn 已退出。所有 protobuf read-modify-write 保留
// unknown fields（不变量 §17）。
package data

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	authpkg "github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/errcode"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
)

const capacityLedgerRetentionGuard = 30 * time.Second

func reservationsKey(pod string) string {
	return fmt.Sprintf("pandora:hub:reservations:{%s}", pod)
}

func reservationExpiryKey(pod string) string {
	return fmt.Sprintf("pandora:hub:reservation-expiry:{%s}", pod)
}

func sessionsKey(pod string) string {
	return fmt.Sprintf("pandora:hub:sessions:{%s}", pod)
}

func sessionExpiryKey(pod string) string {
	return fmt.Sprintf("pandora:hub:session-expiry:{%s}", pod)
}

func capacityLedgerKeys(pod string) []string {
	return []string{reservationsKey(pod), reservationExpiryKey(pod), sessionsKey(pod), sessionExpiryKey(pod)}
}

type hubCapacityLedger struct {
	reservations map[string]*hubv1.HubReservationStorageRecord
	sessions     map[string]*hubv1.HubConnectedOwnershipStorageRecord
}

func newHubCapacityLedger() *hubCapacityLedger {
	return &hubCapacityLedger{
		reservations: make(map[string]*hubv1.HubReservationStorageRecord),
		sessions:     make(map[string]*hubv1.HubConnectedOwnershipStorageRecord),
	}
}

func loadHubCapacityLedger(ctx context.Context, tx *redis.Tx, pod string, capacity int32) (*hubCapacityLedger, error) {
	if capacity <= 0 {
		return nil, errcode.New(errcode.ErrInvalidState, "hub capacity must be positive")
	}
	rKey, sKey := reservationsKey(pod), sessionsKey(pod)
	rLen, err := tx.HLen(ctx, rKey).Result()
	if err != nil {
		return nil, err
	}
	sLen, err := tx.HLen(ctx, sKey).Result()
	if err != nil {
		return nil, err
	}
	// 正常不变量是两表合计 <= capacity。先按单表上限挡住损坏/攻击造成的无界 HGETALL。
	if rLen > int64(capacity) || sLen > int64(capacity) || rLen+sLen > int64(capacity) {
		return nil, errcode.New(errcode.ErrInvalidState,
			"hub capacity ledger exceeds capacity: reservations=%d sessions=%d capacity=%d", rLen, sLen, capacity)
	}
	rawReservations, err := tx.HGetAll(ctx, rKey).Result()
	if err != nil {
		return nil, err
	}
	rawSessions, err := tx.HGetAll(ctx, sKey).Result()
	if err != nil {
		return nil, err
	}
	ledger := newHubCapacityLedger()
	for assignmentID, raw := range rawReservations {
		rec := &hubv1.HubReservationStorageRecord{}
		if err := proto.Unmarshal([]byte(raw), rec); err != nil {
			return nil, fmt.Errorf("decode hub reservation %s/%s: %w", pod, assignmentID, err)
		}
		if rec.GetAssignmentId() != assignmentID {
			return nil, errcode.New(errcode.ErrInvalidState, "hub reservation field identity mismatch")
		}
		ledger.reservations[assignmentID] = rec
	}
	for assignmentID, raw := range rawSessions {
		rec := &hubv1.HubConnectedOwnershipStorageRecord{}
		if err := proto.Unmarshal([]byte(raw), rec); err != nil {
			return nil, fmt.Errorf("decode hub session %s/%s: %w", pod, assignmentID, err)
		}
		if rec.GetAssignmentId() != assignmentID {
			return nil, errcode.New(errcode.ErrInvalidState, "hub session field identity mismatch")
		}
		ledger.sessions[assignmentID] = rec
	}
	return ledger, nil
}

func reservationRecordMatches(rec *hubv1.HubReservationStorageRecord, pod string, id ReservationIdentity) bool {
	return rec != nil && id.PlayerID != 0 && id.AssignmentID != "" && id.InstanceUID != "" &&
		id.ProtocolEpoch != 0 && id.WriterEpoch == authpkg.DSAuthWriterEpochV2 &&
		rec.GetPlayerId() == id.PlayerID && rec.GetAssignmentId() == id.AssignmentID &&
		rec.GetHubPodName() == pod && rec.GetHubInstanceUid() == id.InstanceUID &&
		rec.GetAuthEpoch() == id.ProtocolEpoch && rec.GetAuthWriterEpoch() == id.WriterEpoch
}

func sessionRecordMatches(rec *hubv1.HubConnectedOwnershipStorageRecord, pod string, id ReservationIdentity) bool {
	return rec != nil && id.PlayerID != 0 && id.AssignmentID != "" && id.InstanceUID != "" &&
		id.ProtocolEpoch != 0 && id.WriterEpoch == authpkg.DSAuthWriterEpochV2 &&
		rec.GetPlayerId() == id.PlayerID && rec.GetAssignmentId() == id.AssignmentID &&
		rec.GetHubPodName() == pod && rec.GetHubInstanceUid() == id.InstanceUID &&
		rec.GetAuthEpoch() == id.ProtocolEpoch && rec.GetAuthWriterEpoch() == id.WriterEpoch
}

func ledgerRecordMatchesInstance(pod, uid string, epoch, writer uint32, recPod, recUID string, recEpoch, recWriter uint32) bool {
	return recPod == pod && uid != "" && recUID == uid && epoch != 0 && recEpoch == epoch &&
		writer == authpkg.DSAuthWriterEpochV2 && recWriter == writer
}

// pruneHubCapacityLedger 只在准备成功提交同槽事务时调用；调用者若随后判失败不写回，仍是零副作用。
func pruneHubCapacityLedger(ledger *hubCapacityLedger, pod, uid string, epoch, writer uint32, nowMs int64) {
	for assignmentID, rec := range ledger.reservations {
		if rec.GetExpiresAtMs() <= nowMs || !ledgerRecordMatchesInstance(
			pod, uid, epoch, writer, rec.GetHubPodName(), rec.GetHubInstanceUid(), rec.GetAuthEpoch(), rec.GetAuthWriterEpoch()) {
			delete(ledger.reservations, assignmentID)
		}
	}
	for assignmentID, rec := range ledger.sessions {
		// 新格式 ExpiresAtMs=0，绝不因 heartbeat 漏报或时间推断离场。仅兼容清理
		// expires_at_ms>0 且已到期的旧/未来格式，以及 GameServer UID 漂移数据。
		if (rec.GetExpiresAtMs() > 0 && rec.GetExpiresAtMs() <= nowMs) || !ledgerRecordMatchesInstance(
			pod, uid, epoch, writer, rec.GetHubPodName(), rec.GetHubInstanceUid(), rec.GetAuthEpoch(), rec.GetAuthWriterEpoch()) {
			delete(ledger.sessions, assignmentID)
		}
	}
}

func syncShardCapacityProjection(shard *hubv1.HubShardStorageRecord, ledger *hubCapacityLedger) error {
	reserved, connected := len(ledger.reservations), len(ledger.sessions)
	if shard.GetCapacity() <= 0 || reserved+connected > int(shard.GetCapacity()) {
		return errcode.New(errcode.ErrInvalidState, "hub capacity ledger overflow")
	}
	shard.ReservedCount = int32(reserved)
	shard.ConnectedOwnershipCount = int32(connected)
	shard.PlayerCount = int32(reserved + connected)
	return nil
}

func writeHubCapacityLedger(ctx context.Context, pipe redis.Pipeliner, pod string, ledger *hubCapacityLedger) error {
	rKey, rxKey, sKey, sxKey := reservationsKey(pod), reservationExpiryKey(pod), sessionsKey(pod), sessionExpiryKey(pod)
	pipe.Del(ctx, rKey, rxKey, sKey, sxKey)
	var reservationMaxExpiry, sessionMaxExpiry int64
	reservationIDs := make([]string, 0, len(ledger.reservations))
	for assignmentID := range ledger.reservations {
		reservationIDs = append(reservationIDs, assignmentID)
	}
	sort.Strings(reservationIDs)
	for _, assignmentID := range reservationIDs {
		rec := ledger.reservations[assignmentID]
		payload, err := proto.Marshal(rec)
		if err != nil {
			return err
		}
		pipe.HSet(ctx, rKey, assignmentID, payload)
		pipe.ZAdd(ctx, rxKey, redis.Z{Score: float64(rec.GetExpiresAtMs()), Member: assignmentID})
		if rec.GetExpiresAtMs() > reservationMaxExpiry {
			reservationMaxExpiry = rec.GetExpiresAtMs()
		}
	}
	sessionIDs := make([]string, 0, len(ledger.sessions))
	for assignmentID := range ledger.sessions {
		sessionIDs = append(sessionIDs, assignmentID)
	}
	sort.Strings(sessionIDs)
	for _, assignmentID := range sessionIDs {
		rec := ledger.sessions[assignmentID]
		payload, err := proto.Marshal(rec)
		if err != nil {
			return err
		}
		pipe.HSet(ctx, sKey, assignmentID, payload)
		if rec.GetExpiresAtMs() > 0 {
			pipe.ZAdd(ctx, sxKey, redis.Z{Score: float64(rec.GetExpiresAtMs()), Member: assignmentID})
			if rec.GetExpiresAtMs() > sessionMaxExpiry {
				sessionMaxExpiry = rec.GetExpiresAtMs()
			}
		}
	}
	// 整键 retention 覆盖最晚单项绝对到期 + guard；不能沿用较短 shardTTL 提前删有效 lease。
	if reservationMaxExpiry > 0 {
		deadline := time.UnixMilli(reservationMaxExpiry).Add(capacityLedgerRetentionGuard)
		pipe.PExpireAt(ctx, rKey, deadline)
		pipe.PExpireAt(ctx, rxKey, deadline)
	}
	if sessionMaxExpiry > 0 {
		deadline := time.UnixMilli(sessionMaxExpiry).Add(capacityLedgerRetentionGuard)
		pipe.PExpireAt(ctx, sKey, deadline)
		pipe.PExpireAt(ctx, sxKey, deadline)
	}
	return nil
}

func modelBRoutableReason(authRecord *hubv1.HubShardAuthStorageRecord, shard *hubv1.HubShardStorageRecord,
	pod string, nowMs, maxHeartbeatAgeMs int64, credential *CredentialIdentity) string {
	if authRecord == nil || authRecord.GetPodName() != pod || !hubAuthRecordV2Exact(authRecord) {
		return "auth-missing-or-invalid"
	}
	if authRecord.GetPhase() != hubv1.HubAuthPhase_HUB_AUTH_PHASE_ACTIVE &&
		authRecord.GetPhase() != hubv1.HubAuthPhase_HUB_AUTH_PHASE_ROTATING {
		return "phase-not-active"
	}
	if !routableCredentialComplete(authRecord.GetActive(), authRecord, nowMs) {
		return "no-active"
	}
	if credential != nil && !credMatches(authRecord.GetActive(), *credential) {
		return "credential-not-active"
	}
	if shard == nil || shard.GetHubPodName() != pod || shard.GetState() != "ready" {
		return "shard-not-ready"
	}
	active := authRecord.GetActive()
	if shard.GetLastVerifiedGen() != active.GetGen() || shard.GetLastVerifiedJti() != active.GetJti() ||
		shard.GetGameserverUid() != authRecord.GetInstanceUid() || shard.GetAuthEpoch() != authRecord.GetProtocolEpoch() ||
		shard.GetLastVerifiedWriterEpoch() != active.GetWriterEpoch() {
		return "shard-not-verified-by-active"
	}
	if shard.GetCapacity() <= 0 || shard.GetReportedMaxPlayers() == 0 ||
		int32(shard.GetReportedMaxPlayers()) != shard.GetCapacity() {
		return "max-players-mismatch"
	}
	if shard.GetPlayerCount() != shard.GetReservedCount()+shard.GetConnectedOwnershipCount() ||
		shard.GetPlayerCount() < 0 || shard.GetPlayerCount() > shard.GetCapacity() {
		return "capacity-projection-invalid"
	}
	// 滚动上线时旧连接尚无 admission ledger：实报人数大于 connected ownership 必须
	// 阻断新分配，不能把 reported count 回填成权威或低估后超发。漏报方向只审计不删 owner。
	if shard.GetReportedConnectedCount() < 0 ||
		shard.GetReportedConnectedCount() > shard.GetConnectedOwnershipCount() {
		return "untracked-connected-players"
	}
	if authRecord.GetLastActiveHeartbeatMs() <= 0 || authRecord.GetLastActiveHeartbeatMs() > nowMs {
		return "heartbeat-invalid"
	}
	if maxHeartbeatAgeMs > 0 && nowMs-authRecord.GetLastActiveHeartbeatMs() > maxHeartbeatAgeMs {
		return "heartbeat-stale"
	}
	return ""
}

// modelBCallbackReason 是 Admission/Departure 的 callback 身份门。已有 reservation/session
// 的 ACK 不再要求 shard 仍 ready（可能已进入 draining），但必须仍由当前 active credential
// 调用，且 MaxPlayers/容量投影/实例身份完整一致。
func modelBCallbackReason(authRecord *hubv1.HubShardAuthStorageRecord, shard *hubv1.HubShardStorageRecord,
	pod string, nowMs int64, credential CredentialIdentity) string {
	if authRecord == nil || authRecord.GetPodName() != pod || !hubAuthRecordV2Exact(authRecord) {
		return "auth-missing-or-invalid"
	}
	if authRecord.GetPhase() != hubv1.HubAuthPhase_HUB_AUTH_PHASE_ACTIVE &&
		authRecord.GetPhase() != hubv1.HubAuthPhase_HUB_AUTH_PHASE_ROTATING {
		return "phase-not-active"
	}
	if !routableCredentialComplete(authRecord.GetActive(), authRecord, nowMs) ||
		!credMatches(authRecord.GetActive(), credential) {
		return "credential-not-active"
	}
	if shard == nil || shard.GetHubPodName() != pod {
		return "shard-missing"
	}
	active := authRecord.GetActive()
	if shard.GetLastVerifiedGen() != active.GetGen() || shard.GetLastVerifiedJti() != active.GetJti() ||
		shard.GetGameserverUid() != authRecord.GetInstanceUid() || shard.GetAuthEpoch() != authRecord.GetProtocolEpoch() ||
		shard.GetLastVerifiedWriterEpoch() != active.GetWriterEpoch() {
		return "shard-not-verified-by-active"
	}
	if shard.GetCapacity() <= 0 || shard.GetReportedMaxPlayers() == 0 ||
		int32(shard.GetReportedMaxPlayers()) != shard.GetCapacity() {
		return "max-players-mismatch"
	}
	if shard.GetPlayerCount() != shard.GetReservedCount()+shard.GetConnectedOwnershipCount() ||
		shard.GetPlayerCount() < 0 || shard.GetPlayerCount() > shard.GetCapacity() {
		return "capacity-projection-invalid"
	}
	return ""
}

func reserveResultFromState(authRecord *hubv1.HubShardAuthStorageRecord, shard *hubv1.HubShardStorageRecord) ReserveResult {
	return ReserveResult{
		OK: true, ActiveGen: authRecord.GetActive().GetGen(), ActiveJTI: authRecord.GetActive().GetJti(),
		InstanceUID: authRecord.GetInstanceUid(), ProtocolEpoch: authRecord.GetProtocolEpoch(),
		WriterEpoch: authRecord.GetActive().GetWriterEpoch(), ShardID: shard.GetShardId(),
		HubAddr: shard.GetHubAddr(), Region: shard.GetRegion(), PlayerCount: shard.GetPlayerCount(),
		Capacity: shard.GetCapacity(), ReleaseTrack: shard.GetReleaseTrack(),
	}
}

// ReserveAssignment 以逐 assignment reservation 取代整数 seat++。
func (r *RedisHubAuthRepo) ReserveAssignment(ctx context.Context, pod string, reservation ReservationIdentity,
	nowMs, maxHeartbeatAgeMs int64, shardTTL time.Duration) (ReserveResult, error) {
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	if pod == "" || reservation.PlayerID == 0 || reservation.AssignmentID == "" ||
		reservation.InstanceUID == "" || reservation.ProtocolEpoch == 0 ||
		reservation.WriterEpoch != authpkg.DSAuthWriterEpochV2 || reservation.ExpiresAtMs <= nowMs ||
		reservation.AssignmentExpiresAtMs < reservation.ExpiresAtMs || shardTTL <= 0 {
		return ReserveResult{}, errcode.New(errcode.ErrInvalidArg, "hub reservation identity/expiry invalid")
	}
	aKey, shardKeyName := authKey(pod), shardKey(pod)
	watchKeys := append([]string{aKey, shardKeyName}, capacityLedgerKeys(pod)...)
	for attempt := 0; attempt < hubAuthCASRetries; attempt++ {
		out := ReserveResult{}
		txErr := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			aRaw, err := tx.Get(ctx, aKey).Bytes()
			if err == redis.Nil {
				out.Reason = "auth-missing"
				return nil
			}
			if err != nil {
				return err
			}
			sRaw, err := tx.Get(ctx, shardKeyName).Bytes()
			if err == redis.Nil {
				out.Reason = "shard-missing"
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
			if reason := modelBRoutableReason(authRecord, shard, pod, nowMs, maxHeartbeatAgeMs, nil); reason != "" {
				out.Reason = reason
				return nil
			}
			if reservation.InstanceUID != authRecord.GetInstanceUid() ||
				reservation.ProtocolEpoch != authRecord.GetProtocolEpoch() ||
				reservation.WriterEpoch != authRecord.GetActive().GetWriterEpoch() {
				out.Reason = "reservation-instance-mismatch"
				return nil
			}
			ledger, err := loadHubCapacityLedger(ctx, tx, pod, shard.GetCapacity())
			if err != nil {
				return err
			}
			pruneHubCapacityLedger(ledger, pod, authRecord.GetInstanceUid(), authRecord.GetProtocolEpoch(),
				authRecord.GetActive().GetWriterEpoch(), nowMs)
			if session, ok := ledger.sessions[reservation.AssignmentID]; ok {
				if !sessionRecordMatches(session, pod, reservation) {
					out.Reason = "assignment-session-conflict"
					return nil
				}
				// 已真接纳：同 assignment 重签/重连不创建 reservation，不重复计数。
			} else if current, ok := ledger.reservations[reservation.AssignmentID]; ok {
				if !reservationRecordMatches(current, pod, reservation) {
					out.Reason = "assignment-reservation-conflict"
					return nil
				}
				if reservation.ExpiresAtMs > current.GetExpiresAtMs() {
					current.ExpiresAtMs = reservation.ExpiresAtMs
				}
				if reservation.AssignmentExpiresAtMs > current.GetAssignmentExpiresAtMs() {
					current.AssignmentExpiresAtMs = reservation.AssignmentExpiresAtMs
				}
			} else {
				if len(ledger.reservations)+len(ledger.sessions) >= int(shard.GetCapacity()) {
					out.Reason = "shard-full"
					return nil
				}
				ledger.reservations[reservation.AssignmentID] = &hubv1.HubReservationStorageRecord{
					PlayerId: reservation.PlayerID, AssignmentId: reservation.AssignmentID, HubPodName: pod,
					HubInstanceUid: reservation.InstanceUID, AuthEpoch: reservation.ProtocolEpoch,
					AuthWriterEpoch: reservation.WriterEpoch, CreatedAtMs: nowMs, ExpiresAtMs: reservation.ExpiresAtMs,
					AssignmentExpiresAtMs: reservation.AssignmentExpiresAtMs,
				}
			}
			if err := syncShardCapacityProjection(shard, ledger); err != nil {
				return err
			}
			shardPayload, err := marshalShard(shard)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				if err := writeHubCapacityLedger(ctx, pipe, pod, ledger); err != nil {
					return err
				}
				pipe.Set(ctx, shardKeyName, shardPayload, shardTTL)
				return nil
			})
			if err == nil {
				out = reserveResultFromState(authRecord, shard)
			}
			return err
		}, watchKeys...)
		if txErr == redis.TxFailedErr {
			continue
		}
		return out, txErr
	}
	return ReserveResult{}, errcode.New(errcode.ErrInternal, "hub reserve assignment %s: cas retry exhausted", pod)
}

// AcknowledgeAdmission 原子把 reservation 消费为 connected ownership；同 assignment 的新
// admission_id 表示重连/旧连接替换，更新 owner 但不增加容量。
func (r *RedisHubAuthRepo) AcknowledgeAdmission(ctx context.Context, pod string, credential CredentialIdentity,
	reservation ReservationIdentity, admissionID string, admissionSeq uint64, nowMs int64,
	shardTTL time.Duration) (AdmissionResult, error) {
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	parsedAdmission, err := uuid.Parse(admissionID)
	if err != nil || parsedAdmission.Version() != 4 || parsedAdmission.String() != admissionID ||
		shardTTL <= 0 || reservation.PlayerID == 0 || reservation.AssignmentID == "" ||
		reservation.InstanceUID == "" || reservation.ProtocolEpoch == 0 ||
		reservation.WriterEpoch != authpkg.DSAuthWriterEpochV2 || admissionSeq == 0 {
		return AdmissionResult{}, errcode.New(errcode.ErrInvalidArg, "hub admission identity invalid")
	}
	aKey, shardKeyName := authKey(pod), shardKey(pod)
	watchKeys := append([]string{aKey, shardKeyName}, capacityLedgerKeys(pod)...)
	for attempt := 0; attempt < hubAuthCASRetries; attempt++ {
		out := AdmissionResult{}
		var rejected bool
		var conflict bool
		txErr := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			aRaw, err := tx.Get(ctx, aKey).Bytes()
			if err == redis.Nil {
				rejected = true
				return nil
			}
			if err != nil {
				return err
			}
			sRaw, err := tx.Get(ctx, shardKeyName).Bytes()
			if err == redis.Nil {
				rejected = true
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
			if modelBCallbackReason(authRecord, shard, pod, nowMs, credential) != "" ||
				reservation.InstanceUID != authRecord.GetInstanceUid() ||
				reservation.ProtocolEpoch != authRecord.GetProtocolEpoch() ||
				reservation.WriterEpoch != authRecord.GetActive().GetWriterEpoch() {
				rejected = true
				return nil
			}
			ledger, err := loadHubCapacityLedger(ctx, tx, pod, shard.GetCapacity())
			if err != nil {
				return err
			}
			pruneHubCapacityLedger(ledger, pod, authRecord.GetInstanceUid(), authRecord.GetProtocolEpoch(),
				authRecord.GetActive().GetWriterEpoch(), nowMs)
			if current, ok := ledger.sessions[reservation.AssignmentID]; ok {
				if !sessionRecordMatches(current, pod, reservation) {
					rejected = true
					return nil
				}
				switch {
				case admissionSeq < current.GetAdmissionSeq():
					conflict = true
					out.Conflict = true
					return nil
				case admissionSeq == current.GetAdmissionSeq() && current.GetAdmissionId() != admissionID:
					conflict = true
					out.Conflict = true
					return nil
				case admissionSeq == current.GetAdmissionSeq():
					// 响应丢失：相同 seq+UUID 幂等重认。
					out.AlreadyAdmitted = true
				default:
					// 只有更大 seq 才允许重连替换 owner；迟到旧 ACK 永远不能反向夺回。
					current.AdmissionId = admissionID
					current.AdmissionSeq = admissionSeq
				}
				current.LastSeenMs = nowMs
				current.ExpiresAtMs = 0
			} else {
				pending, ok := ledger.reservations[reservation.AssignmentID]
				if !ok || !reservationRecordMatches(pending, pod, reservation) || pending.GetExpiresAtMs() <= nowMs {
					rejected = true
					return nil
				}
				delete(ledger.reservations, reservation.AssignmentID)
				ledger.sessions[reservation.AssignmentID] = &hubv1.HubConnectedOwnershipStorageRecord{
					PlayerId: reservation.PlayerID, AssignmentId: reservation.AssignmentID, AdmissionId: admissionID,
					HubPodName: pod, HubInstanceUid: reservation.InstanceUID, AuthEpoch: reservation.ProtocolEpoch,
					AuthWriterEpoch: reservation.WriterEpoch, AdmittedAtMs: nowMs, LastSeenMs: nowMs,
					ExpiresAtMs: 0, AdmissionSeq: admissionSeq,
				}
			}
			if err := syncShardCapacityProjection(shard, ledger); err != nil {
				return err
			}
			shardPayload, err := marshalShard(shard)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				if err := writeHubCapacityLedger(ctx, pipe, pod, ledger); err != nil {
					return err
				}
				pipe.Set(ctx, shardKeyName, shardPayload, shardTTL)
				return nil
			})
			if err == nil {
				out.Admitted = true
				out.ReservedCount = shard.GetReservedCount()
				out.ConnectedCount = shard.GetConnectedOwnershipCount()
				out.CapacityOccupancy = shard.GetPlayerCount()
			}
			return err
		}, watchKeys...)
		if txErr == redis.TxFailedErr {
			continue
		}
		if txErr != nil {
			return AdmissionResult{}, txErr
		}
		if conflict {
			return out, nil
		}
		if rejected || !out.Admitted {
			return AdmissionResult{}, errAuthStale
		}
		return out, nil
	}
	return AdmissionResult{}, errcode.New(errcode.ErrInternal, "hub admission %s: cas retry exhausted", pod)
}

// AcknowledgeDeparture exact 删除当前 admission owner。网络响应丢失后同 identity 重试幂等；
// 同 assignment 已被新 admission 接管时返回 Conflict，旧 Logout 不得删新连接。
func (r *RedisHubAuthRepo) AcknowledgeDeparture(ctx context.Context, pod string, credential CredentialIdentity,
	reservation ReservationIdentity, admissionID string, admissionSeq uint64, nowMs int64,
	shardTTL time.Duration) (DepartureResult, error) {
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	parsedAdmission, err := uuid.Parse(admissionID)
	if err != nil || parsedAdmission.Version() != 4 || parsedAdmission.String() != admissionID ||
		shardTTL <= 0 || reservation.PlayerID == 0 || reservation.AssignmentID == "" ||
		reservation.InstanceUID == "" || reservation.ProtocolEpoch == 0 ||
		reservation.WriterEpoch != authpkg.DSAuthWriterEpochV2 || admissionSeq == 0 {
		return DepartureResult{}, errcode.New(errcode.ErrInvalidArg, "hub departure identity invalid")
	}
	aKey, shardKeyName := authKey(pod), shardKey(pod)
	watchKeys := append([]string{aKey, shardKeyName}, capacityLedgerKeys(pod)...)
	for attempt := 0; attempt < hubAuthCASRetries; attempt++ {
		out := DepartureResult{}
		var unauthorized bool
		txErr := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			aRaw, err := tx.Get(ctx, aKey).Bytes()
			if err == redis.Nil {
				unauthorized = true
				return nil
			}
			if err != nil {
				return err
			}
			sRaw, err := tx.Get(ctx, shardKeyName).Bytes()
			if err == redis.Nil {
				unauthorized = true
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
			if modelBCallbackReason(authRecord, shard, pod, nowMs, credential) != "" ||
				reservation.InstanceUID != authRecord.GetInstanceUid() ||
				reservation.ProtocolEpoch != authRecord.GetProtocolEpoch() ||
				reservation.WriterEpoch != authRecord.GetActive().GetWriterEpoch() {
				unauthorized = true
				return nil
			}
			ledger, err := loadHubCapacityLedger(ctx, tx, pod, shard.GetCapacity())
			if err != nil {
				return err
			}
			pruneHubCapacityLedger(ledger, pod, authRecord.GetInstanceUid(), authRecord.GetProtocolEpoch(),
				authRecord.GetActive().GetWriterEpoch(), nowMs)
			current, exists := ledger.sessions[reservation.AssignmentID]
			switch {
			case !exists:
				// 已成功删过且没有重新 reservation，exact 重试幂等成功。若 reservation 仍在，
				// 说明从未完成 admission，不把它伪装成 departure 成功。
				if pending, reserved := ledger.reservations[reservation.AssignmentID]; reserved &&
					reservationRecordMatches(pending, pod, reservation) {
					out.Conflict = true
				} else {
					out.Departed = true
					out.AlreadyDeparted = true
				}
			case !sessionRecordMatches(current, pod, reservation):
				out.Conflict = true
			case current.GetAdmissionSeq() != admissionSeq || current.GetAdmissionId() != admissionID:
				out.Conflict = true
			default:
				delete(ledger.sessions, reservation.AssignmentID)
				out.Departed = true
			}
			// 旧 Logout / 从未 admission 的 reservation 都是 exact identity conflict。
			// 冲突必须保持新 owner、TTL、projection 逐字不变，不能顺手 prune/续期其他记录。
			if out.Conflict {
				return nil
			}
			if err := syncShardCapacityProjection(shard, ledger); err != nil {
				return err
			}
			shardPayload, err := marshalShard(shard)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				if err := writeHubCapacityLedger(ctx, pipe, pod, ledger); err != nil {
					return err
				}
				pipe.Set(ctx, shardKeyName, shardPayload, shardTTL)
				return nil
			})
			if err == nil {
				out.ReservedCount = shard.GetReservedCount()
				out.ConnectedCount = shard.GetConnectedOwnershipCount()
				out.CapacityOccupancy = shard.GetPlayerCount()
			}
			return err
		}, watchKeys...)
		if txErr == redis.TxFailedErr {
			continue
		}
		if txErr != nil {
			return DepartureResult{}, txErr
		}
		if unauthorized {
			return DepartureResult{}, errAuthStale
		}
		return out, nil
	}
	return DepartureResult{}, errcode.New(errcode.ErrInternal, "hub departure %s: cas retry exhausted", pod)
}

// ReleaseAssignmentSeat 在调用方赢得跨 slot assignment CAS 后，只精确删除尚未 Admission
// 的 reservation。live connected ownership 不是一条可随意回收的容量记录：它对应旧 Hub
// 上真实存在的 PlayerController/Pawn，必须由 exact Departure 或确认的 UID teardown 删除。
func (r *RedisHubAuthRepo) ReleaseAssignmentSeat(ctx context.Context, pod string,
	expected AssignmentInstanceIdentity, shardTTL time.Duration) (bool, error) {
	result, err := r.ReleaseAssignmentSeatExact(ctx, pod, expected, shardTTL)
	return result.Released, err
}

// InspectAssignmentSeat returns the exact capacity/physical-owner state without
// mutation. It deliberately does not use TTL or heartbeat absence as departure
// proof; connected ownership is persistent until Departure/UID teardown.
func (r *RedisHubAuthRepo) InspectAssignmentSeat(ctx context.Context, pod string,
	expected AssignmentInstanceIdentity) (AssignmentSeatSnapshot, error) {
	if pod == "" || expected.PlayerID == 0 || expected.AssignmentID == "" || expected.InstanceUID == "" ||
		expected.ProtocolEpoch == 0 || expected.WriterEpoch != authpkg.DSAuthWriterEpochV2 {
		return AssignmentSeatSnapshot{}, errcode.New(errcode.ErrInvalidArg,
			"complete assignment seat inspection identity required")
	}
	identity := ReservationIdentity{
		PlayerID: expected.PlayerID, AssignmentID: expected.AssignmentID, InstanceUID: expected.InstanceUID,
		ProtocolEpoch: expected.ProtocolEpoch, WriterEpoch: expected.WriterEpoch,
	}
	var reservationCmd, sessionCmd *redis.StringCmd
	_, err := r.rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		reservationCmd = pipe.HGet(ctx, reservationsKey(pod), expected.AssignmentID)
		sessionCmd = pipe.HGet(ctx, sessionsKey(pod), expected.AssignmentID)
		return nil
	})
	if err != nil && err != redis.Nil {
		return AssignmentSeatSnapshot{}, err
	}
	var reservation *hubv1.HubReservationStorageRecord
	if raw, getErr := reservationCmd.Bytes(); getErr == nil {
		reservation = &hubv1.HubReservationStorageRecord{}
		if decodeErr := proto.Unmarshal(raw, reservation); decodeErr != nil {
			return AssignmentSeatSnapshot{}, decodeErr
		}
	} else if getErr != redis.Nil {
		return AssignmentSeatSnapshot{}, getErr
	}
	var session *hubv1.HubConnectedOwnershipStorageRecord
	if raw, getErr := sessionCmd.Bytes(); getErr == nil {
		session = &hubv1.HubConnectedOwnershipStorageRecord{}
		if decodeErr := proto.Unmarshal(raw, session); decodeErr != nil {
			return AssignmentSeatSnapshot{}, decodeErr
		}
	} else if getErr != redis.Nil {
		return AssignmentSeatSnapshot{}, getErr
	}
	if reservation != nil && session != nil {
		return AssignmentSeatSnapshot{}, errcode.New(errcode.ErrInvalidState,
			"assignment exists in reservation and session ledgers")
	}
	switch {
	case reservation == nil && session == nil:
		return AssignmentSeatSnapshot{AlreadyAbsent: true}, nil
	case reservation != nil:
		if !reservationRecordMatches(reservation, pod, identity) {
			return AssignmentSeatSnapshot{Conflict: true}, nil
		}
		return AssignmentSeatSnapshot{Reserved: true,
			ReservationExpiresAtMs: reservation.GetExpiresAtMs()}, nil
	default:
		if !sessionRecordMatches(session, pod, identity) || session.GetAdmissionId() == "" ||
			session.GetAdmissionSeq() == 0 {
			return AssignmentSeatSnapshot{Conflict: true}, nil
		}
		return AssignmentSeatSnapshot{Connected: true, AdmissionID: session.GetAdmissionId(),
			AdmissionSeq: session.GetAdmissionSeq()}, nil
	}
}

// ReleaseAssignmentSeatExact exposes the states required by a durable cleanup
// saga. AlreadyAbsent is an idempotent replay success; Conflict means another
// identity exists. DepartureRequired is the crucial physical fence: a matching
// connected owner is never deleted here and must be evicted by its source DS.
func (r *RedisHubAuthRepo) ReleaseAssignmentSeatExact(ctx context.Context, pod string,
	expected AssignmentInstanceIdentity, shardTTL time.Duration) (ReleaseAssignmentSeatResult, error) {
	if pod == "" || expected.PlayerID == 0 || expected.AssignmentID == "" || expected.InstanceUID == "" ||
		expected.ProtocolEpoch == 0 || expected.WriterEpoch != authpkg.DSAuthWriterEpochV2 || shardTTL <= 0 {
		return ReleaseAssignmentSeatResult{}, errcode.New(errcode.ErrInvalidArg,
			"complete assignment seat release identity required")
	}
	identity := ReservationIdentity{
		PlayerID: expected.PlayerID, AssignmentID: expected.AssignmentID, InstanceUID: expected.InstanceUID,
		ProtocolEpoch: expected.ProtocolEpoch, WriterEpoch: expected.WriterEpoch,
	}
	aKey, shardKeyName := authKey(pod), shardKey(pod)
	watchKeys := append([]string{aKey, shardKeyName}, capacityLedgerKeys(pod)...)
	for attempt := 0; attempt < hubAuthCASRetries; attempt++ {
		out := ReleaseAssignmentSeatResult{}
		txErr := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			var reservation *hubv1.HubReservationStorageRecord
			if raw, err := tx.HGet(ctx, reservationsKey(pod), expected.AssignmentID).Bytes(); err == nil {
				reservation = &hubv1.HubReservationStorageRecord{}
				if err := proto.Unmarshal(raw, reservation); err != nil {
					return err
				}
			} else if err != redis.Nil {
				return err
			}
			var session *hubv1.HubConnectedOwnershipStorageRecord
			if raw, err := tx.HGet(ctx, sessionsKey(pod), expected.AssignmentID).Bytes(); err == nil {
				session = &hubv1.HubConnectedOwnershipStorageRecord{}
				if err := proto.Unmarshal(raw, session); err != nil {
					return err
				}
			} else if err != redis.Nil {
				return err
			}
			reservationMatches := reservationRecordMatches(reservation, pod, identity)
			sessionMatches := sessionRecordMatches(session, pod, identity)
			if reservation != nil && session != nil {
				return errcode.New(errcode.ErrInvalidState, "assignment exists in reservation and session ledgers")
			}
			if reservation == nil && session == nil {
				out.AlreadyAbsent = true
				return nil
			}
			if !reservationMatches && !sessionMatches {
				out.Conflict = true
				return nil
			}
			if sessionMatches {
				out.DepartureRequired = true
				return nil
			}

			// 只有当前 auth+shard 仍属于同一实例时才改 projection；旧 UID ledger 可精确删，
			// 但绝不能触碰同名新实例的 player_count。
			var authRecord *hubv1.HubShardAuthStorageRecord
			if raw, err := tx.Get(ctx, aKey).Bytes(); err == nil {
				authRecord = &hubv1.HubShardAuthStorageRecord{}
				if err := proto.Unmarshal(raw, authRecord); err != nil {
					return err
				}
			} else if err != redis.Nil {
				return err
			}
			var shard *hubv1.HubShardStorageRecord
			if raw, err := tx.Get(ctx, shardKeyName).Bytes(); err == nil {
				var decodeErr error
				shard, decodeErr = unmarshalShard(pod, raw)
				if decodeErr != nil {
					return decodeErr
				}
			} else if err != redis.Nil {
				return err
			}
			active := authRecord.GetActive()
			currentProjection := authRecord != nil && shard != nil && active != nil &&
				hubAuthRecordV2Exact(authRecord) && authRecord.GetPodName() == pod &&
				authRecord.GetInstanceUid() == expected.InstanceUID &&
				authRecord.GetProtocolEpoch() == expected.ProtocolEpoch &&
				active.GetWriterEpoch() == expected.WriterEpoch &&
				shard.GetGameserverUid() == expected.InstanceUID &&
				shard.GetAuthEpoch() == expected.ProtocolEpoch &&
				shard.GetLastVerifiedWriterEpoch() == expected.WriterEpoch
			if !currentProjection {
				_, err := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
					pipe.HDel(ctx, reservationsKey(pod), expected.AssignmentID)
					pipe.ZRem(ctx, reservationExpiryKey(pod), expected.AssignmentID)
					return nil
				})
				if err == nil {
					out.Released = true
				}
				return err
			}

			ledger, err := loadHubCapacityLedger(ctx, tx, pod, shard.GetCapacity())
			if err != nil {
				return err
			}
			pruneHubCapacityLedger(ledger, pod, expected.InstanceUID, expected.ProtocolEpoch,
				expected.WriterEpoch, time.Now().UnixMilli())
			// target 可能刚被 expiry prune 从内存视图删除；底层精确记录仍是本次读到并
			// 匹配的那个，继续提交整个 ledger 才能真正清掉它并修正 projection。
			delete(ledger.reservations, expected.AssignmentID)
			if err := syncShardCapacityProjection(shard, ledger); err != nil {
				return err
			}
			payload, err := marshalShard(shard)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				if err := writeHubCapacityLedger(ctx, pipe, pod, ledger); err != nil {
					return err
				}
				pipe.Set(ctx, shardKeyName, payload, shardTTL)
				return nil
			})
			if err == nil {
				out.Released = true
			}
			return err
		}, watchKeys...)
		if txErr == redis.TxFailedErr {
			continue
		}
		return out, txErr
	}
	return ReleaseAssignmentSeatResult{}, errcode.New(errcode.ErrInternal,
		"hub assignment release %s: cas retry exhausted", pod)
}

// RemoveCapacityLedger 仅在分片已被确认回收后清派生账；活分片绝不调用。
func (r *RedisHubAuthRepo) RemoveCapacityLedger(ctx context.Context, pod string) error {
	if pod == "" {
		return errcode.New(errcode.ErrInvalidArg, "hub pod required")
	}
	return r.rdb.Del(ctx, capacityLedgerKeys(pod)...).Err()
}
