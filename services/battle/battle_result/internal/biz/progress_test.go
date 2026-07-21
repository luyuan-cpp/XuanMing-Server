// progress_test.go — 实时进度通道业务逻辑单测(实时成长,realtime-progression.md)。
//
// 覆盖:校验矩阵 / 换算聚合 / 水位重放 / 已结算拒收 / 出箱发布幂等键 /
// 结算防双发(掉落发放单一权威路径)。
package biz

import (
	"context"
	"testing"

	"github.com/luyuancpp/pandora/pkg/errcode"
	battlev1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/battle/v1"

	"github.com/luyuancpp/pandora/services/battle/battle_result/internal/conf"
	"github.com/luyuancpp/pandora/services/battle/battle_result/internal/data"
)

// progressUsecase 构造带怪物经验表 + 掉落白名单的测试 usecase。
func progressUsecase(repo data.BattleRepo) *BattleResultUsecase {
	cfg := conf.BattleConf{
		EloKFactor: 32, BaseMMR: 1500,
		MonsterExp:    map[uint32]uint64{101: 10, 102: 25},
		DropWhitelist: []uint32{5001, 5002},
	}
	return NewBattleResultUsecase(repo, NewStaticMMRReader(cfg.BaseMMR), &fakePusher{}, nil, cfg)
}

func killEvent(seq, playerID uint64, monsterID, count uint32) *battlev1.BattleProgressEvent {
	return &battlev1.BattleProgressEvent{
		Seq: seq, PlayerId: playerID,
		Fact: &battlev1.BattleProgressEvent_MonsterKill{
			MonsterKill: &battlev1.MonsterKillFact{MonsterConfigId: monsterID, Count: count},
		},
	}
}

func pickupEvent(seq, playerID uint64, itemID, count uint32) *battlev1.BattleProgressEvent {
	return &battlev1.BattleProgressEvent{
		Seq: seq, PlayerId: playerID,
		Fact: &battlev1.BattleProgressEvent_ItemPickup{
			ItemPickup: &battlev1.ItemPickupFact{ItemConfigId: itemID, Count: count},
		},
	}
}

func TestReportProgress_ValidationMatrix(t *testing.T) {
	repo := newFakeRepo()
	uc := progressUsecase(repo)
	ctx := context.Background()
	roster := []uint64{7, 8}

	cases := []struct {
		name   string
		events []*battlev1.BattleProgressEvent
		want   errcode.Code
	}{
		{"empty events", nil, errcode.ErrInvalidArg},
		{"seq zero", []*battlev1.BattleProgressEvent{killEvent(0, 7, 101, 1)}, errcode.ErrInvalidArg},
		{"seq not ascending", []*battlev1.BattleProgressEvent{killEvent(2, 7, 101, 1), killEvent(2, 7, 101, 1)}, errcode.ErrInvalidArg},
		{"seq over cap", []*battlev1.BattleProgressEvent{killEvent(1_000_000, 7, 101, 1)}, errcode.ErrInvalidArg},
		{"player missing", []*battlev1.BattleProgressEvent{killEvent(1, 0, 101, 1)}, errcode.ErrInvalidArg},
		{"player not in roster", []*battlev1.BattleProgressEvent{killEvent(1, 999, 101, 1)}, errcode.ErrUnauthorized},
		{"kill count zero", []*battlev1.BattleProgressEvent{killEvent(1, 7, 101, 0)}, errcode.ErrInvalidArg},
		{"kill count over cap", []*battlev1.BattleProgressEvent{killEvent(1, 7, 101, 101)}, errcode.ErrInvalidArg},
		{"pickup count over cap", []*battlev1.BattleProgressEvent{pickupEvent(1, 7, 5001, 11)}, errcode.ErrInvalidArg},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := uc.ReportProgress(ctx, 900, roster, tc.events); errcode.As(err) != tc.want {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
	if _, err := uc.ReportProgress(ctx, 0, roster, []*battlev1.BattleProgressEvent{killEvent(1, 7, 101, 1)}); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("match_id=0 want ErrInvalidArg, got %v", err)
	}
	// 坏批被拒必须零副作用(水位不动、无出箱行)。
	if seq := repo.progressSeq[900]; seq != 0 {
		t.Fatalf("rejected batches must not advance watermark, got %d", seq)
	}
	if len(repo.progressOutbox) != 0 {
		t.Fatalf("rejected batches must not enqueue outbox, got %d", len(repo.progressOutbox))
	}
}

func TestReportProgress_Disabled(t *testing.T) {
	cfg := conf.BattleConf{ProgressDisabled: true}
	uc := NewBattleResultUsecase(newFakeRepo(), NewStaticMMRReader(1500), &fakePusher{}, nil, cfg)
	_, err := uc.ReportProgress(context.Background(), 900, nil, []*battlev1.BattleProgressEvent{killEvent(1, 7, 101, 1)})
	if errcode.As(err) != errcode.ErrInvalidState {
		t.Fatalf("disabled channel want ErrInvalidState, got %v", err)
	}
}

func TestReportProgress_AggregatesAndAcks(t *testing.T) {
	repo := newFakeRepo()
	uc := progressUsecase(repo)
	ctx := context.Background()
	roster := []uint64{7, 8}

	events := []*battlev1.BattleProgressEvent{
		killEvent(1, 7, 101, 2), // 7: 2×10 = 20 exp
		killEvent(2, 7, 102, 1), // 7: +25 → 45 exp
		killEvent(3, 8, 101, 1), // 8: 10 exp
		pickupEvent(4, 7, 5001, 2),
		pickupEvent(5, 7, 9999, 1), // 非白名单 → 跳过发放,水位照常推进
		killEvent(6, 8, 777, 1),    // 未配置怪物 → 跳过发放
	}
	acked, err := uc.ReportProgress(ctx, 900, roster, events)
	if err != nil || acked != 6 {
		t.Fatalf("acked=%d err=%v, want 6/nil", acked, err)
	}
	if repo.progressSeq[900] != 6 {
		t.Fatalf("watermark=%d want 6", repo.progressSeq[900])
	}
	// 聚合:7 的 exp 一行(45)+ 7 的 item 一行([5001,5001])+ 8 的 exp 一行(10)。
	var exp7, exp8 uint64
	var items7 []uint32
	for _, row := range repo.progressOutbox {
		if row.Seq != 6 {
			t.Fatalf("row seq=%d want batch-end 6", row.Seq)
		}
		switch {
		case row.Kind == data.ProgressGrantExp && row.PlayerID == 7:
			exp7 = row.ExpDelta
		case row.Kind == data.ProgressGrantExp && row.PlayerID == 8:
			exp8 = row.ExpDelta
		case row.Kind == data.ProgressGrantItem && row.PlayerID == 7:
			items7 = row.ItemConfigIDs
		default:
			t.Fatalf("unexpected outbox row %+v", row)
		}
	}
	if exp7 != 45 || exp8 != 10 {
		t.Fatalf("exp7=%d exp8=%d, want 45/10", exp7, exp8)
	}
	if len(items7) != 2 || items7[0] != 5001 || items7[1] != 5001 {
		t.Fatalf("items7=%v, want [5001 5001]", items7)
	}

	// 原批重发(at-least-once)→ 纯重放 ACK,零副作用。
	rows := len(repo.progressOutbox)
	acked2, err := uc.ReportProgress(ctx, 900, roster, events)
	if err != nil || acked2 != 6 {
		t.Fatalf("replay acked=%d err=%v, want 6/nil", acked2, err)
	}
	if len(repo.progressOutbox) != rows {
		t.Fatalf("replay must not enqueue outbox: %d → %d", rows, len(repo.progressOutbox))
	}

	// 续批(部分旧 + 部分新)→ 只入账新事件。
	next := []*battlev1.BattleProgressEvent{
		killEvent(6, 8, 777, 1), // 旧(≤水位)
		killEvent(7, 8, 101, 3), // 新:8 +30 exp
	}
	acked3, err := uc.ReportProgress(ctx, 900, roster, next)
	if err != nil || acked3 != 7 {
		t.Fatalf("next acked=%d err=%v, want 7/nil", acked3, err)
	}
	if len(repo.progressOutbox) != rows+1 {
		t.Fatalf("next batch rows=%d want %d", len(repo.progressOutbox), rows+1)
	}
}

func TestReportProgress_RejectedAfterSettlement(t *testing.T) {
	repo := newFakeRepo()
	uc := progressUsecase(repo)
	ctx := context.Background()
	roster := []uint64{1, 2}

	if _, err := uc.ReportProgress(ctx, 901, roster, []*battlev1.BattleProgressEvent{killEvent(1, 1, 101, 1)}); err != nil {
		t.Fatalf("progress before settle: %v", err)
	}
	// 结算(fakeRepo SaveResult 打终局标记)。
	result := &battlev1.BattleResult{
		MatchId: 901, WinnerTeam: 0,
		Stats: []*battlev1.PlayerStats{{PlayerId: 1, Team: 0}, {PlayerId: 2, Team: 1}},
	}
	if _, err := uc.ReportResult(ctx, result, 1); err != nil {
		t.Fatalf("settle: %v", err)
	}
	// 迟到进度(僵尸 / 分区恢复 DS)一律拒:ErrInvalidState → DS 停流。
	if _, err := uc.ReportProgress(ctx, 901, roster, []*battlev1.BattleProgressEvent{killEvent(2, 1, 101, 1)}); errcode.As(err) != errcode.ErrInvalidState {
		t.Fatalf("post-settle progress want ErrInvalidState, got %v", err)
	}
}

func TestReportResult_SuppressesDropsWhenProgressStreamed(t *testing.T) {
	repo := newFakeRepo()
	uc := progressUsecase(repo)
	ctx := context.Background()
	roster := []uint64{1, 2}

	// 本场走过实时通道(水位 >0)。
	if _, err := uc.ReportProgress(ctx, 902, roster, []*battlev1.BattleProgressEvent{pickupEvent(1, 1, 5001, 1)}); err != nil {
		t.Fatalf("progress: %v", err)
	}
	// 结算再报同一掉落(恶意 / 兼容字段)→ 必须被抑制,不得双发。
	result := &battlev1.BattleResult{
		MatchId: 902, WinnerTeam: 0,
		Stats: []*battlev1.PlayerStats{
			{PlayerId: 1, Team: 0, DroppedItemConfigIds: []uint32{5001}},
			{PlayerId: 2, Team: 1},
		},
	}
	if _, err := uc.ReportResult(ctx, result, 1); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if len(repo.dropOutbox) != 0 {
		t.Fatalf("drops must be suppressed when progress streamed, got %d rows", len(repo.dropOutbox))
	}
	// 对照:未走实时通道的场,结算掉落照常发放。
	legacy := &battlev1.BattleResult{
		MatchId: 903, WinnerTeam: 0,
		Stats: []*battlev1.PlayerStats{{PlayerId: 1, Team: 0, DroppedItemConfigIds: []uint32{5001}}},
	}
	if _, err := uc.ReportResult(ctx, legacy, 0); err != nil {
		t.Fatalf("legacy settle: %v", err)
	}
	if len(repo.dropOutbox) != 1 {
		t.Fatalf("legacy drops must still flow, got %d rows", len(repo.dropOutbox))
	}
}

// fakeExpGranter 捕获 AddExperience 调用;failFirst 模拟 player 瞬时不可用。
type fakeExpGranter struct {
	calls     []grantCall
	failFirst bool
}

func (g *fakeExpGranter) AddExperience(_ context.Context, playerID uint64, expDelta uint64, _ string, key string) error {
	if g.failFirst {
		g.failFirst = false
		return simpleErr("player down")
	}
	g.calls = append(g.calls, grantCall{playerID: playerID, items: []uint32{uint32(expDelta)}, key: key})
	return nil
}

func TestPublishProgressBatch(t *testing.T) {
	repo := newFakeRepo()
	uc := progressUsecase(repo)
	ctx := context.Background()
	roster := []uint64{7}

	events := []*battlev1.BattleProgressEvent{
		killEvent(1, 7, 101, 1),    // 10 exp
		pickupEvent(2, 7, 5002, 1), // 一件 5002
	}
	if _, err := uc.ReportProgress(ctx, 910, roster, events); err != nil {
		t.Fatalf("progress: %v", err)
	}

	expG := &fakeExpGranter{failFirst: true}
	itemG := &fakeGranter{}
	uc.SetExperienceGranter(expG)
	uc.SetInstanceGranter(itemG)

	// 第一轮:exp 行失败保留,item 行照发(单行失败不阻塞)。
	if n, err := uc.publishProgressBatch(ctx); err != nil || n != 1 {
		t.Fatalf("round1 n=%d err=%v, want 1/nil", n, err)
	}
	if len(repo.progressOutbox) != 1 {
		t.Fatalf("failed exp row must remain, got %d rows", len(repo.progressOutbox))
	}
	// 第二轮:exp 行补发成功。
	if n, err := uc.publishProgressBatch(ctx); err != nil || n != 1 {
		t.Fatalf("round2 n=%d err=%v, want 1/nil", n, err)
	}
	if len(repo.progressOutbox) != 0 {
		t.Fatalf("all rows must be delivered, got %d", len(repo.progressOutbox))
	}
	if len(expG.calls) != 1 || expG.calls[0].playerID != 7 || expG.calls[0].key != "progress:910:2:7:exp" {
		t.Fatalf("exp grant calls wrong: %+v", expG.calls)
	}
	if len(itemG.calls) != 1 || itemG.calls[0].key != "progress:910:2:7:item" || len(itemG.calls[0].items) != 1 {
		t.Fatalf("item grant calls wrong: %+v", itemG.calls)
	}
}

func TestPublishProgressBatch_CapacityFullGoesToMail(t *testing.T) {
	repo := newFakeRepo()
	uc := progressUsecase(repo)
	ctx := context.Background()

	if _, err := uc.ReportProgress(ctx, 911, []uint64{7}, []*battlev1.BattleProgressEvent{pickupEvent(1, 7, 5001, 1)}); err != nil {
		t.Fatalf("progress: %v", err)
	}
	itemG := &fakeGranter{capacityFull: true}
	mail := &fakeMailSender{}
	uc.SetInstanceGranter(itemG)
	uc.SetMailSender(mail)

	if n, err := uc.publishProgressBatch(ctx); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v, want 1/nil", n, err)
	}
	if len(mail.calls) != 1 || mail.calls[0].key != "progress:911:1:7:item" {
		t.Fatalf("overflow mail calls wrong: %+v", mail.calls)
	}
	if len(repo.progressOutbox) != 0 {
		t.Fatalf("mailed row must be deleted, got %d", len(repo.progressOutbox))
	}
}

func TestPublishProgressBatch_NilGrantersKeepRows(t *testing.T) {
	repo := newFakeRepo()
	uc := progressUsecase(repo)
	ctx := context.Background()

	if _, err := uc.ReportProgress(ctx, 912, []uint64{7}, []*battlev1.BattleProgressEvent{killEvent(1, 7, 101, 1)}); err != nil {
		t.Fatalf("progress: %v", err)
	}
	// granter 全未注入:行原样积压不丢。
	if n, err := uc.publishProgressBatch(ctx); err != nil || n != 0 {
		t.Fatalf("n=%d err=%v, want 0/nil", n, err)
	}
	if len(repo.progressOutbox) != 1 {
		t.Fatalf("rows must stay when granters missing, got %d", len(repo.progressOutbox))
	}
}
