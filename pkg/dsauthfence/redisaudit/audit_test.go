package redisaudit

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
)

func validSnapshot(now time.Time) Snapshot {
	nowMs, exp := now.UnixMilli(), uint64(now.Add(time.Hour).UnixMilli())
	hc := &hubv1.HubDSCredential{Gen: 2, Jti: "hj", ExpMs: exp, Kid: "kid", InstanceUid: "hu", ProtocolEpoch: 1, TokenSha256: "hash", WriterEpoch: 2}
	ha := &hubv1.HubShardAuthStorageRecord{PodName: "hub-1", InstanceUid: "hu", ProtocolEpoch: 1, Phase: hubv1.HubAuthPhase_HUB_AUTH_PHASE_ACTIVE, Active: hc, HighWaterGen: 2, RequiredWriterEpoch: 2, LastActiveHeartbeatMs: nowMs - 1000}
	hs := &hubv1.HubShardStorageRecord{HubPodName: "hub-1", State: "ready", GameserverUid: "hu", AuthEpoch: 1, LastVerifiedGen: 2, LastVerifiedJti: "hj", LastVerifiedWriterEpoch: 2}
	bc := &dsv1.BattleDSCredential{Gen: 3, Jti: "bj", ExpMs: exp, Kid: "kid", InstanceUid: "bu", InstanceEpoch: 4, TokenSha256: "hash", WriterEpoch: 2}
	ba := &dsv1.BattleDSAuthStorageRecord{MatchId: 9, DsPodName: "battle-9", InstanceUid: "bu", InstanceEpoch: 4, Phase: dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_ACTIVE, Active: bc, HighWaterGen: 3, RequiredWriterEpoch: 2, AllocationId: "alloc", LastActiveHeartbeatMs: nowMs - 1000}
	bs := &dsv1.BattleStorageRecord{MatchId: 9, DsPodName: "battle-9", State: "running", GameserverUid: "bu", InstanceEpoch: 4, LastVerifiedGen: 3, LastVerifiedJti: "bj", LastVerifiedWriterEpoch: 2, AllocationId: "alloc"}
	return Snapshot{Hubs: map[string]HubRecord{"pandora:hub:auth:{hub-1}": {Key: "pandora:hub:auth:{hub-1}", Auth: ha, Shard: hs}}, Battles: map[string]BattleRecord{"pandora:ds:auth:{9}": {Key: "pandora:ds:auth:{9}", Auth: ba, Battle: bs}}}
}

func TestReadSnapshotFromRedis(t *testing.T) {
	now := time.Now()
	want := validSnapshot(now)
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	for key, item := range want.Hubs {
		authRaw, _ := proto.Marshal(item.Auth)
		shardRaw, _ := proto.Marshal(item.Shard)
		if err := mr.Set(key, string(authRaw)); err != nil {
			t.Fatal(err)
		}
		if err := mr.Set("pandora:hub:shard:{"+item.Auth.GetPodName()+"}", string(shardRaw)); err != nil {
			t.Fatal(err)
		}
	}
	for key, item := range want.Battles {
		authRaw, _ := proto.Marshal(item.Auth)
		battleRaw, _ := proto.Marshal(item.Battle)
		if err := mr.Set(key, string(authRaw)); err != nil {
			t.Fatal(err)
		}
		if err := mr.Set("pandora:ds:battle:{9}", string(battleRaw)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadSnapshot(context.Background(), client)
	if err != nil || len(got.Hubs) != 1 || len(got.Battles) != 1 {
		t.Fatalf("snapshot=%+v err=%v", got, err)
	}
	if findings := ValidateSnapshot(got, now, Policy{TargetWriterEpoch: 2, MinHubs: 1, MinBattles: 1}); len(findings) != 0 {
		t.Fatalf("findings=%v", findings)
	}
}

func TestReadSnapshotRejectsLiveProjectionWithoutAuth(t *testing.T) {
	for name, seed := range map[string]func(*testing.T, *miniredis.Miniredis){
		"hub": func(t *testing.T, mr *miniredis.Miniredis) {
			raw, err := proto.Marshal(&hubv1.HubShardStorageRecord{HubPodName: "legacy-hub", State: "ready"})
			if err != nil {
				t.Fatal(err)
			}
			if err := mr.Set("pandora:hub:shard:{legacy-hub}", string(raw)); err != nil {
				t.Fatal(err)
			}
		},
		"battle": func(t *testing.T, mr *miniredis.Miniredis) {
			raw, err := proto.Marshal(&dsv1.BattleStorageRecord{MatchId: 99, State: "running"})
			if err != nil {
				t.Fatal(err)
			}
			if err := mr.Set("pandora:ds:battle:{99}", string(raw)); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			seed(t, mr)
			client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			defer func() { _ = client.Close() }()
			if _, err := ReadSnapshot(context.Background(), client); err == nil {
				t.Fatal("live projection without auth accepted")
			}
		})
	}
}

func TestReadSnapshotFlagsPermanentRecoveryFences(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state string
	}{
		{name: "allocation response unknown", state: "allocation_uncertain"},
		{name: "preactive release unknown", state: "preactive_release_pending"},
		{name: "terminal release or lifecycle unknown", state: "abandoned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = client.Close() })
			record := &dsv1.BattleStorageRecord{
				MatchId: 77, AllocationId: "alloc-77", DsPodName: "battle-77",
				GameserverUid: "uid-77", State: tc.state,
			}
			raw, err := proto.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := mr.Set("pandora:ds:battle:{77}", string(raw)); err != nil {
				t.Fatal(err)
			}
			snapshot, err := ReadSnapshot(context.Background(), client)
			if err != nil {
				t.Fatalf("ReadSnapshot: %v", err)
			}
			findings := ValidateSnapshot(snapshot, time.Now(), Policy{})
			if len(findings) != 1 {
				t.Fatalf("findings=%v, want one permanent recovery blocker", findings)
			}
		})
	}
}

func TestReadSnapshotAllowsBoundedTerminalRetention(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	raw, err := proto.Marshal(&dsv1.BattleStorageRecord{
		MatchId: 77, AllocationId: "alloc-77", DsPodName: "battle-77", State: "ended",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Set(context.Background(), "pandora:ds:battle:{77}", raw, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := ReadSnapshot(context.Background(), client)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if findings := ValidateSnapshot(snapshot, time.Now(), Policy{}); len(findings) != 0 {
		t.Fatalf("bounded historical terminal record should not be a recovery blocker: %v", findings)
	}
}

func TestValidateAndProgress(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	first := validSnapshot(now)
	policy := Policy{TargetWriterEpoch: 2, MaxHeartbeatAge: 30 * time.Second, MinHubs: 1, MinBattles: 1}
	if got := ValidateSnapshot(first, now, policy); len(got) != 0 {
		t.Fatalf("findings=%v", got)
	}
	second := validSnapshot(now)
	second.Hubs["pandora:hub:auth:{hub-1}"].Auth.LastActiveHeartbeatMs += 5000
	second.Battles["pandora:ds:auth:{9}"].Auth.LastActiveHeartbeatMs += 5000
	if got := CompareProgress(first, second); len(got) != 0 {
		t.Fatalf("progress=%v", got)
	}
	second.Hubs["pandora:hub:auth:{hub-1}"].Auth.LastActiveHeartbeatMs = first.Hubs["pandora:hub:auth:{hub-1}"].Auth.LastActiveHeartbeatMs
	if got := CompareProgress(first, second); len(got) == 0 {
		t.Fatal("stuck heartbeat accepted")
	}
	second = validSnapshot(now)
	second.Hubs["pandora:hub:auth:{hub-1}"].Auth.LastActiveHeartbeatMs += 5000
	second.Battles["pandora:ds:auth:{9}"].Auth.LastActiveHeartbeatMs += 5000
	second.Hubs["pandora:hub:auth:{hub-1}"].Auth.HighWaterGen = 1
	if got := CompareProgress(first, second); len(got) == 0 {
		t.Fatal("high-water rollback accepted")
	}
}

func TestValidateRejectsPendingAndProjectionDrift(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	s := validSnapshot(now)
	s.Hubs["pandora:hub:auth:{hub-1}"].Auth.Pending = &hubv1.HubDSCredential{Gen: 3}
	s.Battles["pandora:ds:auth:{9}"].Battle.GameserverUid = "rebuilt"
	if got := ValidateSnapshot(s, now, Policy{TargetWriterEpoch: 2}); len(got) < 2 {
		t.Fatalf("findings=%v", got)
	}
}
