// board_store 的 Redis ZSET 排行榜单测(miniredis,2026-06-27)。
//
// 覆盖:SET_IF_HIGHER / SET / INCREMENT 三种上报模式、降序 / 升序排名、max_size 截断、
// 时间 tie-break(同分先达者名次高)、Around 邻居、Remove / Delete / Clear、GetMeta。
package data

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestStore 起 miniredis 并返回 RedisBoardStore。
func newTestStore(t *testing.T) (*RedisBoardStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedisBoardStore(rdb), mr
}

var testBoard = BoardKey{BoardType: 1, Scope: ScopeGlobal, ScopeID: 0, Period: "2026W26"}

// descOpt 降序榜(高分高名次,默认大多数榜)。
func descOpt() Options { return Options{Ascending: false} }

// ── 上报模式 ──────────────────────────────────────────────────────────────────

func TestSubmit_SetIfHigher_KeepsBest(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if _, _, err := s.Submit(ctx, testBoard, 100, 50, ModeSetIfHigher, descOpt(), 1000); err != nil {
		t.Fatalf("submit 50: %v", err)
	}
	// 更高分 → 覆盖
	got, rank, err := s.Submit(ctx, testBoard, 100, 80, ModeSetIfHigher, descOpt(), 2000)
	if err != nil {
		t.Fatalf("submit 80: %v", err)
	}
	if got != 80 || rank != 1 {
		t.Fatalf("after 80: score=%d rank=%d, want 80/1", got, rank)
	}
	// 更低分 → 不降级,仍 80
	got, _, err = s.Submit(ctx, testBoard, 100, 30, ModeSetIfHigher, descOpt(), 3000)
	if err != nil {
		t.Fatalf("submit 30: %v", err)
	}
	if got != 80 {
		t.Fatalf("after lower 30: score=%d, want 80 (no downgrade)", got)
	}
}

func TestSubmit_SetIfHigher_Ascending(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	b := BoardKey{BoardType: 2, Scope: ScopeInstance, ScopeID: 7, Period: "-"}
	asc := Options{Ascending: true} // 升序榜:小分更优(如竞速用时)

	if _, _, err := s.Submit(ctx, b, 1, 5000, ModeSetIfHigher, asc, 1000); err != nil {
		t.Fatalf("submit 5000: %v", err)
	}
	// 升序榜「更优」= 更小;3000 < 5000 → 覆盖
	got, _, err := s.Submit(ctx, b, 1, 3000, ModeSetIfHigher, asc, 2000)
	if err != nil {
		t.Fatalf("submit 3000: %v", err)
	}
	if got != 3000 {
		t.Fatalf("after better(smaller) 3000: score=%d, want 3000", got)
	}
	// 更大(更差)→ 不更新
	got, _, _ = s.Submit(ctx, b, 1, 9000, ModeSetIfHigher, asc, 3000)
	if got != 3000 {
		t.Fatalf("after worse 9000: score=%d, want 3000", got)
	}
}

func TestSubmit_Increment(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if _, _, err := s.Submit(ctx, testBoard, 100, 10, ModeIncrement, descOpt(), 1000); err != nil {
		t.Fatalf("inc 10: %v", err)
	}
	got, _, err := s.Submit(ctx, testBoard, 100, 15, ModeIncrement, descOpt(), 2000)
	if err != nil {
		t.Fatalf("inc 15: %v", err)
	}
	if got != 25 {
		t.Fatalf("after inc: score=%d, want 25", got)
	}
}

func TestSubmit_Set_Overwrites(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	_, _, _ = s.Submit(ctx, testBoard, 100, 80, ModeSet, descOpt(), 1000)
	got, _, err := s.Submit(ctx, testBoard, 100, 40, ModeSet, descOpt(), 2000)
	if err != nil {
		t.Fatalf("set 40: %v", err)
	}
	if got != 40 {
		t.Fatalf("SET should overwrite even lower: score=%d, want 40", got)
	}
}

// ── 排名 / 区间 ───────────────────────────────────────────────────────────────

func TestRange_Descending(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	_, _, _ = s.Submit(ctx, testBoard, 1, 30, ModeSet, descOpt(), 1000)
	_, _, _ = s.Submit(ctx, testBoard, 2, 90, ModeSet, descOpt(), 1000)
	_, _, _ = s.Submit(ctx, testBoard, 3, 60, ModeSet, descOpt(), 1000)

	got, err := s.Range(ctx, testBoard, 0, 10, false)
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	wantIDs := []uint64{2, 3, 1}
	wantScores := []int64{90, 60, 30}
	if len(got) != 3 {
		t.Fatalf("range len=%d, want 3", len(got))
	}
	for i, e := range got {
		if e.EntityID != wantIDs[i] || e.Score != wantScores[i] || e.Rank != int64(i+1) {
			t.Fatalf("rank %d: id=%d score=%d rank=%d, want id=%d score=%d rank=%d",
				i, e.EntityID, e.Score, e.Rank, wantIDs[i], wantScores[i], i+1)
		}
	}
}

func TestRank_NotFound(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	_, found, err := s.Rank(ctx, testBoard, 999, false)
	if err != nil {
		t.Fatalf("rank: %v", err)
	}
	if found {
		t.Fatalf("found=true for absent entity, want false")
	}
}

func TestRank_Found(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	_, _, _ = s.Submit(ctx, testBoard, 1, 30, ModeSet, descOpt(), 1000)
	_, _, _ = s.Submit(ctx, testBoard, 2, 90, ModeSet, descOpt(), 1000)

	e, found, err := s.Rank(ctx, testBoard, 1, false)
	if err != nil || !found {
		t.Fatalf("rank id=1: found=%v err=%v", found, err)
	}
	if e.Score != 30 || e.Rank != 2 {
		t.Fatalf("id=1: score=%d rank=%d, want 30/2", e.Score, e.Rank)
	}
}

// ── max_size 截断 ────────────────────────────────────────────────────────────

func TestSubmit_MaxSize_TruncatesDescending(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	b := BoardKey{BoardType: 3, Scope: ScopeCustom, ScopeID: 1, Period: "-"}
	opt := Options{Ascending: false, MaxSize: 2} // 只保留 Top-2

	_, _, _ = s.Submit(ctx, b, 1, 10, ModeSet, opt, 1000)
	_, _, _ = s.Submit(ctx, b, 2, 50, ModeSet, opt, 1000)
	_, _, _ = s.Submit(ctx, b, 3, 30, ModeSet, opt, 1000) // 触发截断,挤出最低分(10,id=1)

	total, err := s.Total(ctx, b)
	if err != nil {
		t.Fatalf("total: %v", err)
	}
	if total != 2 {
		t.Fatalf("total=%d, want 2 (truncated)", total)
	}
	if _, found, _ := s.Rank(ctx, b, 1, false); found {
		t.Fatalf("id=1 (lowest) should be truncated out")
	}
	if _, found, _ := s.Rank(ctx, b, 2, false); !found {
		t.Fatalf("id=2 (highest) should remain")
	}
}

// ── 时间 tie-break ───────────────────────────────────────────────────────────

func TestSubmit_TieBreakByTime_EarlierRanksHigher(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	b := BoardKey{BoardType: 4, Scope: ScopeGlobal, ScopeID: 0, Period: "-"}
	opt := Options{Ascending: false, TieBreakByTime: true}

	// 同分 50,id=1 先达(ts 小),id=2 后达(ts 大)。降序 + tie:先达名次更高。
	tsEarly := lbEpochMs + 1_000_000
	tsLate := lbEpochMs + 2_000_000
	_, _, _ = s.Submit(ctx, b, 1, 50, ModeSet, opt, tsEarly)
	_, _, _ = s.Submit(ctx, b, 2, 50, ModeSet, opt, tsLate)

	got, err := s.Range(ctx, b, 0, 10, false)
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2", len(got))
	}
	if got[0].EntityID != 1 || got[1].EntityID != 2 {
		t.Fatalf("tie order = [%d,%d], want [1,2] (earlier first)", got[0].EntityID, got[1].EntityID)
	}
	// 真实分仍还原为 50(打包的时间项被 round 抹掉)
	if got[0].Score != 50 || got[1].Score != 50 {
		t.Fatalf("scores = [%d,%d], want [50,50]", got[0].Score, got[1].Score)
	}
}

// ── Around ───────────────────────────────────────────────────────────────────

func TestAround(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	b := BoardKey{BoardType: 5, Scope: ScopeGlobal, ScopeID: 0, Period: "-"}
	// 分数 10..100,id=1..10(降序后:id10 第1 … id1 第10)
	for i := uint64(1); i <= 10; i++ {
		_, _, _ = s.Submit(ctx, b, i, int64(i*10), ModeSet, descOpt(), 1000)
	}
	// 取 id=5(降序第6名)上下各 1 名 → id4,id5,id6 对应名次 7,6,5 → 顺序应是 id6,id5,id4
	got, found, err := s.Around(ctx, b, 5, 1, false)
	if err != nil || !found {
		t.Fatalf("around id=5: found=%v err=%v", found, err)
	}
	wantIDs := []uint64{6, 5, 4}
	if len(got) != 3 {
		t.Fatalf("around len=%d, want 3", len(got))
	}
	for i, e := range got {
		if e.EntityID != wantIDs[i] {
			t.Fatalf("around[%d] id=%d, want %d", i, e.EntityID, wantIDs[i])
		}
	}
}

// ── Remove / Delete / Clear / GetMeta ────────────────────────────────────────

func TestRemove(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	_, _, _ = s.Submit(ctx, testBoard, 1, 30, ModeSet, descOpt(), 1000)
	if err := s.Remove(ctx, testBoard, 1); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, found, _ := s.Rank(ctx, testBoard, 1, false); found {
		t.Fatalf("id=1 still found after remove")
	}
}

func TestDelete(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	_, _, _ = s.Submit(ctx, testBoard, 1, 30, ModeSet, descOpt(), 1000)
	if err := s.Delete(ctx, testBoard); err != nil {
		t.Fatalf("delete: %v", err)
	}
	total, _ := s.Total(ctx, testBoard)
	if total != 0 {
		t.Fatalf("total=%d after delete, want 0", total)
	}
	if _, _, exists, _ := s.GetMeta(ctx, testBoard); exists {
		t.Fatalf("meta should be gone after delete")
	}
}

func TestClear_KeepsMeta(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	opt := Options{Ascending: false, TieBreakByTime: true}
	_, _, _ = s.Submit(ctx, testBoard, 1, 30, ModeSet, opt, 1000)
	if err := s.Clear(ctx, testBoard); err != nil {
		t.Fatalf("clear: %v", err)
	}
	total, _ := s.Total(ctx, testBoard)
	if total != 0 {
		t.Fatalf("total=%d after clear, want 0", total)
	}
	// Clear 保留 meta(周期 reset 延续榜配置)
	asc, tie, exists, _ := s.GetMeta(ctx, testBoard)
	if !exists || asc || !tie {
		t.Fatalf("meta after clear: asc=%v tie=%v exists=%v, want false/true/true", asc, tie, exists)
	}
}

func TestGetMeta_AfterFirstSubmit(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	opt := Options{Ascending: true, TieBreakByTime: true}
	_, _, _ = s.Submit(ctx, testBoard, 1, 30, ModeSet, opt, 1000)

	asc, tie, exists, err := s.GetMeta(ctx, testBoard)
	if err != nil {
		t.Fatalf("getmeta: %v", err)
	}
	if !exists || !asc || !tie {
		t.Fatalf("meta = asc:%v tie:%v exists:%v, want true/true/true", asc, tie, exists)
	}
}

// ── TTL ──────────────────────────────────────────────────────────────────────

func TestSubmit_SetsTTL(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()
	b := BoardKey{BoardType: 6, Scope: ScopeInstance, ScopeID: 99, Period: "-"}
	opt := Options{Ascending: false, TTLSeconds: 60} // 临时榜

	_, _, _ = s.Submit(ctx, b, 1, 30, ModeSet, opt, 1000)
	if ttl := mr.TTL(b.zKey()); ttl <= 0 {
		t.Fatalf("zkey TTL=%v, want >0 (temporary board)", ttl)
	}
	// 全员分 / 直方图同样跟随临时榜 TTL 自清
	if ttl := mr.TTL(b.sKey()); ttl <= 0 {
		t.Fatalf("skey TTL=%v, want >0 (temporary board)", ttl)
	}
	if ttl := mr.TTL(b.hKey()); ttl <= 0 {
		t.Fatalf("hkey TTL=%v, want >0 (temporary board)", ttl)
	}
}

// ── 截断后分数语义(全员分 :s)────────────────────────────────────────────────

// 截断出榜后 INCREMENT 必须在原累计分上继续累加,不得从 0 重算。
func TestSubmit_Increment_PreservedAfterTruncation(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	b := BoardKey{BoardType: 7, Scope: ScopeGlobal, ScopeID: 0, Period: "-"}
	opt := Options{Ascending: false, MaxSize: 1, EstimateBucketWidth: 10}

	_, _, _ = s.Submit(ctx, b, 1, 10, ModeIncrement, opt, 1000)  // id1=10,在榜
	_, _, _ = s.Submit(ctx, b, 2, 100, ModeIncrement, opt, 2000) // id2=100,把 id1 挤出榜
	got, rank, err := s.Submit(ctx, b, 1, 5, ModeIncrement, opt, 3000)
	if err != nil {
		t.Fatalf("inc after truncation: %v", err)
	}
	if got != 15 {
		t.Fatalf("score=%d after truncated increment, want 15 (10+5, not reset)", got)
	}
	if rank != 0 {
		t.Fatalf("rank=%d, want 0 (still off board)", rank)
	}
}

// 截断出榜后 SET_IF_HIGHER 不得放进更低分。
func TestSubmit_SetIfHigher_NoDowngradeAfterTruncation(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	b := BoardKey{BoardType: 8, Scope: ScopeGlobal, ScopeID: 0, Period: "-"}
	opt := Options{Ascending: false, MaxSize: 1, EstimateBucketWidth: 10}

	_, _, _ = s.Submit(ctx, b, 1, 50, ModeSetIfHigher, opt, 1000)
	_, _, _ = s.Submit(ctx, b, 2, 60, ModeSetIfHigher, opt, 2000) // id1 被挤出
	got, _, err := s.Submit(ctx, b, 1, 40, ModeSetIfHigher, opt, 3000)
	if err != nil {
		t.Fatalf("set_if_higher after truncation: %v", err)
	}
	if got != 50 {
		t.Fatalf("score=%d after lower submit, want 50 (no downgrade off board)", got)
	}
}

// ── 榜外区间估算 ─────────────────────────────────────────────────────────────

// 降序榜:10 人上分,精确榜只留 Top-3,榜外玩家的估算名次应接近真实名次。
func TestEstimate_TruncatedDescending(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	b := BoardKey{BoardType: 9, Scope: ScopeGlobal, ScopeID: 0, Period: "-"}
	opt := Options{Ascending: false, MaxSize: 3, EstimateBucketWidth: 10}

	for i := uint64(1); i <= 10; i++ {
		_, _, _ = s.Submit(ctx, b, i, int64(i*10), ModeSet, opt, 1000) // id1=10 … id10=100
	}
	// id5(=50,真实第 6 名)已被截断:精确查不到
	if _, found, _ := s.Rank(ctx, b, 5, false); found {
		t.Fatalf("id=5 should be truncated out of precise board")
	}
	e, total, found, err := s.Estimate(ctx, b, 5, false)
	if err != nil || !found {
		t.Fatalf("estimate id=5: found=%v err=%v", found, err)
	}
	if e.Score != 50 {
		t.Fatalf("estimate score=%d, want 50", e.Score)
	}
	if total != 10 {
		t.Fatalf("total=%d, want 10", total)
	}
	// 桶宽 10,每人一桶:better=5(60..100)+ 本桶一半 → 恰为真实名次 6
	if e.Rank != 6 {
		t.Fatalf("estimate rank=%d, want 6", e.Rank)
	}
}

// 升序榜(分低者优)的估算方向。
func TestEstimate_TruncatedAscending(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	b := BoardKey{BoardType: 10, Scope: ScopeGlobal, ScopeID: 0, Period: "-"}
	opt := Options{Ascending: true, MaxSize: 2, EstimateBucketWidth: 10}

	for i := uint64(1); i <= 10; i++ {
		_, _, _ = s.Submit(ctx, b, i, int64(i*10), ModeSet, opt, 1000) // 优→劣:id1=10 … id10=100
	}
	e, total, found, err := s.Estimate(ctx, b, 5, true) // id5=50,真实第 5 名
	if err != nil || !found {
		t.Fatalf("estimate asc id=5: found=%v err=%v", found, err)
	}
	if total != 10 || e.Rank != 5 {
		t.Fatalf("estimate asc rank=%d total=%d, want 5/10", e.Rank, total)
	}
}

// 估算名次不得落进精确榜区间(钳制 ZCARD+1)。
func TestEstimate_ClampedBelowPreciseBoard(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	b := BoardKey{BoardType: 11, Scope: ScopeGlobal, ScopeID: 0, Period: "-"}
	// 桶宽极大:所有人同桶 → 直方图无法区分,估算=本桶一半,可能小于榜内人数 → 必须被钳制
	opt := Options{Ascending: false, MaxSize: 3, EstimateBucketWidth: 1000000}

	for i := uint64(1); i <= 5; i++ {
		_, _, _ = s.Submit(ctx, b, i, int64(i*10), ModeSet, opt, 1000)
	}
	e, _, found, err := s.Estimate(ctx, b, 1, false) // id1 最低分,被截断
	if err != nil || !found {
		t.Fatalf("estimate id=1: found=%v err=%v", found, err)
	}
	if e.Rank < 4 {
		t.Fatalf("estimate rank=%d, want >= 4 (clamped after precise top-3)", e.Rank)
	}
}

// 从未上报的 entity:估算也查不到。
func TestEstimate_NeverSubmitted(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	_, _, _ = s.Submit(ctx, testBoard, 1, 30, ModeSet, descOpt(), 1000)
	_, _, found, err := s.Estimate(ctx, testBoard, 999, false)
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if found {
		t.Fatalf("found=true for never-submitted entity, want false")
	}
}

// Remove 必须同步回扣直方图与全员分。
func TestRemove_UpdatesEstimateState(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	b := BoardKey{BoardType: 12, Scope: ScopeGlobal, ScopeID: 0, Period: "-"}
	opt := Options{Ascending: false, MaxSize: 1, EstimateBucketWidth: 10}

	_, _, _ = s.Submit(ctx, b, 1, 10, ModeSet, opt, 1000)
	_, _, _ = s.Submit(ctx, b, 2, 20, ModeSet, opt, 1000)
	_, _, _ = s.Submit(ctx, b, 3, 30, ModeSet, opt, 1000) // 榜内只剩 id3

	if err := s.Remove(ctx, b, 1); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, _, found, _ := s.Estimate(ctx, b, 1, false); found {
		t.Fatalf("removed entity still estimable")
	}
	_, total, found, err := s.Estimate(ctx, b, 2, false)
	if err != nil || !found {
		t.Fatalf("estimate id=2: found=%v err=%v", found, err)
	}
	if total != 2 {
		t.Fatalf("total=%d after remove, want 2", total)
	}
}

// Clear(周期 reset)清空全员分与直方图。
func TestClear_ClearsEstimateState(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	opt := Options{Ascending: false, MaxSize: 1, EstimateBucketWidth: 10}
	_, _, _ = s.Submit(ctx, testBoard, 1, 10, ModeSet, opt, 1000)
	_, _, _ = s.Submit(ctx, testBoard, 2, 20, ModeSet, opt, 1000) // id1 出榜但仍可估算

	if err := s.Clear(ctx, testBoard); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, _, found, _ := s.Estimate(ctx, testBoard, 1, false); found {
		t.Fatalf("estimate state should be cleared with board reset")
	}
}

// 桶宽建榜定死:后续上报换宽度不生效(防直方图桶宽混用)。
func TestSubmit_BucketWidthImmutable(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()
	b := BoardKey{BoardType: 13, Scope: ScopeGlobal, ScopeID: 0, Period: "-"}

	_, _, _ = s.Submit(ctx, b, 1, 10, ModeSet, Options{EstimateBucketWidth: 10}, 1000)
	_, _, _ = s.Submit(ctx, b, 2, 20, ModeSet, Options{EstimateBucketWidth: 999}, 1000)
	bw := mr.HGet(b.mKey(), "bw")
	if bw != "10" {
		t.Fatalf("meta bw=%q, want \"10\" (immutable after board creation)", bw)
	}
}

// 升级兼容:本改动前建的榜(:s/:h 为空)成员再上报时,旧分从 ZSET 回退补记,不丢累计。
func TestSubmit_LegacyBoardBackfill(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()
	b := BoardKey{BoardType: 14, Scope: ScopeGlobal, ScopeID: 0, Period: "-"}
	opt := Options{Ascending: false, EstimateBucketWidth: 10}

	_, _, _ = s.Submit(ctx, b, 1, 50, ModeSetIfHigher, opt, 1000)
	// 模拟旧版本产生的数据:删掉新结构,只留 z/t/m(m 里去掉 bw)
	mr.Del(b.sKey())
	mr.Del(b.hKey())
	mr.HDel(b.mKey(), "bw")

	// 再上报更低分:SET_IF_HIGHER 不写 z,但旧分应从 z 回退补进 :s / :h
	got, _, err := s.Submit(ctx, b, 1, 40, ModeSetIfHigher, opt, 2000)
	if err != nil {
		t.Fatalf("legacy resubmit: %v", err)
	}
	if got != 50 {
		t.Fatalf("score=%d, want 50 (kept from legacy zset)", got)
	}
	if v := mr.HGet(b.sKey(), "1"); v != "50" {
		t.Fatalf("skey backfill=%q, want \"50\"", v)
	}
	_, total, found, err := s.Estimate(ctx, b, 1, false)
	if err != nil || !found {
		t.Fatalf("estimate after backfill: found=%v err=%v", found, err)
	}
	if total != 1 {
		t.Fatalf("total=%d after backfill, want 1", total)
	}
}
