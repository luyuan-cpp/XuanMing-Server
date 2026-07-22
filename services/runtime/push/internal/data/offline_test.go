// RedisOfflineCacheRepo 单测(2026-07-22 cursor 权威 + always-cache 重写;
// R4 修订:游标基线改服务端 now,不信任帧原始 kafka ts)。
//
// 用 miniredis 跑纯内存 redis,验证:
//   - AssignAndBuffer:游标每玩家严格递增且唯一(kafka 原始 ts 相同/回拨/远旧/远未来
//     都不影响),帧 ts_ms 被重铸为游标;
//   - **刚写入的帧必然存活**(R4 P1-1 回归:旧实现拿过期 kafka ts 当游标,同一 Lua
//     的窗口修剪会删掉刚写入的帧后 ack = 静默丢帧);
//   - Range 严格 > 且应用保留窗下界(R4 P1-2:静默玩家未被写侧修剪的旧帧不得投递);
//   - 双修剪:TTL 窗口外旧帧删除 + maxFrames 条数硬上限(§9.18);
//   - TTL 刷新(连续写重置过期时间)。
//
// 注意:游标 = max(基线+1, 服务端 now),窗口修剪/读过滤用墙钟 now-ttl 做 cutoff,
// 涉及窗口的用例用短 ttl + 真实 sleep 构造过期,不再伪造旧 ts。
package data

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	pushv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/push/v1"
)

func newTestRepo(t *testing.T, ttl time.Duration, maxFrames int) (*RedisOfflineCacheRepo, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedisOfflineCacheRepo(rdb, ttl, maxFrames), mr
}

func mustFrame(topic string, tsMs int64, payload string) *pushv1.PushFrame {
	return &pushv1.PushFrame{
		Topic:   topic,
		Payload: []byte(payload),
		TsMs:    tsMs,
		TraceId: "trace-" + topic,
	}
}

// buffer 写入一帧并返回分配的游标。
func buffer(t *testing.T, repo *RedisOfflineCacheRepo, player uint64, tsMs int64, payload string) int64 {
	t.Helper()
	cursor, err := repo.AssignAndBuffer(context.Background(), player, mustFrame("pandora.team.update", tsMs, payload))
	if err != nil {
		t.Fatalf("AssignAndBuffer(%s) err=%v", payload, err)
	}
	return cursor
}

// 用例 1:写入 → Range 闭环;游标严格递增;帧 ts_ms == 游标。
func TestOfflineBuffer_RoundTripCursorMonotonic(t *testing.T) {
	repo, _ := newTestRepo(t, time.Minute, 0)
	ctx := context.Background()
	base := time.Now().UnixMilli()

	c1 := buffer(t, repo, 100, base, "a")
	c2 := buffer(t, repo, 100, base, "b")     // 原始 ts 相同 → 游标仍须 +1
	c3 := buffer(t, repo, 100, base-500, "c") // 原始 ts 回拨 → 游标仍须递增
	if !(c1 < c2 && c2 < c3) {
		t.Fatalf("cursors must strictly increase: %d %d %d", c1, c2, c3)
	}

	got, err := repo.Range(ctx, 100, 0)
	if err != nil {
		t.Fatalf("Range err=%v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len=%d want=3", len(got))
	}
	for i, want := range []int64{c1, c2, c3} {
		if got[i].ScoreMs != want || got[i].Frame.GetTsMs() != want {
			t.Fatalf("i=%d score=%d frame.ts=%d want=%d(帧 ts 必须重铸为游标)",
				i, got[i].ScoreMs, got[i].Frame.GetTsMs(), want)
		}
	}
}

// 用例 2:Range 严格 >(断点续传不重不漏):以第 N 帧游标续传恰好得后续帧。
func TestOfflineBuffer_RangeStrictlyAfter(t *testing.T) {
	repo, _ := newTestRepo(t, time.Minute, 0)
	ctx := context.Background()
	base := time.Now().UnixMilli()

	c1 := buffer(t, repo, 200, base, "a")
	c2 := buffer(t, repo, 200, base, "b")
	c3 := buffer(t, repo, 200, base, "c")

	got, err := repo.Range(ctx, 200, c1)
	if err != nil {
		t.Fatalf("Range err=%v", err)
	}
	if len(got) != 2 || got[0].ScoreMs != c2 || got[1].ScoreMs != c3 {
		t.Fatalf("resume after %d: got=%+v want=[%d %d]", c1, got, c2, c3)
	}
	if got2, _ := repo.Range(ctx, 200, c3); len(got2) != 0 {
		t.Fatalf("resume after last cursor must be empty, got=%d", len(got2))
	}
}

// 用例 3:迟到帧闭环(审计 P1 核心场景):玩家已推进到游标 C,一条原始 ts << C 的
// 迟到帧写入后游标 > C,客户端按 C 续传必可见。
func TestOfflineBuffer_LateFrameStaysVisible(t *testing.T) {
	repo, _ := newTestRepo(t, time.Minute, 0)
	ctx := context.Background()
	base := time.Now().UnixMilli()

	cNew := buffer(t, repo, 300, base+2000, "first") // 客户端已见,游标推进到 base+2000
	cLate := buffer(t, repo, 300, base+1900, "late") // 跨 producer 迟到帧
	if cLate <= cNew {
		t.Fatalf("late frame cursor=%d must exceed prior cursor=%d", cLate, cNew)
	}
	got, err := repo.Range(ctx, 300, cNew)
	if err != nil || len(got) != 1 || string(got[0].Frame.GetPayload()) != "late" {
		t.Fatalf("late frame must be visible after client cursor: got=%+v err=%v", got, err)
	}
}

// 用例 4:TTL 每次写入刷新。
func TestOfflineBuffer_TTLRefreshOnWrite(t *testing.T) {
	repo, mr := newTestRepo(t, time.Hour, 0)
	ctx := context.Background()
	base := time.Now().UnixMilli()

	buffer(t, repo, 400, base, "p1")
	mr.FastForward(30 * time.Minute)
	buffer(t, repo, 400, base+1, "p2")
	mr.FastForward(40 * time.Minute) // 未刷新则首帧 1h 已到期

	got, err := repo.Range(ctx, 400, 0)
	if err != nil {
		t.Fatalf("Range err=%v", err)
	}
	if len(got) != 2 {
		t.Fatalf("after TTL refresh len=%d want=2", len(got))
	}
}

// 用例 5:TTL 窗口修剪:整 key TTL 被持续写入刷新时,窗口外旧 member 必须被删。
// 用短 ttl + 真实 sleep 构造过期(游标 = 服务端 now,无法再靠伪造旧 ts 造窗口外帧)。
func TestOfflineBuffer_TrimsExpiredMembers(t *testing.T) {
	repo, _ := newTestRepo(t, 50*time.Millisecond, 0)
	ctx := context.Background()

	buffer(t, repo, 500, 0, "old")
	time.Sleep(80 * time.Millisecond) // 首帧滑出 50ms 窗口
	buffer(t, repo, 500, 0, "fresh")  // 写侧修剪应删掉 old

	got, err := repo.Range(ctx, 500, 0)
	if err != nil {
		t.Fatalf("Range err=%v", err)
	}
	if len(got) != 1 || string(got[0].Frame.GetPayload()) != "fresh" {
		t.Fatalf("got=%+v want single fresh frame (stale member must be trimmed)", got)
	}
}

// 用例 5b(R4 P1-1 回归,修复前失败):原始 kafka ts 远早于保留窗的帧(重投/积压),
// 写入后必须立即可见——旧实现拿它当游标,同一 Lua 的窗口修剪会把刚写入的帧连同
// 哨兵一起删掉,随后 ack = 静默丢帧。
func TestOfflineBuffer_StaleKafkaTsFrameSurvivesOwnWrite(t *testing.T) {
	repo, _ := newTestRepo(t, time.Minute, 0)
	ctx := context.Background()
	staleTs := time.Now().Add(-2 * time.Minute).UnixMilli() // 窗口(1min)之外的原始 ts

	cursor := buffer(t, repo, 510, staleTs, "redelivered")
	if now := time.Now().UnixMilli(); cursor < now-time.Minute.Milliseconds() {
		t.Fatalf("cursor=%d must be based on server now, not stale kafka ts=%d", cursor, staleTs)
	}
	got, err := repo.Range(ctx, 510, 0)
	if err != nil || len(got) != 1 || string(got[0].Frame.GetPayload()) != "redelivered" {
		t.Fatalf("stale-ts frame must survive its own write (write-then-delete bug): got=%+v err=%v", got, err)
	}
}

// 用例 5c(R4 P1-2):Range 应用保留窗下界——静默玩家(无新写触发写侧修剪)key 里
// 残留的窗口外旧帧不得投递;窗口内帧正常返回。
func TestOfflineBuffer_RangeFiltersOutsideRetentionWindow(t *testing.T) {
	repo, _ := newTestRepo(t, 50*time.Millisecond, 0)
	ctx := context.Background()

	buffer(t, repo, 520, 0, "aged")
	time.Sleep(80 * time.Millisecond) // 滑出窗口;无后续写,写侧修剪不触发
	got, err := repo.Range(ctx, 520, 0)
	if err != nil {
		t.Fatalf("Range err=%v", err)
	}
	if len(got) != 0 {
		t.Fatalf("frames outside retention window must not be delivered, got=%+v", got)
	}

	fresh := buffer(t, repo, 520, 0, "fresh")
	got, err = repo.Range(ctx, 520, 0)
	if err != nil || len(got) != 1 || got[0].ScoreMs != fresh {
		t.Fatalf("in-window frame must be returned: got=%+v err=%v", got, err)
	}
}

// 用例 6:maxFrames 条数硬上限(§9.18):超限只保留最新 N 条。
func TestOfflineBuffer_EnforcesMaxFrames(t *testing.T) {
	repo, _ := newTestRepo(t, time.Minute, 3)
	ctx := context.Background()
	base := time.Now().UnixMilli()

	var cursors []int64
	for i := 0; i < 5; i++ {
		cursors = append(cursors, buffer(t, repo, 600, base, "p"))
	}

	got, err := repo.Range(ctx, 600, 0)
	if err != nil {
		t.Fatalf("Range err=%v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len=%d want=3 (max frames cap)", len(got))
	}
	for i, f := range got {
		if want := cursors[i+2]; f.ScoreMs != want {
			t.Fatalf("i=%d score=%d want=%d (must keep newest)", i, f.ScoreMs, want)
		}
	}
}

// 用例 7:游标跨玩家独立——玩家 A 的高游标基线不得拖高玩家 B 的游标。
func TestOfflineBuffer_CursorPerPlayer(t *testing.T) {
	repo, mr := newTestRepo(t, time.Minute, 0)
	base := time.Now().UnixMilli()

	// 玩家 701 的游标基线被抬到远未来(模拟其历史高游标)。
	if _, err := mr.ZAdd(offlineKey(701), float64(base+int64(time.Hour.Milliseconds())), "wm"); err != nil {
		t.Fatalf("seed wm: %v", err)
	}
	cA := buffer(t, repo, 701, 0, "a")
	if cA <= base+int64(time.Hour.Milliseconds()) {
		t.Fatalf("player 701 cursor=%d must continue from its own high base", cA)
	}
	cB := buffer(t, repo, 702, 0, "b")
	if cB >= cA {
		t.Fatalf("player 702 cursor=%d must not be dragged by player 701 (%d)", cB, cA)
	}
	got, _ := repo.Range(context.Background(), 702, 0)
	if len(got) != 1 || got[0].ScoreMs != cB {
		t.Fatalf("player 702 got=%+v", got)
	}
}

// 用例 8:游标基线哨兵在帧修剪后存活——条数上限把旧帧全部挤出后,新游标仍必须
// 高于历史最大(否则重启/修剪会让游标回退,客户端按旧游标补推漏帧)。
func TestOfflineBuffer_CursorBaseSurvivesTrim(t *testing.T) {
	repo, _ := newTestRepo(t, time.Minute, 2)
	base := time.Now().UnixMilli()

	var last int64
	for i := 0; i < 5; i++ { // 容量 2,前 3 帧被条数修剪挤出
		last = buffer(t, repo, 800, base, "p")
	}
	next := buffer(t, repo, 800, base-1000, "late-after-trim") // 原始 ts 低于全部历史
	if next <= last {
		t.Fatalf("cursor must stay monotonic across trims: next=%d last=%d", next, last)
	}
}

// 用例 9:脏数据韧性(混版兼容路径已按 §15 删除,服务未上线无旧格式负担)——
// 非法格式 member 不阻断 Range、不被当帧投递;其 score 仍抬升游标基线(单调不回退)。
func TestOfflineBuffer_MalformedMemberSkipped(t *testing.T) {
	repo, mr := newTestRepo(t, time.Minute, 0)
	ctx := context.Background()
	base := time.Now().UnixMilli()

	junk := mustFrame("pandora.team.update", base+100, "junk-payload")
	raw, err := proto.Marshal(junk)
	if err != nil {
		t.Fatalf("marshal junk: %v", err)
	}
	member := string(raw) + string(rune(0x1F)) + "12345" // 无 %020d 前缀 = 非法格式
	if _, err := mr.ZAdd(offlineKey(900), float64(base+100), member); err != nil {
		t.Fatalf("seed malformed member: %v", err)
	}

	fresh := buffer(t, repo, 900, 0, "new")
	got, err := repo.Range(ctx, 900, 0)
	if err != nil {
		t.Fatalf("Range err=%v", err)
	}
	if len(got) != 1 || string(got[0].Frame.GetPayload()) != "new" {
		t.Fatalf("malformed member must be skipped, not delivered: got=%+v", got)
	}
	// 游标基线仍以现存最大 score 为准(脏数据不破坏单调性)。
	if fresh <= base+100 {
		t.Fatalf("cursor=%d must stay above max existing score=%d", fresh, base+100)
	}
}

// ── gap 检测(R4 P1-3 → R4 复审 P1-2:LostSince 返回丢失上界)────────────────────

// 用例 10:条数修剪产生确定丢失 → LostSince 对被修剪区间返回丢失上界(=fl),
// 对已覆盖游标返回 0;修剪后写入的新帧不受影响。
func TestOfflineBuffer_LostAfterCountTrim(t *testing.T) {
	repo, _ := newTestRepo(t, time.Minute, 2)
	ctx := context.Background()

	c1 := buffer(t, repo, 1000, 0, "a")
	buffer(t, repo, 1000, 0, "b")
	buffer(t, repo, 1000, 0, "c") // 容量 2:a 被条数修剪,fl=c1

	lost, err := repo.LostSince(ctx, 1000, c1-1)
	if err != nil || lost != c1 {
		t.Fatalf("cursor before trimmed frame must report lost up to fl=%d: lost=%d err=%v", c1, lost, err)
	}
	lost, err = repo.LostSince(ctx, 1000, c1)
	if err != nil || lost != 0 {
		t.Fatalf("cursor at trim floor has lost nothing: lost=%d err=%v", lost, err)
	}
}

// 用例 11:窗口修剪同样记录 fl;无任何丢失时(空 key / 游标最新)不误报。
func TestOfflineBuffer_LostAfterWindowTrimAndNoFalsePositive(t *testing.T) {
	repo, _ := newTestRepo(t, 50*time.Millisecond, 0)
	ctx := context.Background()

	// 空 key:从未分配过帧,任何游标都无丢失。
	if lost, err := repo.LostSince(ctx, 1100, 12345); err != nil || lost != 0 {
		t.Fatalf("empty key must not report loss: lost=%d err=%v", lost, err)
	}

	old := buffer(t, repo, 1100, 0, "old")
	time.Sleep(80 * time.Millisecond)
	fresh := buffer(t, repo, 1100, 0, "fresh") // 写侧窗口修剪删 old,fl=old

	if lost, err := repo.LostSince(ctx, 1100, old-1); err != nil || lost != old {
		t.Fatalf("window-trimmed frame must report lost up to %d: lost=%d err=%v", old, lost, err)
	}
	if lost, err := repo.LostSince(ctx, 1100, fresh); err != nil || lost != 0 {
		t.Fatalf("up-to-date cursor must not report loss: lost=%d err=%v", lost, err)
	}
}

// 用例 12:静默玩家(写侧修剪未触发)的窗口外残留帧:Range 不投递(用例 5c),
// LostSince 必须把它计入丢失上界(读侧隐藏 = 交付层面的丢),游标跳过后不再报。
func TestOfflineBuffer_LostOnHiddenAgedFrames(t *testing.T) {
	repo, _ := newTestRepo(t, 50*time.Millisecond, 0)
	ctx := context.Background()

	aged := buffer(t, repo, 1200, 0, "aged")
	time.Sleep(80 * time.Millisecond) // 无后续写,帧仍在 key 里但已滑出保留窗

	if lost, err := repo.LostSince(ctx, 1200, aged-1); err != nil || lost != aged {
		t.Fatalf("hidden aged frame must report lost up to %d: lost=%d err=%v", aged, lost, err)
	}
	if lost, err := repo.LostSince(ctx, 1200, aged); err != nil || lost != 0 {
		t.Fatalf("cursor already covering the aged frame must not report loss: lost=%d err=%v", lost, err)
	}
}
