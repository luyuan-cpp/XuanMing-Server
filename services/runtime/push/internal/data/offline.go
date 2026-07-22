// Package data 是 push 服务的数据层。
//
// 投递缓冲(2026-07-22 审计 v2:Redis 单 key 是**唯一定序与投递权威**):
//
//   - key: pandora:push:offline:<player_id>(ZSET;key 名是 dev 环境沿用,格式版本
//     说明见下"格式版本"注)
//   - score = **投递游标**:AssignAndBuffer 用**单 key 单 Lua** 原子完成
//     「读当前最大游标 → cursor = max(max+1, 服务端 now) → ZADD 帧 → 更新游标哨兵」。
//     游标基线用**服务端 now 而不是 kafka ts**(审计 R4 P1-1):kafka 重投/积压帧的
//     原始 ts 可能早于保留窗下界,若拿它当游标,同一 Lua 里的窗口修剪会把刚写入的帧
//     连同哨兵一起删掉,随后 ack = 静默丢帧;now 恒在窗口内,写入的帧必然存活到本次
//     修剪之后。副产品:不再信任任何外部时间戳,远未来 ts 污染游标(2^53 精度)一并
//     消除——游标量级恒 ≈ 墙钟毫秒(~2^41),远低于 Lua double 的 2^53 精度界。
//     单 Lua = 游标分配与入缓冲不可分割:任意多个 Pod / topic consumer 并发写同一
//     玩家,缓冲内容恒等于"已分配游标的全集"(不存在"游标 C+1 可见而更早分配的 C
//     不可见"的窗口),Range(>X) 永远返回 X 之后的**完整前缀**——跨 Pod 顺序由
//     Redis 单点定序,不依赖进程内锁(审计 P1:进程内玩家锁只在单 Pod 内成立)。
//     多 Pod 时钟偏差不破坏单调性:base+1 兜底保证游标严格递增。
//   - 哨兵 member "wm"(score = 最后分配的游标)把游标基线折进同一 key:修剪/重启
//     不丢基线;key TTL 7 天(≥ 客户端游标寿命量级),帧 member 按 5min 窗口 +
//     条数双修剪(§9.18),哨兵不受条数修剪影响(score 恒最大)。
//   - 帧 member 内 payload 保留**原始帧**(含原始 kafka ts);投递游标只存在于 score,
//     读取方(Range)把 frame.ts_ms 重铸为 score 后交付——避免"先猜游标再序列化"
//     的两段式非原子写。
//   - 交付语义:**每帧先入缓冲,后 best-effort 实时唤醒,最后才 ack kafka**;
//     连接写者按「本地唤醒 + 定时轮询」从缓冲拉取投递(跨 Pod 写入也能在轮询周期内
//     送达在线客户端,审计 P1:无 owner 转投时健康长连不能只等断线)。
//   - **at-least-once,不承诺不重**(审计 P1 诚实契约):kafka 重投/redis 结果不确定时
//     重试会给同一业务事件分配新游标 → 客户端可能重复收到,业务事件必须幂等或按
//     业务 ID 判重(chat 有 message_id;状态类推送天然幂等)。游标保证的是**不漏**
//     与**每玩家全序**,不是 exactly-once。
//
// 格式版本(审计 R4 混版诚实化):push 服务尚未上线,本格式(%020d 游标前缀)是
// **首个线上格式**,不存在需要兼容的旧副本写入——此前声称的"旧格式尾部拆分兼容"
// 是没有对应生产场景的伪装路径,已按 §15 删除。dev 环境残留的旧格式 member 按脏
// 数据跳过(Unmarshal 失败/格式不识别),由窗口修剪自然清理。上线后如需演进 member
// 格式,按 §9.17 双向兼容纪律另行设计,不得复用"静默跳过"当兼容手段。
package data

import (
	"context"
	"fmt"
	"strconv"

	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	pushv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/push/v1"
)

// OfflineFrame 是从投递缓冲读出的一条帧,ScoreMs 是投递游标(Frame.TsMs 已重铸为该值)。
type OfflineFrame struct {
	Frame   *pushv1.PushFrame
	ScoreMs int64
}

// OfflineCacheRepo 是投递缓冲的抽象接口。
type OfflineCacheRepo interface {
	// AssignAndBuffer 单 Lua 原子:分配该玩家下一个投递游标(= max(当前最大+1,
	// 服务端 now),严格递增、唯一,不信任帧原始 ts)并把帧写入缓冲。返回分配的游标。
	// 失败必须让调用方拒绝 ack(缓冲是交付权威,不能跳过)。跨 Pod / 跨 topic 并发
	// 由 Redis 单点定序,调用方无需持锁。
	AssignAndBuffer(ctx context.Context, playerID uint64, frame *pushv1.PushFrame) (int64, error)

	// Range 拉取该玩家 score **严格大于** afterCursor 且不早于保留窗下界(now-ttl,
	// 与写侧修剪同界:读结果不随"最近有没有写触发修剪"漂移)的帧(升序;游标唯一 →
	// 断点续传不漏)。返回帧的 ts_ms 已重铸为投递游标。单次返回受 maxFrames 上限,
	// 调用方按末帧游标循环拉到空。afterCursor 早于窗下界时窗口外帧可能已丢——gap
	// 判定与 resync 信号由上层处理。
	Range(ctx context.Context, playerID uint64, afterCursor int64) ([]OfflineFrame, error)

	// LostSince 返回客户端从 afterCursor 续传时,**确定分配过但不再可交付**的最高
	// 游标(被窗口/条数修剪,或滑出读侧保留窗;0 = 无丢失)。>afterCursor 时补推无法
	// 闭合,调用方必须向客户端发 resync 信号并把本地游标跳到该上界(同一段丢失只
	// 信号一次)。无误报:仅修剪线哨兵或隐藏帧确证丢失时非 0。查询失败必须返回 err,
	// 调用方 fail-closed(R4 复审:不得把「查不了」当「无丢失」推进游标越过缺口)。
	LostSince(ctx context.Context, playerID uint64, afterCursor int64) (int64, error)
}

// RedisOfflineCacheRepo 是基于 go-redis/v9 的实现。
type RedisOfflineCacheRepo struct {
	rdb       redis.UniversalClient
	ttl       time.Duration
	maxFrames int64
}

// cursorKeyTTL 是整 key 的保活时长(哨兵 member 承载游标基线,必须活得比客户端本地
// 游标寿命久;帧 member 另按 r.ttl 窗口修剪)。
const cursorKeyTTL = 7 * 24 * time.Hour

// NewRedisOfflineCacheRepo 构造,ttl<=0 时 fallback 到 5min、maxFrames<=0 时
// fallback 到 512(防 cfg 漏配;§9.18 有界纪律)。
func NewRedisOfflineCacheRepo(rdb redis.UniversalClient, ttl time.Duration, maxFrames int) *RedisOfflineCacheRepo {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if maxFrames <= 0 {
		maxFrames = 512
	}
	return &RedisOfflineCacheRepo{rdb: rdb, ttl: ttl, maxFrames: int64(maxFrames)}
}

// offlineKey 投递缓冲 key(dev 环境沿用既有名;格式版本见 package 注释)。
func offlineKey(playerID uint64) string {
	return fmt.Sprintf("pandora:push:offline:%d", playerID)
}

// memberSeparator 把游标前缀和 proto bytes 分开;0x1F 不会作为 protobuf 字段标签
// 出现(payload 值字节内仍可能出现,故新格式用**定宽数字前缀**判别,不靠扫描)。
const memberSeparator = byte(0x1F)

// watermarkMember 是游标基线哨兵 member(score = 最后分配的游标;不参与投递)。
const watermarkMember = "wm"

// trimFloorMember 是修剪线哨兵 member(score = 历史被修剪帧的最高游标;不参与投递,
// R4 P1-3 gap 检测):客户端 afterCursor < 该值 ⇒ (afterCursor, fl] 内**确定**有已
// 分配但已被修剪的帧 → 补推无法闭合,必须向客户端发 resync 信号(见 LostSince 与
// biz.drainBuffer 拉空终检)。与 wm 同 key 同 Lua 维护,修剪与丢失记录不可分割。
const trimFloorMember = "fl"

// cursorPrefixWidth 是帧 member 游标前缀的十进制定宽(游标唯一 → member 唯一)。
const cursorPrefixWidth = 20

// decodeMember 反解帧 member,返回原始 proto bytes;哨兵/格式不识别(脏数据)返回 nil。
// 唯一合法格式 = %020d + 0x1F + payload(服务未上线,无历史格式兼容负担,见 package 注)。
func decodeMember(raw []byte) []byte {
	if string(raw) == watermarkMember || string(raw) == trimFloorMember {
		return nil
	}
	if len(raw) > cursorPrefixWidth && raw[cursorPrefixWidth] == memberSeparator {
		digits := true
		for _, c := range raw[:cursorPrefixWidth] {
			if c < '0' || c > '9' {
				digits = false
				break
			}
		}
		if digits {
			return raw[cursorPrefixWidth+1:]
		}
	}
	return nil
}

// assignAndBufferScript:单 key 原子「读最大游标 → 分配 → ZADD 帧 → 更新哨兵 →
// 双修剪 → 续期」。KEYS[1]=offline zset。
// ARGV: 1=服务端 now(ms;游标基线,**不用帧原始 kafka ts**——重投/积压帧的旧 ts
// 会落在窗口下界之外,被同一脚本的修剪立即删掉后 ack = 静默丢帧),
// 2=payload(原始帧 proto bytes), 3=帧窗口下界(now-ttl), 4=maxFrames, 5=key ttl 秒。
// cursor = max(基线+1, now) ≥ now > now-ttl:刚写入的帧与哨兵必然在修剪线之上。
//
// 游标基线 = max(哨兵 score, 现存最大帧 score):哨兵覆盖"帧被修剪光"的场景,
// 现存帧 score 覆盖"哨兵尚不存在(混版旧数据)"的场景。
// 修剪一律显式枚举 member(不用 ZREMRANGEBYSCORE 盲删):跳过 wm/fl 哨兵,并把被删
// 帧的最高游标记入 fl 哨兵(gap 检测权威;§9.16 修剪即丢失,丢失必须留痕)。
var assignAndBufferScript = redis.NewScript(`
local base = 0
local wmScore = redis.call('ZSCORE', KEYS[1], 'wm')
if wmScore then base = tonumber(wmScore) end
local top = redis.call('ZREVRANGE', KEYS[1], 0, 0, 'WITHSCORES')
if top[2] and tonumber(top[2]) > base then base = tonumber(top[2]) end
local cursor = base + 1
local now = tonumber(ARGV[1])
if now > cursor then cursor = now end
local member = string.format('%020d', cursor) .. string.char(31) .. ARGV[2]
redis.call('ZADD', KEYS[1], cursor, member)
redis.call('ZADD', KEYS[1], cursor, 'wm')
local fl = 0
local flScore = redis.call('ZSCORE', KEYS[1], 'fl')
if flScore then fl = tonumber(flScore) end
-- 窗口修剪:score < now-ttl 的帧删除,记录最高被删游标
local expired = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', '(' .. ARGV[3], 'WITHSCORES')
for i = 1, #expired, 2 do
  local v = expired[i]
  if v ~= 'wm' and v ~= 'fl' then
    redis.call('ZREM', KEYS[1], v)
    local s = tonumber(expired[i + 1])
    if s > fl then fl = s end
  end
end
-- 条数修剪:帧数(去掉哨兵)超上限时从最旧删起,同样记录最高被删游标
local sentinels = 1
if redis.call('ZSCORE', KEYS[1], 'fl') then sentinels = 2 end
local excess = redis.call('ZCARD', KEYS[1]) - sentinels - tonumber(ARGV[4])
if excess > 0 then
  local victims = redis.call('ZRANGE', KEYS[1], 0, excess + 1, 'WITHSCORES')
  local removed = 0
  for i = 1, #victims, 2 do
    if removed >= excess then break end
    local v = victims[i]
    if v ~= 'wm' and v ~= 'fl' then
      redis.call('ZREM', KEYS[1], v)
      local s = tonumber(victims[i + 1])
      if s > fl then fl = s end
      removed = removed + 1
    end
  end
end
if fl > 0 then redis.call('ZADD', KEYS[1], fl, 'fl') end
redis.call('EXPIRE', KEYS[1], ARGV[5])
return cursor
`)

// AssignAndBuffer 实现 OfflineCacheRepo.AssignAndBuffer(见 package 注释)。
func (r *RedisOfflineCacheRepo) AssignAndBuffer(ctx context.Context, playerID uint64, frame *pushv1.PushFrame) (int64, error) {
	if frame == nil {
		return 0, errcode.New(errcode.ErrInvalidArg, "nil frame")
	}
	payload, merr := proto.Marshal(frame)
	if merr != nil {
		return 0, errcode.New(errcode.ErrInternal, "marshal push frame: %v", merr)
	}
	nowMs := time.Now().UnixMilli()
	expiredBefore := nowMs - r.ttl.Milliseconds()
	cursor, err := assignAndBufferScript.Run(ctx, r.rdb,
		[]string{offlineKey(playerID)},
		nowMs, payload, expiredBefore, r.maxFrames, int64(cursorKeyTTL/time.Second),
	).Int64()
	if err != nil {
		return 0, errcode.New(errcode.ErrInternal, "redis assign+buffer: %v", err)
	}
	// 调用方随后的实时投递用同一游标语义:回写帧对象(缓冲内 payload 保留原始 ts,
	// 读取侧统一重铸)。
	frame.TsMs = cursor
	return cursor, nil
}

// Range 实现 OfflineCacheRepo.Range(严格 > afterCursor 且 ≥ 保留窗下界;帧 ts_ms
// 重铸为投递游标)。读侧窗口过滤与写侧修剪同界(审计 R4 P1-2):修剪只在该玩家有新写
// 时触发,静默玩家的 key 里可能残留最长 7 天(key TTL)的旧帧;若首连/久离线重连把
// 它们当"新消息"整批補推,契约声明的 5min 窗口就形同虚设。窗口外帧一律不投递,
// 交付契约收敛为确定性的「(now-ttl, now] 内不漏」。
func (r *RedisOfflineCacheRepo) Range(ctx context.Context, playerID uint64, afterCursor int64) ([]OfflineFrame, error) {
	windowFloor := time.Now().UnixMilli() - r.ttl.Milliseconds()
	min := strconv.FormatInt(windowFloor, 10) // 与写侧修剪线一致:score ≥ now-ttl 保留
	if afterCursor >= windowFloor {
		min = "(" + strconv.FormatInt(afterCursor, 10)
	}

	zs, err := r.rdb.ZRangeByScoreWithScores(ctx, offlineKey(playerID), &redis.ZRangeBy{
		Min:   min,
		Max:   "+inf",
		Count: r.maxFrames + 1, // +1:结果里可能混着哨兵 member
	}).Result()
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "redis offline zrange: %v", err)
	}
	if len(zs) == 0 {
		return nil, nil
	}

	out := make([]OfflineFrame, 0, len(zs))
	for _, z := range zs {
		raw, ok := z.Member.(string)
		if !ok || raw == watermarkMember || raw == trimFloorMember {
			continue
		}
		payload := decodeMember([]byte(raw))
		if payload == nil {
			continue
		}
		var frame pushv1.PushFrame
		if err := proto.Unmarshal(payload, &frame); err != nil {
			// 单帧坏不阻断其它(避免一条脏数据拖死整个补推)
			continue
		}
		cursor := int64(z.Score)
		frame.TsMs = cursor // 投递游标权威在 score,交付前统一重铸
		out = append(out, OfflineFrame{Frame: &frame, ScoreMs: cursor})
	}
	return out, nil
}

// LostSince 实现 OfflineCacheRepo.LostSince:返回从 afterCursor 续传时已确定不可
// 交付的最高游标(0 = 无丢失)。两个精确来源(均无误报):
//  1. fl 哨兵(修剪线):afterCursor < fl ⇒ (afterCursor, fl] 内有已分配但被修剪
//     (窗口/条数)的帧,物理已删;
//  2. 读侧窗口过滤:仍留在 key 里但 score < now-ttl 的帧不投递(写侧修剪未触发的
//     静默残留),afterCursor 之后此类隐藏帧的最高 score 同样构成丢失上界。
//
// 两次读之间非原子:修剪单调推进,竞态最多把本次丢失漏成下次拉空再报,不产生误报。
func (r *RedisOfflineCacheRepo) LostSince(ctx context.Context, playerID uint64, afterCursor int64) (int64, error) {
	key := offlineKey(playerID)
	var lost int64
	flScore, err := r.rdb.ZScore(ctx, key, trimFloorMember).Result()
	switch {
	case err == nil:
		if fl := int64(flScore); fl > afterCursor {
			lost = fl
		}
	case err != redis.Nil:
		return 0, errcode.New(errcode.ErrInternal, "redis gap floor: %v", err)
	}

	windowFloor := time.Now().UnixMilli() - r.ttl.Milliseconds()
	if afterCursor >= windowFloor {
		return lost, nil
	}
	// (afterCursor, windowFloor) 内隐藏帧的最高 score:倒序取几条以便滤掉哨兵
	// (wm/fl 哨兵 score 也可能落在该区间,首个真实帧 member 即最高丢失上界)。
	zs, err := r.rdb.ZRevRangeByScoreWithScores(ctx, key, &redis.ZRangeBy{
		Min:   "(" + strconv.FormatInt(afterCursor, 10),
		Max:   "(" + strconv.FormatInt(windowFloor, 10),
		Count: 3,
	}).Result()
	if err != nil {
		return 0, errcode.New(errcode.ErrInternal, "redis gap hidden scan: %v", err)
	}
	for _, z := range zs {
		if raw, ok := z.Member.(string); ok && raw != watermarkMember && raw != trimFloorMember {
			if s := int64(z.Score); s > lost {
				lost = s
			}
			break
		}
	}
	return lost, nil
}
