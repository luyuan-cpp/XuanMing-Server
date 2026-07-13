package data

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/dsauthrecord"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
)

func seedBattleAuthority(t *testing.T, client redis.UniversalClient, now time.Time) BattleResultCredential {
	t.Helper()
	exp := now.Add(time.Hour).UnixMilli()
	credential := BattleResultCredential{
		MatchID: 9, PodName: "battle-9", InstanceUID: "uid-9", InstanceEpoch: 3,
		Gen: 7, JTI: "j7", ExpMs: exp, Kid: "kid7", TokenSHA256: "hash7",
		WriterEpoch: auth.DSAuthWriterEpochV2,
	}
	active := &dsv1.BattleDSCredential{
		Gen: 7, Jti: "j7", ExpMs: uint64(exp), Kid: "kid7", InstanceUid: "uid-9",
		InstanceEpoch: 3, TokenSha256: "hash7", WriterEpoch: auth.DSAuthWriterEpochV2,
	}
	authRecord := &dsv1.BattleDSAuthStorageRecord{
		MatchId: 9, DsPodName: "battle-9", InstanceUid: "uid-9", InstanceEpoch: 3,
		Phase: dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_ACTIVE, Active: active,
		HighWaterGen: 7, RequiredWriterEpoch: auth.DSAuthWriterEpochV2,
		AllocationId: "alloc-9", LastActiveHeartbeatMs: now.Add(-time.Second).UnixMilli(),
	}
	battleRecord := &dsv1.BattleStorageRecord{
		MatchId: 9, DsPodName: "battle-9", State: "running", AllocationId: "alloc-9",
		GameserverUid: "uid-9", InstanceEpoch: 3, LastVerifiedGen: 7,
		LastVerifiedJti: "j7", LastVerifiedWriterEpoch: auth.DSAuthWriterEpochV2,
	}
	authRaw, err := proto.Marshal(authRecord)
	if err != nil {
		t.Fatal(err)
	}
	battleRaw, err := proto.Marshal(battleRecord)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.MSet(context.Background(), battleAuthKey(9), authRaw, battleKey(9), battleRaw).Err(); err != nil {
		t.Fatal(err)
	}
	if err := client.Expire(context.Background(), battleKey(9), 2*time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	return credential
}

func TestRedisBattleAuthoritySnapshotAndResultReceipt(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	credential := seedBattleAuthority(t, client, now)
	reader := NewRedisBattleAuthReader(client)
	reader.now = func() time.Time { return now }
	authRecord, battleRecord, found, err := reader.GetBattleAuthority(context.Background(), 9)
	if err != nil || !found || authRecord.GetDsPodName() != "battle-9" || battleRecord.GetAllocationId() != "alloc-9" {
		t.Fatalf("auth=%v battle=%v found=%v err=%v", authRecord, battleRecord, found, err)
	}
	if err := reader.RecordBattleResult(context.Background(), credential, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	// 响应丢失重试必须幂等，不得换 receipt 身份。
	if err := reader.RecordBattleResult(context.Background(), credential, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	raw, err := client.Get(context.Background(), dsauthrecord.BattleResultReceiptKey(9)).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := dsauthrecord.UnmarshalBattleResultReceipt(raw)
	if err != nil || receipt.AllocationID != "alloc-9" || receipt.JTI != "j7" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if ttl := mr.TTL(dsauthrecord.BattleResultReceiptKey(9)); ttl <= time.Hour {
		t.Fatalf("receipt TTL=%v was truncated by credential expiration", ttl)
	}
}

func TestRecordBattleResultRejectsAuthorityDriftWithNoReceipt(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	for _, mutate := range []func(*BattleResultCredential){
		func(c *BattleResultCredential) { c.JTI = "stale" },
		func(c *BattleResultCredential) { c.InstanceUID = "rebuilt" },
		func(c *BattleResultCredential) { c.WriterEpoch = 1 },
	} {
		mr := miniredis.RunT(t)
		client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		credential := seedBattleAuthority(t, client, now)
		mutate(&credential)
		reader := NewRedisBattleAuthReader(client)
		reader.now = func() time.Time { return now }
		if err := reader.RecordBattleResult(context.Background(), credential, 30*time.Second); err == nil {
			t.Fatal("authority drift accepted")
		}
		if client.Exists(context.Background(), dsauthrecord.BattleResultReceiptKey(9)).Val() != 0 {
			t.Fatal("rejected result changed receipt")
		}
		_ = client.Close()
	}
}

func TestRecordBattleResultV2RejectsLowAndFutureWriterWithoutRedisMutation(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	for _, tc := range []struct {
		name          string
		requiredEpoch uint32
		writerEpoch   uint32
	}{
		{name: "low-required-active-claims-1", requiredEpoch: 1, writerEpoch: 1},
		{name: "future-required-active-claims-3", requiredEpoch: 3, writerEpoch: 3},
		{name: "required-2-future-active-claims-3", requiredEpoch: 2, writerEpoch: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = client.Close() })
			credential := seedBattleAuthority(t, client, now)
			ctx := context.Background()
			authRaw, err := client.Get(ctx, battleAuthKey(9)).Bytes()
			if err != nil {
				t.Fatal(err)
			}
			battleRaw, err := client.Get(ctx, battleKey(9)).Bytes()
			if err != nil {
				t.Fatal(err)
			}
			authRecord := &dsv1.BattleDSAuthStorageRecord{}
			battleRecord := &dsv1.BattleStorageRecord{}
			if err := proto.Unmarshal(authRaw, authRecord); err != nil {
				t.Fatal(err)
			}
			if err := proto.Unmarshal(battleRaw, battleRecord); err != nil {
				t.Fatal(err)
			}
			authRecord.RequiredWriterEpoch = tc.requiredEpoch
			authRecord.Active.WriterEpoch = tc.writerEpoch
			authRecord.Pending = proto.Clone(authRecord.Active).(*dsv1.BattleDSCredential)
			battleRecord.LastVerifiedWriterEpoch = tc.writerEpoch
			credential.WriterEpoch = tc.writerEpoch
			authRaw, err = proto.Marshal(authRecord)
			if err != nil {
				t.Fatal(err)
			}
			battleRaw, err = proto.Marshal(battleRecord)
			if err != nil {
				t.Fatal(err)
			}
			if err := client.MSet(ctx, battleAuthKey(9), authRaw, battleKey(9), battleRaw).Err(); err != nil {
				t.Fatal(err)
			}
			reader := NewRedisBattleAuthReader(client)
			reader.now = func() time.Time { return now }
			if err := reader.RecordBattleResult(ctx, credential, 30*time.Second); err == nil {
				t.Fatal("V2 receipt writer accepted non-v2 authority")
			}
			if client.Exists(ctx, dsauthrecord.BattleResultReceiptKey(9)).Val() != 0 {
				t.Fatal("rejected non-v2 authority created result receipt")
			}
			authAfter, _ := client.Get(ctx, battleAuthKey(9)).Bytes()
			battleAfter, _ := client.Get(ctx, battleKey(9)).Bytes()
			if string(authAfter) != string(authRaw) || string(battleAfter) != string(battleRaw) {
				t.Fatal("rejected non-v2 result mutated authority")
			}
		})
	}
}

func TestRedisBattleAuthorityMissingAndBadProto(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = client.Close() }()
	reader := NewRedisBattleAuthReader(client)
	if _, _, found, err := reader.GetBattleAuthority(context.Background(), 9); err != nil || found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if err := client.MSet(context.Background(), battleAuthKey(9), "bad", battleKey(9), "bad").Err(); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := reader.GetBattleAuthority(context.Background(), 9); err == nil {
		t.Fatal("bad proto accepted")
	}
}
