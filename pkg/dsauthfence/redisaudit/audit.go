// Package redisaudit 审计 Redis 中所有 Hub/Battle active credential 与业务投影。
// 它只读，不修复数据；任何坏 proto、半激活、pending、旧 writer 或心跳不推进都会阻断激活。
package redisaudit

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
)

const (
	hubPattern              = "pandora:hub:auth:{*}"
	hubProjectionPattern    = "pandora:hub:shard:{*}"
	battlePattern           = "pandora:ds:auth:{*}"
	battleProjectionPattern = "pandora:ds:battle:{*}"
	battleAllocationUnknown = "allocation_uncertain"
	battlePreactiveRelease  = "preactive_release_pending"
)

type HubRecord struct {
	Key   string
	Auth  *hubv1.HubShardAuthStorageRecord
	Shard *hubv1.HubShardStorageRecord
}

type BattleRecord struct {
	Key    string
	Auth   *dsv1.BattleDSAuthStorageRecord
	Battle *dsv1.BattleStorageRecord
}

type Snapshot struct {
	Hubs    map[string]HubRecord
	Battles map[string]BattleRecord
	// SafetyFindings 是扫描投影时发现、但不能安全自动修复的阻断项。
	// 典型例子是 GSA 结果未知后永久保留的 allocation_uncertain：它保证不会二次 POST，
	// 却必须由具备外部幂等证明的人工/controller 决策，绝不能被激活审计静默忽略。
	SafetyFindings []string
}

type Policy struct {
	TargetWriterEpoch uint32
	MaxHeartbeatAge   time.Duration
	MinHubs           int
	MinBattles        int
}

// ReadSnapshot 对每个 auth record 同步读取业务投影；读取/解码失败立即返回 error。
func ReadSnapshot(ctx context.Context, client redis.UniversalClient) (Snapshot, error) {
	if client == nil {
		return Snapshot{}, fmt.Errorf("nil redis client")
	}
	hubKeys, err := scanKeys(ctx, client, hubPattern)
	if err != nil {
		return Snapshot{}, fmt.Errorf("scan hub auth: %w", err)
	}
	battleKeys, err := scanKeys(ctx, client, battlePattern)
	if err != nil {
		return Snapshot{}, fmt.Errorf("scan battle auth: %w", err)
	}
	hubProjectionKeys, err := scanKeys(ctx, client, hubProjectionPattern)
	if err != nil {
		return Snapshot{}, fmt.Errorf("scan hub projections: %w", err)
	}
	battleProjectionKeys, err := scanKeys(ctx, client, battleProjectionPattern)
	if err != nil {
		return Snapshot{}, fmt.Errorf("scan battle projections: %w", err)
	}
	hubAuthSet := keySet(hubKeys)
	battleAuthSet := keySet(battleKeys)
	safetyFindings := make([]string, 0)
	// 必须反向扫描 live projection；只从 auth 推导会漏掉 legacy ready/running 记录。
	for _, key := range hubProjectionKeys {
		raw, getErr := client.Get(ctx, key).Bytes()
		if getErr != nil {
			return Snapshot{}, fmt.Errorf("get %s: %w", key, getErr)
		}
		shard := &hubv1.HubShardStorageRecord{}
		if unmarshalErr := proto.Unmarshal(raw, shard); unmarshalErr != nil {
			return Snapshot{}, fmt.Errorf("unmarshal %s: %w", key, unmarshalErr)
		}
		if shard.GetState() == "ready" {
			authKey := fmt.Sprintf("pandora:hub:auth:{%s}", shard.GetHubPodName())
			if _, ok := hubAuthSet[authKey]; !ok {
				return Snapshot{}, fmt.Errorf("live Hub projection %s has no auth record", key)
			}
		}
	}
	for _, key := range battleProjectionKeys {
		raw, getErr := client.Get(ctx, key).Bytes()
		if getErr != nil {
			return Snapshot{}, fmt.Errorf("get %s: %w", key, getErr)
		}
		battle := &dsv1.BattleStorageRecord{}
		if unmarshalErr := proto.Unmarshal(raw, battle); unmarshalErr != nil {
			return Snapshot{}, fmt.Errorf("unmarshal %s: %w", key, unmarshalErr)
		}
		if battle.GetState() == "ready" || battle.GetState() == "running" {
			authKey := fmt.Sprintf("pandora:ds:auth:{%d}", battle.GetMatchId())
			if _, ok := battleAuthSet[authKey]; !ok {
				return Snapshot{}, fmt.Errorf("live Battle projection %s has no auth record", key)
			}
		}
		switch battle.GetState() {
		case battleAllocationUnknown:
			safetyFindings = append(safetyFindings, fmt.Sprintf(
				"Battle projection %s is allocation_uncertain (match_id=%d allocation_id=%q pod=%q); explicit authoritative recovery required",
				key, battle.GetMatchId(), battle.GetAllocationId(), battle.GetDsPodName()))
		case battlePreactiveRelease:
			safetyFindings = append(safetyFindings, fmt.Sprintf(
				"Battle projection %s is preactive_release_pending (match_id=%d allocation_id=%q pod=%q uid=%q); UID-precondition release is not yet confirmed",
				key, battle.GetMatchId(), battle.GetAllocationId(), battle.GetDsPodName(), battle.GetGameserverUid()))
		case "ended", "abandoned":
			// 成功完成外部 release/lifecycle 后终态墓碑会恢复有限 TTL；PTTL=-1 表示
			// 回收或补偿仍未确认，必须像 allocation_uncertain 一样阻断 activation。
			ttl, ttlErr := client.PTTL(ctx, key).Result()
			if ttlErr != nil {
				return Snapshot{}, fmt.Errorf("pttl %s: %w", key, ttlErr)
			}
			if ttl == -1 {
				safetyFindings = append(safetyFindings, fmt.Sprintf(
					"Battle projection %s is a persistent terminal fence (state=%s match_id=%d allocation_id=%q); release/lifecycle recovery is incomplete",
					key, battle.GetState(), battle.GetMatchId(), battle.GetAllocationId()))
			}
		}
	}
	out := Snapshot{
		Hubs:           make(map[string]HubRecord, len(hubKeys)),
		Battles:        make(map[string]BattleRecord, len(battleKeys)),
		SafetyFindings: safetyFindings,
	}
	for _, key := range hubKeys {
		raw, err := client.Get(ctx, key).Bytes()
		if err != nil {
			return Snapshot{}, fmt.Errorf("get %s: %w", key, err)
		}
		auth := &hubv1.HubShardAuthStorageRecord{}
		if err := proto.Unmarshal(raw, auth); err != nil {
			return Snapshot{}, fmt.Errorf("unmarshal %s: %w", key, err)
		}
		shardKey := fmt.Sprintf("pandora:hub:shard:{%s}", auth.GetPodName())
		shardRaw, err := client.Get(ctx, shardKey).Bytes()
		if err != nil {
			return Snapshot{}, fmt.Errorf("get projection %s: %w", shardKey, err)
		}
		shard := &hubv1.HubShardStorageRecord{}
		if err := proto.Unmarshal(shardRaw, shard); err != nil {
			return Snapshot{}, fmt.Errorf("unmarshal %s: %w", shardKey, err)
		}
		out.Hubs[key] = HubRecord{Key: key, Auth: auth, Shard: shard}
	}
	for _, key := range battleKeys {
		raw, err := client.Get(ctx, key).Bytes()
		if err != nil {
			return Snapshot{}, fmt.Errorf("get %s: %w", key, err)
		}
		auth := &dsv1.BattleDSAuthStorageRecord{}
		if err := proto.Unmarshal(raw, auth); err != nil {
			return Snapshot{}, fmt.Errorf("unmarshal %s: %w", key, err)
		}
		battleKey := fmt.Sprintf("pandora:ds:battle:{%d}", auth.GetMatchId())
		battleRaw, err := client.Get(ctx, battleKey).Bytes()
		if err != nil {
			return Snapshot{}, fmt.Errorf("get projection %s: %w", battleKey, err)
		}
		battle := &dsv1.BattleStorageRecord{}
		if err := proto.Unmarshal(battleRaw, battle); err != nil {
			return Snapshot{}, fmt.Errorf("unmarshal %s: %w", battleKey, err)
		}
		out.Battles[key] = BattleRecord{Key: key, Auth: auth, Battle: battle}
	}
	return out, nil
}

// ValidateSnapshot 要求所有扫描到的 live 记录均为稳定 active、无 pending、投影精确一致。
func ValidateSnapshot(snapshot Snapshot, now time.Time, policy Policy) []string {
	if policy.TargetWriterEpoch == 0 {
		policy.TargetWriterEpoch = 2
	}
	if policy.MaxHeartbeatAge <= 0 {
		policy.MaxHeartbeatAge = 30 * time.Second
	}
	findings := append([]string(nil), snapshot.SafetyFindings...)
	if len(snapshot.Hubs) < policy.MinHubs {
		findings = append(findings, fmt.Sprintf("Hub active records=%d < min=%d", len(snapshot.Hubs), policy.MinHubs))
	}
	if len(snapshot.Battles) < policy.MinBattles {
		findings = append(findings, fmt.Sprintf("Battle active records=%d < min=%d", len(snapshot.Battles), policy.MinBattles))
	}
	nowMs := now.UnixMilli()
	for key, item := range snapshot.Hubs {
		a, s := item.Auth, item.Shard
		prefix := "Hub " + key + ": "
		if a == nil || s == nil {
			findings = append(findings, prefix+"auth/shard missing")
			continue
		}
		if key != fmt.Sprintf("pandora:hub:auth:{%s}", a.GetPodName()) {
			findings = append(findings, prefix+"key/pod mismatch")
		}
		if a.GetPhase() != hubv1.HubAuthPhase_HUB_AUTH_PHASE_ACTIVE || a.GetPending() != nil {
			findings = append(findings, prefix+"phase not stable ACTIVE or pending exists")
		}
		c := a.GetActive()
		if !hubCredentialComplete(a, c, nowMs, policy.TargetWriterEpoch) {
			findings = append(findings, prefix+"active credential incomplete/expired/writer mismatch")
			continue
		}
		if !heartbeatFresh(a.GetLastActiveHeartbeatMs(), nowMs, policy.MaxHeartbeatAge) {
			findings = append(findings, prefix+"active heartbeat missing/future/stale")
		}
		if s.GetState() != "ready" || s.GetHubPodName() != a.GetPodName() ||
			s.GetGameserverUid() != a.GetInstanceUid() || s.GetAuthEpoch() != a.GetProtocolEpoch() ||
			s.GetLastVerifiedGen() != c.GetGen() || s.GetLastVerifiedJti() != c.GetJti() ||
			s.GetLastVerifiedWriterEpoch() != c.GetWriterEpoch() {
			findings = append(findings, prefix+"ready projection does not equal active tuple")
		}
	}
	for key, item := range snapshot.Battles {
		a, b := item.Auth, item.Battle
		prefix := "Battle " + key + ": "
		if a == nil || b == nil {
			findings = append(findings, prefix+"auth/battle missing")
			continue
		}
		if key != fmt.Sprintf("pandora:ds:auth:{%d}", a.GetMatchId()) {
			findings = append(findings, prefix+"key/match mismatch")
		}
		if a.GetPhase() != dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_ACTIVE || a.GetPending() != nil {
			findings = append(findings, prefix+"phase not stable ACTIVE or pending exists")
		}
		c := a.GetActive()
		if !battleCredentialComplete(a, c, nowMs, policy.TargetWriterEpoch) {
			findings = append(findings, prefix+"active credential incomplete/expired/writer mismatch")
			continue
		}
		if !heartbeatFresh(a.GetLastActiveHeartbeatMs(), nowMs, policy.MaxHeartbeatAge) {
			findings = append(findings, prefix+"active heartbeat missing/future/stale")
		}
		if b.GetState() != "ready" && b.GetState() != "running" {
			findings = append(findings, prefix+"battle projection is not live")
		}
		if a.GetAllocationId() == "" || b.GetAllocationId() != a.GetAllocationId() ||
			b.GetMatchId() != a.GetMatchId() || b.GetDsPodName() != a.GetDsPodName() ||
			b.GetGameserverUid() != a.GetInstanceUid() || b.GetInstanceEpoch() != a.GetInstanceEpoch() ||
			b.GetLastVerifiedGen() != c.GetGen() || b.GetLastVerifiedJti() != c.GetJti() ||
			b.GetLastVerifiedWriterEpoch() != c.GetWriterEpoch() {
			findings = append(findings, prefix+"live projection does not equal active tuple/allocation")
		}
	}
	sort.Strings(findings)
	return findings
}

// CompareProgress 要求连续样本 key/credential 身份不变，且每个服务端心跳严格前进。
func CompareProgress(previous, current Snapshot) []string {
	findings := make([]string, 0)
	compare := func(kind string, before, after map[string]string, beforeHB, afterHB map[string]int64) {
		if strings.Join(sortedMapKeys(before), "\n") != strings.Join(sortedMapKeys(after), "\n") {
			findings = append(findings, kind+" active key set changed during stability window")
			return
		}
		for key, identity := range before {
			if after[key] != identity {
				findings = append(findings, fmt.Sprintf("%s %s active identity changed", kind, key))
			}
			if afterHB[key] <= beforeHB[key] {
				findings = append(findings, fmt.Sprintf("%s %s server heartbeat did not advance", kind, key))
			}
		}
	}
	ph, ch := hubIdentities(previous), hubIdentities(current)
	pb, cb := battleIdentities(previous), battleIdentities(current)
	compare("Hub", ph.identities, ch.identities, ph.heartbeats, ch.heartbeats)
	compare("Battle", pb.identities, cb.identities, pb.heartbeats, cb.heartbeats)
	sort.Strings(findings)
	return findings
}

type identitySet struct {
	identities map[string]string
	heartbeats map[string]int64
}

func hubIdentities(s Snapshot) identitySet {
	out := identitySet{map[string]string{}, map[string]int64{}}
	for key, item := range s.Hubs {
		a, c := item.Auth, item.Auth.GetActive()
		out.identities[key] = fmt.Sprintf("%s/%d/%d/%s/%d/%s/%s/%d/%d/%d/%d",
			a.GetInstanceUid(), a.GetProtocolEpoch(), c.GetGen(), c.GetJti(), c.GetExpMs(),
			c.GetKid(), c.GetTokenSha256(), c.GetWriterEpoch(), a.GetRequiredWriterEpoch(),
			a.GetHighWaterGen(), a.GetPhase())
		out.heartbeats[key] = a.GetLastActiveHeartbeatMs()
	}
	return out
}

func battleIdentities(s Snapshot) identitySet {
	out := identitySet{map[string]string{}, map[string]int64{}}
	for key, item := range s.Battles {
		a, c := item.Auth, item.Auth.GetActive()
		out.identities[key] = fmt.Sprintf("%s/%s/%s/%d/%d/%s/%d/%s/%s/%d/%d/%d/%d",
			a.GetAllocationId(), a.GetDsPodName(), a.GetInstanceUid(), a.GetInstanceEpoch(),
			c.GetGen(), c.GetJti(), c.GetExpMs(), c.GetKid(), c.GetTokenSha256(),
			c.GetWriterEpoch(), a.GetRequiredWriterEpoch(), a.GetHighWaterGen(), a.GetPhase())
		out.heartbeats[key] = a.GetLastActiveHeartbeatMs()
	}
	return out
}

func hubCredentialComplete(a *hubv1.HubShardAuthStorageRecord, c *hubv1.HubDSCredential, nowMs int64, target uint32) bool {
	return a.GetRequiredWriterEpoch() == target && a.GetHighWaterGen() >= c.GetGen() && c != nil &&
		a.GetPodName() != "" && a.GetInstanceUid() != "" && a.GetProtocolEpoch() > 0 &&
		c.GetGen() > 0 && c.GetJti() != "" && c.GetExpMs() > uint64(nowMs) && c.GetKid() != "" &&
		c.GetTokenSha256() != "" && c.GetInstanceUid() == a.GetInstanceUid() &&
		c.GetProtocolEpoch() == a.GetProtocolEpoch() && c.GetWriterEpoch() == target
}

func battleCredentialComplete(a *dsv1.BattleDSAuthStorageRecord, c *dsv1.BattleDSCredential, nowMs int64, target uint32) bool {
	return a.GetRequiredWriterEpoch() == target && a.GetHighWaterGen() >= c.GetGen() && c != nil &&
		a.GetMatchId() > 0 && a.GetDsPodName() != "" && a.GetInstanceUid() != "" && a.GetInstanceEpoch() > 0 &&
		c.GetGen() > 0 && c.GetJti() != "" && c.GetExpMs() > uint64(nowMs) && c.GetKid() != "" &&
		c.GetTokenSha256() != "" && c.GetInstanceUid() == a.GetInstanceUid() &&
		c.GetInstanceEpoch() == a.GetInstanceEpoch() && c.GetWriterEpoch() == target
}

func heartbeatFresh(value, nowMs int64, maxAge time.Duration) bool {
	return value > 0 && value <= nowMs && nowMs-value <= maxAge.Milliseconds()
}

func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func keySet(keys []string) map[string]struct{} {
	out := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		out[key] = struct{}{}
	}
	return out
}

func scanKeys(ctx context.Context, client redis.UniversalClient, pattern string) ([]string, error) {
	set := make(map[string]struct{})
	scan := func(c redis.UniversalClient) error {
		var cursor uint64
		for {
			keys, next, err := c.Scan(ctx, cursor, pattern, 256).Result()
			if err != nil {
				return err
			}
			for _, key := range keys {
				set[key] = struct{}{}
			}
			cursor = next
			if cursor == 0 {
				return nil
			}
		}
	}
	if cluster, ok := client.(*redis.ClusterClient); ok {
		if err := cluster.ForEachMaster(ctx, func(ctx context.Context, master *redis.Client) error { return scan(master) }); err != nil {
			return nil, err
		}
	} else if err := scan(client); err != nil {
		return nil, err
	}
	keys := sortedMapKeys(set)
	return keys, nil
}
