// hub_capacity_ledger.go 实现 Hub 的逐 reservation / connected ownership 容量权威。
//
// 六把 ledger key 与 auth/shard 共用 {pod} hashtag，可在一次 WATCH/MULTI/EXEC 中完成：
//
//	pandora:hub:reservations:{pod}         HASH assignment_id -> HubReservationStorageRecord
//	pandora:hub:reservation-expiry:{pod}  ZSET assignment_id -> expires_at_ms
//	pandora:hub:sessions:{pod}             HASH assignment_id -> HubConnectedOwnershipStorageRecord
//	pandora:hub:session-expiry:{pod}       ZSET assignment_id -> expires_at_ms
//	pandora:hub:successors:{pod}           HASH exact_capability -> HubReservationStorageRecord
//	pandora:hub:successor-expiry:{pod}     ZSET exact_capability -> expires_at_ms
//
// successor 是同 assignment 重签时的有界接力 lease：旧 session 仍在时它不重复计容，
// exact Departure 删掉旧 owner 后则立即作为 reserved seat 计容，直到新 Admission 原子
// 消费或绝对到期。player_count 只由这个逐 assignment 并集派生；Heartbeat 的 reported count/list 只写
// 审计字段，绝不能覆盖 reservation 或推断连接离场。connected ownership 新格式没有时间 TTL，
// 只由 exact Departure 或已确认的 UID teardown 删除；Release/Transfer 只能下发物理 eviction
// 并等待该 proof，不能直接删 ledger 冒充 Pawn 已退出。所有 protobuf read-modify-write 保留
// unknown fields（不变量 §17）。
package data

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
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

func successorsKey(pod string) string {
	return fmt.Sprintf("pandora:hub:successors:{%s}", pod)
}

func successorExpiryKey(pod string) string {
	return fmt.Sprintf("pandora:hub:successor-expiry:{%s}", pod)
}

func capacityLedgerKeys(pod string) []string {
	return []string{reservationsKey(pod), reservationExpiryKey(pod), sessionsKey(pod), sessionExpiryKey(pod),
		successorsKey(pod), successorExpiryKey(pod)}
}

func instanceTeardownProofKey(pod string) string {
	return fmt.Sprintf("pandora:hub:instance-teardown:{%s}", pod)
}

// RecordInstanceTeardownProof stores proof by immutable GameServer UID.  A
// later same-name replacement cannot invalidate or accidentally consume the
// old UID proof, and a proof can never authorize cleanup of a different UID.
func (r *RedisHubAuthRepo) RecordInstanceTeardownProof(ctx context.Context, pod, instanceUID string, proofTTL time.Duration) error {
	if pod == "" || instanceUID == "" {
		return errcode.New(errcode.ErrInvalidArg, "hub instance teardown proof requires pod and uid")
	}
	key := instanceTeardownProofKey(pod)
	_, err := r.rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.HSet(ctx, key, instanceUID, time.Now().UnixMilli())
		if proofTTL > 0 {
			pipe.Expire(ctx, key, proofTTL)
		}
		return nil
	})
	return err
}

func (r *RedisHubAuthRepo) HasInstanceTeardownProof(ctx context.Context, pod, instanceUID string) (bool, error) {
	if pod == "" || instanceUID == "" {
		return false, errcode.New(errcode.ErrInvalidArg, "hub instance teardown proof requires pod and uid")
	}
	return r.rdb.HExists(ctx, instanceTeardownProofKey(pod), instanceUID).Result()
}

type hubCapacityLedger struct {
	reservations map[string]*hubv1.HubReservationStorageRecord
	sessions     map[string]*hubv1.HubConnectedOwnershipStorageRecord
	// successors is keyed by an encoded exact capability rather than merely the
	// assignment id.  The value deliberately reuses the canonical reservation
	// storage proto; the capability field carries placement identity without a
	// wire/schema expansion.
	successors map[string]*hubv1.HubReservationStorageRecord
}

func newHubCapacityLedger() *hubCapacityLedger {
	return &hubCapacityLedger{
		reservations: make(map[string]*hubv1.HubReservationStorageRecord),
		sessions:     make(map[string]*hubv1.HubConnectedOwnershipStorageRecord),
		successors:   make(map[string]*hubv1.HubReservationStorageRecord),
	}
}

func loadHubCapacityLedger(ctx context.Context, tx *redis.Tx, pod string, capacity int32) (*hubCapacityLedger, error) {
	if capacity <= 0 {
		return nil, errcode.New(errcode.ErrInvalidState, "hub capacity must be positive")
	}
	rKey, sKey, xKey := reservationsKey(pod), sessionsKey(pod), successorsKey(pod)
	rLen, err := tx.HLen(ctx, rKey).Result()
	if err != nil {
		return nil, err
	}
	sLen, err := tx.HLen(ctx, sKey).Result()
	if err != nil {
		return nil, err
	}
	xLen, err := tx.HLen(ctx, xKey).Result()
	if err != nil {
		return nil, err
	}
	// successor 最多与一个 session 或 detached reserved seat 一一对应。先按
	// 单表上限挡住损坏/攻击造成的无界 HGETALL；并集容量在解码后再校验。
	if rLen > int64(capacity) || sLen > int64(capacity) || xLen > int64(capacity) ||
		rLen+sLen > int64(capacity) {
		return nil, errcode.New(errcode.ErrInvalidState,
			"hub capacity ledger exceeds capacity: reservations=%d sessions=%d successors=%d capacity=%d",
			rLen, sLen, xLen, capacity)
	}
	rawReservations, err := tx.HGetAll(ctx, rKey).Result()
	if err != nil {
		return nil, err
	}
	rawSessions, err := tx.HGetAll(ctx, sKey).Result()
	if err != nil {
		return nil, err
	}
	rawSuccessors, err := tx.HGetAll(ctx, xKey).Result()
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
	seenSuccessorAssignments := make(map[string]string, len(rawSuccessors))
	for capability, raw := range rawSuccessors {
		rec := &hubv1.HubReservationStorageRecord{}
		if err := proto.Unmarshal([]byte(raw), rec); err != nil {
			return nil, fmt.Errorf("decode hub successor %s/%s: %w", pod, capability, err)
		}
		identity, err := decodeSuccessorCapability(capability, pod)
		if err != nil || !reservationRecordMatches(rec, pod, identity) {
			return nil, errcode.New(errcode.ErrInvalidState, "hub successor capability identity mismatch")
		}
		if previous, duplicate := seenSuccessorAssignments[rec.GetAssignmentId()]; duplicate && previous != capability {
			return nil, errcode.New(errcode.ErrInvalidState, "multiple hub successors for one assignment")
		}
		seenSuccessorAssignments[rec.GetAssignmentId()] = capability
		ledger.successors[capability] = rec
	}
	if _, _, err := capacityLedgerCounts(ledger, capacity); err != nil {
		return nil, err
	}
	return ledger, nil
}

const successorCapabilityDomain = "pandora-hub-successor-v1"

// successorCapability is persisted as the Redis HASH field.  Encoding the full
// tuple (instead of only hashing it) lets every loader validate that the field,
// protobuf value and admission request all describe the same immutable target
// and placement operation.  All components are service-owned identifiers; a
// newline is rejected before encoding so the canonical form stays unambiguous.
func successorCapability(pod string, id ReservationIdentity) (string, error) {
	if pod == "" || id.PlayerID == 0 || id.AssignmentID == "" || id.InstanceUID == "" ||
		id.ProtocolEpoch == 0 || id.WriterEpoch != authpkg.DSAuthWriterEpochV2 ||
		!reservationPlacementValid(id) || strings.ContainsAny(pod+id.AssignmentID+id.InstanceUID+id.PlacementOperationID, "\r\n") {
		return "", errcode.New(errcode.ErrInvalidArg, "hub successor capability identity invalid")
	}
	canonical := strings.Join([]string{
		successorCapabilityDomain,
		strconv.FormatUint(id.PlayerID, 10),
		id.AssignmentID,
		pod,
		id.InstanceUID,
		strconv.FormatUint(uint64(id.ProtocolEpoch), 10),
		strconv.FormatUint(uint64(id.WriterEpoch), 10),
		strconv.FormatUint(id.PlacementVersion, 10),
		id.PlacementOperationID,
		strconv.FormatUint(id.SourceMatchID, 10),
	}, "\n")
	return base64.RawURLEncoding.EncodeToString([]byte(canonical)), nil
}

func decodeSuccessorCapability(capability, expectedPod string) (ReservationIdentity, error) {
	raw, err := base64.RawURLEncoding.DecodeString(capability)
	if err != nil {
		return ReservationIdentity{}, err
	}
	parts := strings.Split(string(raw), "\n")
	if len(parts) != 10 || parts[0] != successorCapabilityDomain || expectedPod == "" || parts[3] != expectedPod {
		return ReservationIdentity{}, errcode.New(errcode.ErrInvalidState, "invalid hub successor capability encoding")
	}
	playerID, playerErr := strconv.ParseUint(parts[1], 10, 64)
	protocolEpoch, protocolErr := strconv.ParseUint(parts[5], 10, 32)
	writerEpoch, writerErr := strconv.ParseUint(parts[6], 10, 32)
	placementVersion, placementErr := strconv.ParseUint(parts[7], 10, 64)
	sourceMatchID, sourceErr := strconv.ParseUint(parts[9], 10, 64)
	if playerErr != nil || protocolErr != nil || writerErr != nil || placementErr != nil || sourceErr != nil {
		return ReservationIdentity{}, errcode.New(errcode.ErrInvalidState, "invalid hub successor capability numbers")
	}
	id := ReservationIdentity{
		PlayerID: playerID, AssignmentID: parts[2], InstanceUID: parts[4],
		ProtocolEpoch: uint32(protocolEpoch), WriterEpoch: uint32(writerEpoch),
		PlacementVersion: placementVersion, PlacementOperationID: parts[8], SourceMatchID: sourceMatchID,
	}
	if encoded, encodeErr := successorCapability(parts[3], id); encodeErr != nil || encoded != capability {
		return ReservationIdentity{}, errcode.New(errcode.ErrInvalidState, "non-canonical hub successor capability")
	}
	return id, nil
}

func reservationPlacementValid(id ReservationIdentity) bool {
	if id.PlacementVersion == 0 {
		return id.PlacementOperationID == "" && id.SourceMatchID == 0
	}
	parsed, err := uuid.Parse(id.PlacementOperationID)
	return err == nil && parsed != uuid.Nil && parsed.Version() == 4 && parsed.String() == id.PlacementOperationID
}

func successorForAssignment(ledger *hubCapacityLedger, assignmentID string) (string, *hubv1.HubReservationStorageRecord, bool) {
	for capability, rec := range ledger.successors {
		if rec.GetAssignmentId() == assignmentID {
			return capability, rec, true
		}
	}
	return "", nil, false
}

func successorMatches(ledger *hubCapacityLedger, pod string, id ReservationIdentity) (string, *hubv1.HubReservationStorageRecord, bool, bool) {
	capability, rec, exists := successorForAssignment(ledger, id.AssignmentID)
	if !exists {
		return "", nil, false, false
	}
	expected, err := successorCapability(pod, id)
	if err != nil || capability != expected || !reservationRecordMatches(rec, pod, id) {
		return capability, rec, true, false
	}
	return capability, rec, true, true
}

// loadBoundedSuccessors is used by cleanup paths that may only know the base
// assignment owner (old durable records did not persist source placement).  It
// must be called under WATCH of successorsKey and the shard key; HLEN is checked
// against authoritative shard capacity before HGETALL so corrupted input cannot
// turn cleanup into an unbounded scan.
func loadBoundedSuccessors(ctx context.Context, tx *redis.Tx, pod string, capacity int32) (map[string]*hubv1.HubReservationStorageRecord, error) {
	count, err := tx.HLen(ctx, successorsKey(pod)).Result()
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return map[string]*hubv1.HubReservationStorageRecord{}, nil
	}
	if capacity <= 0 || count > int64(capacity) {
		return nil, errcode.New(errcode.ErrInvalidState,
			"hub successor scan exceeds shard capacity: successors=%d capacity=%d", count, capacity)
	}
	rawRecords, err := tx.HGetAll(ctx, successorsKey(pod)).Result()
	if err != nil {
		return nil, err
	}
	out := make(map[string]*hubv1.HubReservationStorageRecord, len(rawRecords))
	for capability, raw := range rawRecords {
		rec := &hubv1.HubReservationStorageRecord{}
		if err := proto.Unmarshal([]byte(raw), rec); err != nil {
			return nil, err
		}
		decoded, decodeErr := decodeSuccessorCapability(capability, pod)
		if decodeErr != nil || !reservationRecordMatches(rec, pod, decoded) {
			return nil, errcode.New(errcode.ErrInvalidState, "hub successor capability identity mismatch")
		}
		out[capability] = rec
	}
	return out, nil
}

// successorForCleanup accepts missing placement lineage for compatibility with
// already-persisted transfer-cleanup records, but never accepts a different
// base owner.  If lineage is present it must match exactly.  More than one
// capability for an assignment is an ABA/corruption conflict, not a choice.
func successorForCleanup(records map[string]*hubv1.HubReservationStorageRecord, pod string,
	expected ReservationIdentity, nowMs int64) (string, *hubv1.HubReservationStorageRecord, bool, bool) {
	var foundField string
	var found *hubv1.HubReservationStorageRecord
	for capability, rec := range records {
		if rec.GetAssignmentId() != expected.AssignmentID {
			continue
		}
		decoded, err := decodeSuccessorCapability(capability, pod)
		if err != nil || !reservationRecordMatches(rec, pod, expected) {
			return "", nil, false, true
		}
		if expected.PlacementVersion != 0 && rec.GetExpiresAtMs() > nowMs {
			switch {
			case decoded.PlacementVersion > expected.PlacementVersion:
				// A live capability from a newer placement may belong to a writer
				// that won after this cleanup snapshot; never cancel it backwards.
				return "", nil, false, true
			case decoded.PlacementVersion == expected.PlacementVersion &&
				(decoded.PlacementOperationID != expected.PlacementOperationID ||
					decoded.SourceMatchID != expected.SourceMatchID):
				// Same-version forks are ABA conflicts.  A strictly older capability
				// is different: assignment bind may have committed vN+1 before the
				// post-bind successor rotation, so cleanup of vN+1 may cancel vN.
				return "", nil, false, true
			}
		}
		if found != nil {
			return "", nil, false, true
		}
		foundField, found = capability, rec
	}
	return foundField, found, found != nil, false
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
	for capability, rec := range ledger.successors {
		if rec.GetExpiresAtMs() <= nowMs || !ledgerRecordMatchesInstance(
			pod, uid, epoch, writer, rec.GetHubPodName(), rec.GetHubInstanceUid(), rec.GetAuthEpoch(), rec.GetAuthWriterEpoch()) {
			delete(ledger.successors, capability)
		}
	}
}

func capacityLedgerCounts(ledger *hubCapacityLedger, capacity int32) (int, int, error) {
	if capacity <= 0 {
		return 0, 0, errcode.New(errcode.ErrInvalidState, "hub capacity must be positive")
	}
	for assignmentID := range ledger.reservations {
		if _, connected := ledger.sessions[assignmentID]; connected {
			return 0, 0, errcode.New(errcode.ErrInvalidState,
				"assignment exists in reservation and session ledgers")
		}
	}
	reservedAssignments := make(map[string]struct{}, len(ledger.reservations)+len(ledger.successors))
	for assignmentID := range ledger.reservations {
		reservedAssignments[assignmentID] = struct{}{}
	}
	for _, successor := range ledger.successors {
		assignmentID := successor.GetAssignmentId()
		if _, connected := ledger.sessions[assignmentID]; !connected {
			reservedAssignments[assignmentID] = struct{}{}
		}
	}
	reserved, connected := len(reservedAssignments), len(ledger.sessions)
	if reserved+connected > int(capacity) {
		return 0, 0, errcode.New(errcode.ErrInvalidState, "hub capacity ledger overflow")
	}
	return reserved, connected, nil
}

func syncShardCapacityProjection(shard *hubv1.HubShardStorageRecord, ledger *hubCapacityLedger) error {
	reserved, connected, err := capacityLedgerCounts(ledger, shard.GetCapacity())
	if err != nil {
		return err
	}
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
	xKey, xxKey := successorsKey(pod), successorExpiryKey(pod)
	pipe.Del(ctx, rKey, rxKey, sKey, sxKey, xKey, xxKey)
	var reservationMaxExpiry, sessionMaxExpiry, successorMaxExpiry int64
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
	successorCapabilities := make([]string, 0, len(ledger.successors))
	for capability := range ledger.successors {
		successorCapabilities = append(successorCapabilities, capability)
	}
	sort.Strings(successorCapabilities)
	for _, capability := range successorCapabilities {
		rec := ledger.successors[capability]
		payload, err := proto.Marshal(rec)
		if err != nil {
			return err
		}
		pipe.HSet(ctx, xKey, capability, payload)
		pipe.ZAdd(ctx, xxKey, redis.Z{Score: float64(rec.GetExpiresAtMs()), Member: capability})
		if rec.GetExpiresAtMs() > successorMaxExpiry {
			successorMaxExpiry = rec.GetExpiresAtMs()
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
	if successorMaxExpiry > 0 {
		deadline := time.UnixMilli(successorMaxExpiry).Add(capacityLedgerRetentionGuard)
		pipe.PExpireAt(ctx, xKey, deadline)
		pipe.PExpireAt(ctx, xxKey, deadline)
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
		reservation.AssignmentExpiresAtMs < reservation.ExpiresAtMs || shardTTL <= 0 ||
		!reservationPlacementValid(reservation) {
		return ReserveResult{}, errcode.New(errcode.ErrInvalidArg, "hub reservation identity/expiry invalid")
	}
	successorField, successorFieldErr := successorCapability(pod, reservation)
	if successorFieldErr != nil {
		return ReserveResult{}, successorFieldErr
	}
	aKey, shardKeyName := authKey(pod), shardKey(pod)
	watchKeys := append([]string{aKey, shardKeyName, instanceTeardownProofKey(pod)}, capacityLedgerKeys(pod)...)
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
			existingSuccessorField, existingSuccessor, successorExists, successorExact :=
				successorMatches(ledger, pod, reservation)
			if session, ok := ledger.sessions[reservation.AssignmentID]; ok {
				if !sessionRecordMatches(session, pod, reservation) {
					out.Reason = "assignment-session-conflict"
					return nil
				}
				if successorExists && !successorExact {
					previous, decodeErr := decodeSuccessorCapability(existingSuccessorField, pod)
					if decodeErr != nil || !reservationRecordMatches(existingSuccessor, pod, reservation) ||
						reservation.PlacementVersion == 0 ||
						reservation.PlacementVersion <= previous.PlacementVersion {
						out.Reason = "assignment-successor-conflict"
						return nil
					}
					// A same-assignment Hub transfer can advance the canonical placement
					// operation after its initial seat check.  Only a strictly newer
					// version may rotate the pending capability; same-version forks and
					// stale callers fail closed.
					rotated := proto.Clone(existingSuccessor).(*hubv1.HubReservationStorageRecord)
					delete(ledger.successors, existingSuccessorField)
					ledger.successors[successorField] = rotated
					existingSuccessorField, existingSuccessor = successorField, rotated
					successorExists, successorExact = true, true
				}
				if !successorExists {
					ledger.successors[successorField] = &hubv1.HubReservationStorageRecord{
						PlayerId: reservation.PlayerID, AssignmentId: reservation.AssignmentID, HubPodName: pod,
						HubInstanceUid: reservation.InstanceUID, AuthEpoch: reservation.ProtocolEpoch,
						AuthWriterEpoch: reservation.WriterEpoch, CreatedAtMs: nowMs,
						ExpiresAtMs: reservation.ExpiresAtMs, AssignmentExpiresAtMs: reservation.AssignmentExpiresAtMs,
					}
				} else {
					if reservation.ExpiresAtMs > existingSuccessor.GetExpiresAtMs() {
						existingSuccessor.ExpiresAtMs = reservation.ExpiresAtMs
					}
					if reservation.AssignmentExpiresAtMs > existingSuccessor.GetAssignmentExpiresAtMs() {
						existingSuccessor.AssignmentExpiresAtMs = reservation.AssignmentExpiresAtMs
					}
					// The canonical field is deterministic; this guard makes a future
					// encoding migration fail closed instead of silently duplicating it.
					if existingSuccessorField != successorField {
						out.Reason = "assignment-successor-conflict"
						return nil
					}
				}
				// The live session already owns the physical seat.  The successor is
				// only a bounded handoff lease and is excluded from projection until
				// exact Departure removes this owner.
			} else if current, ok := ledger.reservations[reservation.AssignmentID]; ok {
				if !reservationRecordMatches(current, pod, reservation) {
					out.Reason = "assignment-reservation-conflict"
					return nil
				}
				if successorExists && !successorExact {
					out.Reason = "assignment-successor-conflict"
					return nil
				}
				if reservation.ExpiresAtMs > current.GetExpiresAtMs() {
					current.ExpiresAtMs = reservation.ExpiresAtMs
				}
				if reservation.AssignmentExpiresAtMs > current.GetAssignmentExpiresAtMs() {
					current.AssignmentExpiresAtMs = reservation.AssignmentExpiresAtMs
				}
				if successorExists {
					// A mixed-version allocator may have recreated a normal reservation
					// after old Departure while the successor key survived.  Collapse
					// the two exact leases back to the placement-bound successor.
					if current.GetExpiresAtMs() > existingSuccessor.GetExpiresAtMs() {
						existingSuccessor.ExpiresAtMs = current.GetExpiresAtMs()
					}
					if current.GetAssignmentExpiresAtMs() > existingSuccessor.GetAssignmentExpiresAtMs() {
						existingSuccessor.AssignmentExpiresAtMs = current.GetAssignmentExpiresAtMs()
					}
					delete(ledger.reservations, reservation.AssignmentID)
				}
			} else if successorExists {
				if !successorExact {
					previous, decodeErr := decodeSuccessorCapability(existingSuccessorField, pod)
					if decodeErr != nil || !reservationRecordMatches(existingSuccessor, pod, reservation) ||
						reservation.PlacementVersion == 0 ||
						reservation.PlacementVersion <= previous.PlacementVersion {
						out.Reason = "assignment-successor-conflict"
						return nil
					}
					rotated := proto.Clone(existingSuccessor).(*hubv1.HubReservationStorageRecord)
					delete(ledger.successors, existingSuccessorField)
					ledger.successors[successorField] = rotated
					existingSuccessorField, existingSuccessor = successorField, rotated
					successorExact = true
				}
				// Old owner already departed; this exact successor is now the one
				// reserved seat.  Retrying IssueDSTicket only refreshes its bound.
				if reservation.ExpiresAtMs > existingSuccessor.GetExpiresAtMs() {
					existingSuccessor.ExpiresAtMs = reservation.ExpiresAtMs
				}
				if reservation.AssignmentExpiresAtMs > existingSuccessor.GetAssignmentExpiresAtMs() {
					existingSuccessor.AssignmentExpiresAtMs = reservation.AssignmentExpiresAtMs
				}
			} else {
				reserved, connected, countErr := capacityLedgerCounts(ledger, shard.GetCapacity())
				if countErr != nil {
					return countErr
				}
				if reserved+connected >= int(shard.GetCapacity()) {
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
			casConflictBackoff(ctx, attempt)
			continue
		}
		return out, txErr
	}
	return ReserveResult{}, errcode.New(errcode.ErrUnavailable, "hub reserve assignment %s: cas retry exhausted", pod)
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
		reservation.WriterEpoch != authpkg.DSAuthWriterEpochV2 || admissionSeq == 0 ||
		!reservationPlacementValid(reservation) {
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
			successorField, successor, successorExists, successorExact :=
				successorMatches(ledger, pod, reservation)
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
					// A new physical owner must consume the exact successor prepared by
					// IssueDSTicket.  Merely observing an old live session is not an
					// admission capability: without this lease, reject fail-closed.
					if !successorExists || !successorExact || successor.GetExpiresAtMs() <= nowMs {
						rejected = true
						return nil
					}
					delete(ledger.successors, successorField)
					// 只有更大 seq 才允许重连替换 owner；迟到旧 ACK 永远不能反向夺回。
					current.AdmissionId = admissionID
					current.AdmissionSeq = admissionSeq
				}
				current.LastSeenMs = nowMs
				current.ExpiresAtMs = 0
			} else {
				pending, ok := ledger.reservations[reservation.AssignmentID]
				pendingExact := !ok || reservationRecordMatches(pending, pod, reservation)
				normalExact := ok && pendingExact && pending.GetExpiresAtMs() > nowMs
				successorUsable := successorExists && successorExact && successor.GetExpiresAtMs() > nowMs
				if !pendingExact || (successorExists && !successorExact) || (!normalExact && !successorUsable) {
					rejected = true
					return nil
				}
				// Consume every exact representation in one transaction.  The normal
				// lease overlap is tolerated only for mixed-version rollout; neither
				// representation can be replayed after this point.
				if normalExact {
					delete(ledger.reservations, reservation.AssignmentID)
				}
				if successorUsable {
					delete(ledger.successors, successorField)
				}
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
			casConflictBackoff(ctx, attempt)
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
	return AdmissionResult{}, errcode.New(errcode.ErrUnavailable, "hub admission %s: cas retry exhausted", pod)
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
				// A detached successor proves that this exact assignment previously
				// had an owner whose Departure won.  Preserve it and report an exact
				// retry as idempotent success.  A plain reservation still means the
				// assignment never completed Admission and remains a conflict.
				_, successor, hasSuccessor := successorForAssignment(ledger, reservation.AssignmentID)
				if hasSuccessor && !reservationRecordMatches(successor, pod, reservation) {
					out.Conflict = true
					break
				}
				if pending, reserved := ledger.reservations[reservation.AssignmentID]; reserved {
					if !reservationRecordMatches(pending, pod, reservation) || !hasSuccessor {
						out.Conflict = true
					} else {
						out.Departed = true
						out.AlreadyDeparted = true
					}
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
				// Do not delete the exact successor.  In this same transaction the
				// projection reinterprets it from a zero-cost shadow into the one
				// reserved seat that bridges old Departure -> new Admission.
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
			casConflictBackoff(ctx, attempt)
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
	return DepartureResult{}, errcode.New(errcode.ErrUnavailable, "hub departure %s: cas retry exhausted", pod)
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
		PlacementVersion: expected.PlacementVersion, PlacementOperationID: expected.PlacementOperationID,
		SourceMatchID: expected.SourceMatchID,
	}
	if !reservationPlacementValid(identity) {
		return AssignmentSeatSnapshot{}, errcode.New(errcode.ErrInvalidArg,
			"assignment seat inspection placement identity invalid")
	}
	watchKeys := []string{shardKey(pod), reservationsKey(pod), sessionsKey(pod), successorsKey(pod)}
	for attempt := 0; attempt < hubAuthCASRetries; attempt++ {
		out := AssignmentSeatSnapshot{}
		txErr := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			capacity := int32(0)
			if raw, err := tx.Get(ctx, shardKey(pod)).Bytes(); err == nil {
				shard, decodeErr := unmarshalShard(pod, raw)
				if decodeErr != nil {
					return decodeErr
				}
				capacity = shard.GetCapacity()
			} else if err != redis.Nil {
				return err
			}
			successors, err := loadBoundedSuccessors(ctx, tx, pod, capacity)
			if err != nil {
				return err
			}
			nowMs := time.Now().UnixMilli()
			_, successor, successorExists, successorConflict :=
				successorForCleanup(successors, pod, identity, nowMs)
			if successorConflict {
				out.Conflict = true
				return nil
			}
			if successorExists && successor.GetExpiresAtMs() <= nowMs {
				// An expired handoff is no longer a physical or capacity owner.  The
				// read-only inspector may ignore it; Release or key retention removes
				// the stale bytes without waiting for a heartbeat.
				successor, successorExists = nil, false
			}

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
			if reservation != nil && session != nil {
				return errcode.New(errcode.ErrInvalidState,
					"assignment exists in reservation and session ledgers")
			}
			switch {
			case reservation == nil && session == nil && !successorExists:
				out.AlreadyAbsent = true
			case session != nil:
				if !sessionRecordMatches(session, pod, identity) || session.GetAdmissionId() == "" ||
					session.GetAdmissionSeq() == 0 {
					out = AssignmentSeatSnapshot{Conflict: true}
				} else {
					out = AssignmentSeatSnapshot{Connected: true, AdmissionID: session.GetAdmissionId(),
						AdmissionSeq: session.GetAdmissionSeq()}
				}
			default:
				if (reservation != nil && !reservationRecordMatches(reservation, pod, identity)) ||
					(successorExists && !reservationRecordMatches(successor, pod, identity)) {
					out = AssignmentSeatSnapshot{Conflict: true}
				} else {
					expiresAt := int64(0)
					if reservation != nil {
						expiresAt = reservation.GetExpiresAtMs()
					}
					if successorExists && successor.GetExpiresAtMs() > expiresAt {
						expiresAt = successor.GetExpiresAtMs()
					}
					out = AssignmentSeatSnapshot{Reserved: true, ReservationExpiresAtMs: expiresAt}
				}
			}
			// A read-only MULTI/EXEC validates the WATCH snapshot without
			// refreshing any TTL or mutating canonical state.
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Exists(ctx, watchKeys...)
				return nil
			})
			return err
		}, watchKeys...)
		if txErr == redis.TxFailedErr {
			casConflictBackoff(ctx, attempt)
			continue
		}
		return out, txErr
	}
	return AssignmentSeatSnapshot{}, errcode.New(errcode.ErrUnavailable,
		"hub assignment seat inspection %s: cas retry exhausted", pod)
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
		PlacementVersion: expected.PlacementVersion, PlacementOperationID: expected.PlacementOperationID,
		SourceMatchID: expected.SourceMatchID,
	}
	if !reservationPlacementValid(identity) {
		return ReleaseAssignmentSeatResult{}, errcode.New(errcode.ErrInvalidArg,
			"assignment seat release placement identity invalid")
	}
	aKey, shardKeyName := authKey(pod), shardKey(pod)
	watchKeys := append([]string{aKey, shardKeyName}, capacityLedgerKeys(pod)...)
	for attempt := 0; attempt < hubAuthCASRetries; attempt++ {
		out := ReleaseAssignmentSeatResult{}
		txErr := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			scanCapacity := int32(0)
			if raw, err := tx.Get(ctx, shardKeyName).Bytes(); err == nil {
				scanShard, decodeErr := unmarshalShard(pod, raw)
				if decodeErr != nil {
					return decodeErr
				}
				scanCapacity = scanShard.GetCapacity()
			} else if err != redis.Nil {
				return err
			}
			successors, err := loadBoundedSuccessors(ctx, tx, pod, scanCapacity)
			if err != nil {
				return err
			}
			successorField, successor, successorExists, successorConflict :=
				successorForCleanup(successors, pod, identity, time.Now().UnixMilli())
			if successorConflict {
				out.Conflict = true
				return nil
			}
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
			successorMatchesExpected := !successorExists || reservationRecordMatches(successor, pod, identity)
			if reservation != nil && session != nil {
				return errcode.New(errcode.ErrInvalidState, "assignment exists in reservation and session ledgers")
			}
			if reservation == nil && session == nil && !successorExists {
				out.AlreadyAbsent = true
				// WATCH has no linearization effect until EXEC.  Cleanup treats
				// AlreadyAbsent as authoritative physical-absence proof, so validate
				// this exact empty snapshot with a read-only transaction before
				// returning success.
				_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
					pipe.Exists(ctx, watchKeys...)
					return nil
				})
				return err
			}
			if (reservation != nil && !reservationMatches) || (session != nil && !sessionMatches) ||
				!successorMatchesExpected {
				out.Conflict = true
				return nil
			}
			if sessionMatches {
				// A fully activated replacement UID/epoch is an authoritative
				// teardown proof for the old GameServer process.  Only the exact
				// current auth+shard projection may unlock this path; heartbeat
				// staleness, missing Redis keys, or a half-initialized replacement
				// still require the old DS Logout ACK.
				var currentAuth *hubv1.HubShardAuthStorageRecord
				if raw, err := tx.Get(ctx, aKey).Bytes(); err == nil {
					currentAuth = &hubv1.HubShardAuthStorageRecord{}
					if err := proto.Unmarshal(raw, currentAuth); err != nil {
						return err
					}
				} else if err != redis.Nil {
					return err
				}
				var currentShard *hubv1.HubShardStorageRecord
				if raw, err := tx.Get(ctx, shardKeyName).Bytes(); err == nil {
					var decodeErr error
					currentShard, decodeErr = unmarshalShard(pod, raw)
					if decodeErr != nil {
						return decodeErr
					}
				} else if err != redis.Nil {
					return err
				}
				active := currentAuth.GetActive()
				superseded := currentAuth != nil && currentShard != nil && active != nil &&
					hubAuthRecordV2Exact(currentAuth) && currentAuth.GetPodName() == pod &&
					currentShard.GetGameserverUid() == currentAuth.GetInstanceUid() &&
					currentShard.GetAuthEpoch() == currentAuth.GetProtocolEpoch() &&
					currentShard.GetLastVerifiedWriterEpoch() == active.GetWriterEpoch() &&
					(currentAuth.GetInstanceUid() != expected.InstanceUID ||
						currentAuth.GetProtocolEpoch() != expected.ProtocolEpoch)
				teardownConfirmed, err := tx.HExists(ctx, instanceTeardownProofKey(pod), expected.InstanceUID).Result()
				if err != nil {
					return err
				}
				if !superseded && !teardownConfirmed {
					// Release/Transfer cancels the not-yet-consumed handoff capability,
					// but it must never delete the physical owner.  If Admission won
					// concurrently, WATCH retries and the caller will evict that owner.
					if successor != nil {
						_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
							pipe.HDel(ctx, successorsKey(pod), successorField)
							pipe.ZRem(ctx, successorExpiryKey(pod), successorField)
							return nil
						})
						if err != nil {
							return err
						}
					}
					out.DepartureRequired = true
					return nil
				}
				// When the torn-down UID is still the current projection, update the
				// derived capacity in the same transaction.  A replacement projection
				// must never be rewritten from the old UID's ledger.
				currentProjection := currentAuth != nil && currentShard != nil && active != nil &&
					hubAuthRecordV2Exact(currentAuth) && currentAuth.GetPodName() == pod &&
					currentAuth.GetInstanceUid() == expected.InstanceUID &&
					currentAuth.GetProtocolEpoch() == expected.ProtocolEpoch &&
					active.GetWriterEpoch() == expected.WriterEpoch &&
					currentShard.GetGameserverUid() == expected.InstanceUID &&
					currentShard.GetAuthEpoch() == expected.ProtocolEpoch &&
					currentShard.GetLastVerifiedWriterEpoch() == expected.WriterEpoch
				if teardownConfirmed && currentProjection {
					ledger, loadErr := loadHubCapacityLedger(ctx, tx, pod, currentShard.GetCapacity())
					if loadErr != nil {
						return loadErr
					}
					delete(ledger.sessions, expected.AssignmentID)
					delete(ledger.successors, successorField)
					if syncErr := syncShardCapacityProjection(currentShard, ledger); syncErr != nil {
						return syncErr
					}
					payload, marshalErr := marshalShard(currentShard)
					if marshalErr != nil {
						return marshalErr
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
				}
				_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
					pipe.HDel(ctx, sessionsKey(pod), expected.AssignmentID)
					pipe.ZRem(ctx, sessionExpiryKey(pod), expected.AssignmentID)
					pipe.HDel(ctx, successorsKey(pod), successorField)
					pipe.ZRem(ctx, successorExpiryKey(pod), successorField)
					return nil
				})
				if err == nil {
					out.Released = true
				}
				return err
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
					pipe.HDel(ctx, successorsKey(pod), successorField)
					pipe.ZRem(ctx, successorExpiryKey(pod), successorField)
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
			delete(ledger.successors, successorField)
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
			casConflictBackoff(ctx, attempt)
			continue
		}
		return out, txErr
	}
	return ReleaseAssignmentSeatResult{}, errcode.New(errcode.ErrUnavailable,
		"hub assignment release %s: cas retry exhausted", pod)
}

// RemoveCapacityLedger 仅在分片已被确认回收后清派生账；活分片绝不调用。
func (r *RedisHubAuthRepo) RemoveCapacityLedger(ctx context.Context, pod string) error {
	if pod == "" {
		return errcode.New(errcode.ErrInvalidArg, "hub pod required")
	}
	return r.rdb.Del(ctx, capacityLedgerKeys(pod)...).Err()
}
