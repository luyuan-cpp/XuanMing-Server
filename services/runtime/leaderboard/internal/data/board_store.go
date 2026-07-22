// 本文件实现 Redis ZSET 排行榜:实时排名的权威计算层(2026-06-27;2026-07-21 加榜外区间估算)。
//
// key(同一 board 用 hashtag 锁同一 Redis Cluster slot,SubmitScore 的 Lua 同时碰全部 key,避免 CROSSSLOT):
//
//	pandora:lb:{<board>}:z   ZSET   member=entity_id,score=packed(见 §3.3 时间 tie-break 打包;max_size 截断只保留 Top-N)
//	pandora:lb:{<board>}:t   HASH   entity_id → updated_at_ms(展示 / 审计;只保留榜内成员,随截断清理)
//	pandora:lb:{<board>}:m   HASH   榜元信息(asc / tie / bw 桶宽,建榜时定死)
//	pandora:lb:{<board>}:s   HASH   entity_id → 真实分(全员,不随截断清理;截断后 INCREMENT 累计 /
//	                                SET_IF_HIGHER 不降级语义靠它保持正确,也是榜外估算的分数来源)
//	pandora:lb:{<board>}:h   HASH   bucket(floor(score/bw)) → count 分数直方图(全员;榜外名次区间估算)
//
// <board> = "<board_type>:<scope>:<scope_id>:<period>"(period 为空用 "-" 占位避免空段)。
//
// 分数打包(docs/design/decision-revisit-leaderboard.md §3.3):
//   - 不开 tie-break:packed = real(同分按 member 字典序);
//   - 降序榜 + tie-break:packed = real - normTs*1e-13(同分先达者 packed 大 → 名次高);
//   - 升序榜 + tie-break:packed = real + normTs*1e-13(同分先达者 packed 小 → 名次高);
//   - 还原真实分:real = round(packed)(时间项 < 0.5,不影响取整)。
//
// 榜外区间估算(截断榜专用,CLAUDE.md §9.18 有界精神):
//   - 精确名次只保留 Top-max_size(ZSET 有界);榜外玩家名次 = 直方图里「比我优的桶计数和 +
//     本桶一半」的估算值(非权威、可重建的派生展示状态,不变量 §22),客户端按「约 X 名 / 百分位」展示。
//   - :s / :h 是全员结构,内存量级 = 参与人数 ×(hash 条目 ≈ 数十字节),远小于全员 ZSET;
//     周期榜随 Clear / TTL 清理。直方图桶数 = 分数跨度 / bw,桶宽按榜量纲配置,桶索引钳制在
//     ±maxBucketIdx 防异常分数撑爆 field 数。
//   - 升级兼容:本改动前建的榜 :s / :h 为空,成员首次再上报时以旧分(ZSET 回退)补记;
//     直方图对存量未再上报成员会低估「比我优」人数,由估算语义(约值)+ 榜内 ZCARD 钳制兜底。
package data

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// lbEpochMs 是排行榜时间 tie-break 的纪元(2026-01-01 UTC,毫秒)。normTs = ts_ms - lbEpochMs。
const lbEpochMs int64 = 1767225600000

// Scope 是榜归属维度(对齐 proto LeaderboardScope 数值)。
type Scope int32

const (
	ScopeGlobal   Scope = 1
	ScopeGuild    Scope = 2
	ScopeInstance Scope = 3
	ScopeCustom   Scope = 4
)

// 上报模式(对齐 proto SubmitMode 数值)。
const (
	ModeSetIfHigher int32 = 1
	ModeSet         int32 = 2
	ModeIncrement   int32 = 3
)

// BoardKey 是榜的复合标识(存储层内部结构)。
type BoardKey struct {
	BoardType uint32
	Scope     Scope
	ScopeID   uint64
	Period    string
}

// Options 是建榜 / 写入行为参数。
type Options struct {
	TTLSeconds     int64
	MaxSize        int64
	TieBreakByTime bool
	Ascending      bool
	// EstimateBucketWidth 榜外估算直方图桶宽(>0;biz 层已用服务默认值兜底)。建榜时写入
	// meta 定死,后续上报忽略变更(桶宽混用会毁掉直方图)。
	EstimateBucketWidth int64
}

// Entry 是榜上一项(存储层视图)。
type Entry struct {
	EntityID    uint64
	Score       int64
	Rank        int64 // 1-based
	UpdatedAtMs int64
}

// String 返回 board 串(period 空用 "-" 占位)。
func (b BoardKey) String() string {
	p := b.Period
	if p == "" {
		p = "-"
	}
	return fmt.Sprintf("%d:%d:%d:%s", b.BoardType, int32(b.Scope), b.ScopeID, p)
}

func (b BoardKey) zKey() string { return fmt.Sprintf("pandora:lb:{%s}:z", b.String()) }
func (b BoardKey) tKey() string { return fmt.Sprintf("pandora:lb:{%s}:t", b.String()) }
func (b BoardKey) mKey() string { return fmt.Sprintf("pandora:lb:{%s}:m", b.String()) }
func (b BoardKey) sKey() string { return fmt.Sprintf("pandora:lb:{%s}:s", b.String()) }
func (b BoardKey) hKey() string { return fmt.Sprintf("pandora:lb:{%s}:h", b.String()) }

// unpackReal 把 ZSET packed score 还原成真实整数分(round)。
func unpackReal(packed float64) int64 {
	return int64(math.Floor(packed + 0.5))
}

// maxBucketIdx 钳制直方图桶索引绝对值上限(防异常分数把 field 数撑到失控;正常配置远达不到)。
// 与 submitLua / removeLua 内的钳制常量必须一致。
const maxBucketIdx int64 = 1 << 20

// bucketOf 返回分数所属直方图桶(floor 除法,负分正确;索引钳制 ±maxBucketIdx)。
func bucketOf(score, width int64) int64 {
	q := score / width
	if score%width != 0 && (score < 0) != (width < 0) {
		q--
	}
	if q > maxBucketIdx {
		q = maxBucketIdx
	}
	if q < -maxBucketIdx {
		q = -maxBucketIdx
	}
	return q
}

// BoardStore 是排行榜存储抽象(Redis ZSET 实现)。biz 只依赖此接口。
type BoardStore interface {
	// Submit 按 mode 写入分数并(可选)截断 / 设 TTL,返回写入后的真实分与 1-based 名次(0=未上榜)。
	Submit(ctx context.Context, b BoardKey, entityID uint64, score int64, mode int32, opt Options, tsMs int64) (newScore, rank int64, err error)
	// Rank 查某 entity 的名次 + 分;不在榜 found=false。
	Rank(ctx context.Context, b BoardKey, entityID uint64, ascending bool) (entry Entry, found bool, err error)
	// Estimate 用分数直方图估算未进精确榜 entity 的名次(约值,UpdatedAtMs 恒 0);
	// total 是直方图口径的参与总人数。entity 从未上报 / 榜无直方图(升级前旧榜)→ found=false。
	Estimate(ctx context.Context, b BoardKey, entityID uint64, ascending bool) (entry Entry, total int64, found bool, err error)
	// Range 取榜区间(offset 0-based)。
	Range(ctx context.Context, b BoardKey, offset int64, limit int, ascending bool) ([]Entry, error)
	// Around 取某 entity 上下 radius 名(含自身);不在榜 found=false。
	Around(ctx context.Context, b BoardKey, entityID uint64, radius int, ascending bool) ([]Entry, bool, error)
	// Total 返回榜总人数。
	Total(ctx context.Context, b BoardKey) (int64, error)
	// GetMeta 读榜元信息(ascending / tie-break);榜不存在 exists=false。
	GetMeta(ctx context.Context, b BoardKey) (ascending, tieBreak, exists bool, err error)
	// Remove 移除某 entity。
	Remove(ctx context.Context, b BoardKey, entityID uint64) error
	// Delete 删整个榜(z + t)。
	Delete(ctx context.Context, b BoardKey) error
	// Clear 清空榜分数但保留 key(周期 reset)。
	Clear(ctx context.Context, b BoardKey) error
}

// DefaultEstimateBucketWidth 是估算直方图桶宽的兜底默认值(MMR 量纲;conf 默认值也引用此常量)。
const DefaultEstimateBucketWidth int64 = 25

// RedisBoardStore 是基于 go-redis ZSET 的 BoardStore。
type RedisBoardStore struct {
	rdb        redis.UniversalClient
	submitFunc *redis.Script
	removeFunc *redis.Script
}

// NewRedisBoardStore 构造。
func NewRedisBoardStore(rdb redis.UniversalClient) *RedisBoardStore {
	return &RedisBoardStore{
		rdb:        rdb,
		submitFunc: redis.NewScript(submitLua),
		removeFunc: redis.NewScript(removeLua),
	}
}

// submitLua 原子完成:读旧分(:s 全员分,回退 :z)→ 按 mode 算新真实分 → 打包 → ZADD/HSET →
// 维护全员分数 :s 与直方图 :h → 截断 maxSize(只清 :z/:t,:s/:h 保留全员)→ 设 TTL → 返回真实分 + 名次。
// KEYS[1]=zkey KEYS[2]=tkey KEYS[3]=mkey KEYS[4]=skey KEYS[5]=hkey
// ARGV: 1 member 2 score 3 mode 4 tieBreak(0/1) 5 ascending(0/1) 6 tsMs 7 epochMs 8 maxSize 9 ttlSeconds 10 bucketWidth
// 返回: {newReal, rank1Based}
const submitLua = `
local zkey, tkey, mkey, skey, hkey = KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5]
local member = ARGV[1]
local score  = tonumber(ARGV[2])
local mode   = tonumber(ARGV[3])
local tie    = tonumber(ARGV[4])
local asc    = tonumber(ARGV[5])
local ts     = tonumber(ARGV[6])
local epoch  = tonumber(ARGV[7])
local maxSize= tonumber(ARGV[8])
local ttl    = tonumber(ARGV[9])
local bwArg  = tonumber(ARGV[10])

local function realOf(p) return math.floor(p + 0.5) end

-- 桶索引(floor 除法;钳制 ±1048576,与 Go 侧 maxBucketIdx 一致)
local MAXB = 1048576
local function bucketOf(v, w)
  local q = math.floor(v / w)
  if q > MAXB then q = MAXB end
  if q < -MAXB then q = -MAXB end
  return q
end

-- 首次写定义榜元信息(asc / tie),供后续读查询判定排序方向
if redis.call('EXISTS', mkey) == 0 then
  redis.call('HSET', mkey, 'asc', asc, 'tie', tie)
end
-- 桶宽只在首次定死(升级前旧榜的 meta 无 bw,首次再上报时补记);后续变更忽略
if redis.call('HEXISTS', mkey, 'bw') == 0 then
  redis.call('HSET', mkey, 'bw', bwArg)
end
local bw = tonumber(redis.call('HGET', mkey, 'bw'))

-- 旧真实分:优先 :s 全员分(截断后仍在);:s 无记录回退 :z(升级前旧榜存量成员)
local sRaw = redis.call('HGET', skey, member)
local sExisted = (sRaw ~= false)
local curReal = nil
if sExisted then
  curReal = tonumber(sRaw)
else
  local cur = redis.call('ZSCORE', zkey, member)
  if cur then curReal = realOf(tonumber(cur)) end
end

-- 决定新真实分与是否写入
local newReal
local doWrite = true
if mode == 3 then
  newReal = (curReal or 0) + score
else
  newReal = score
  if mode == 1 and curReal ~= nil then
    if asc == 1 then
      if newReal >= curReal then doWrite = false end
    else
      if newReal <= curReal then doWrite = false end
    end
  end
end

if doWrite then
  local normTs = ts - epoch
  if normTs < 0 then normTs = 0 end
  local packed = newReal
  if tie == 1 then
    if asc == 1 then packed = newReal + normTs * 1e-13
    else packed = newReal - normTs * 1e-13 end
  end
  redis.call('ZADD', zkey, packed, member)
  redis.call('HSET', tkey, member, ts)
else
  newReal = curReal
end

-- 全员分数 :s + 直方图 :h(doWrite=false 且已记录时分数未变,无需动)
if doWrite or not sExisted then
  redis.call('HSET', skey, member, newReal)
  local newB = bucketOf(newReal, bw)
  if not sExisted then
    -- 首次进直方图(含旧榜存量成员补记)
    redis.call('HINCRBY', hkey, newB, 1)
  else
    local oldB = bucketOf(curReal, bw)
    if oldB ~= newB then
      redis.call('HINCRBY', hkey, newB, 1)
      local c = redis.call('HINCRBY', hkey, oldB, -1)
      if c <= 0 then redis.call('HDEL', hkey, oldB) end
    end
  end
end

-- 截断 maxSize(精确榜只保留最优 Top-N,清理被挤出者的 t 记录;:s/:h 保留全员供估算)
if maxSize > 0 then
  local n = redis.call('ZCARD', zkey)
  if n > maxSize then
    local victims
    if asc == 1 then
      victims = redis.call('ZRANGE', zkey, maxSize, -1)            -- 升序:最优在前,挤出尾部(高分)
    else
      victims = redis.call('ZRANGE', zkey, 0, n - maxSize - 1)     -- 降序:最优在后,挤出头部(低分)
    end
    if victims and #victims > 0 then
      redis.call('ZREM', zkey, unpack(victims))
      redis.call('HDEL', tkey, unpack(victims))
    end
  end
end

-- TTL(临时榜)
if ttl > 0 then
  redis.call('EXPIRE', zkey, ttl)
  redis.call('EXPIRE', tkey, ttl)
  redis.call('EXPIRE', mkey, ttl)
  redis.call('EXPIRE', skey, ttl)
  redis.call('EXPIRE', hkey, ttl)
end

-- 名次(1-based;被截断 / 不在榜 → 0)
local rank = 0
local idx
if asc == 1 then idx = redis.call('ZRANK', zkey, member)
else idx = redis.call('ZREVRANK', zkey, member) end
if idx ~= false and idx ~= nil then rank = idx + 1 end

if newReal == nil then newReal = 0 end
return {newReal, rank}
`

// Submit 调 Lua 原子写入。
func (s *RedisBoardStore) Submit(ctx context.Context, b BoardKey, entityID uint64, score int64, mode int32, opt Options, tsMs int64) (int64, int64, error) {
	tie := 0
	if opt.TieBreakByTime {
		tie = 1
	}
	asc := 0
	if opt.Ascending {
		asc = 1
	}
	bw := opt.EstimateBucketWidth
	if bw <= 0 {
		bw = DefaultEstimateBucketWidth // 防御:biz 已兜底,此处保证 Lua 不除零
	}
	res, err := s.submitFunc.Run(ctx, s.rdb,
		[]string{b.zKey(), b.tKey(), b.mKey(), b.sKey(), b.hKey()},
		strconv.FormatUint(entityID, 10), score, mode, tie, asc, tsMs, lbEpochMs, opt.MaxSize, opt.TTLSeconds, bw,
	).Result()
	if err != nil {
		return 0, 0, errcode.New(errcode.ErrInternal, "lb submit board=%s entity=%d: %v", b.String(), entityID, err)
	}
	arr, ok := res.([]interface{})
	if !ok || len(arr) != 2 {
		return 0, 0, errcode.New(errcode.ErrInternal, "lb submit bad reply board=%s", b.String())
	}
	newScore, _ := arr[0].(int64)
	rank, _ := arr[1].(int64)
	return newScore, rank, nil
}

// Rank 查名次 + 分。
func (s *RedisBoardStore) Rank(ctx context.Context, b BoardKey, entityID uint64, ascending bool) (Entry, bool, error) {
	member := strconv.FormatUint(entityID, 10)
	zkey := b.zKey()
	var idx int64
	var rerr error
	if ascending {
		idx, rerr = s.rdb.ZRank(ctx, zkey, member).Result()
	} else {
		idx, rerr = s.rdb.ZRevRank(ctx, zkey, member).Result()
	}
	if rerr == redis.Nil {
		return Entry{}, false, nil
	}
	if rerr != nil {
		return Entry{}, false, errcode.New(errcode.ErrInternal, "lb rank board=%s entity=%d: %v", b.String(), entityID, rerr)
	}
	packed, serr := s.rdb.ZScore(ctx, zkey, member).Result()
	if serr != nil {
		return Entry{}, false, errcode.New(errcode.ErrInternal, "lb score board=%s entity=%d: %v", b.String(), entityID, serr)
	}
	updated, _ := s.rdb.HGet(ctx, b.tKey(), member).Int64()
	return Entry{EntityID: entityID, Score: unpackReal(packed), Rank: idx + 1, UpdatedAtMs: updated}, true, nil
}

// Estimate 用分数直方图估算未进精确榜 entity 的名次(约值)。
//
// 估算 = 比我优的桶计数和 + 本桶一半(桶内取中位),并钳制到 ZCARD+1 之后——保证榜外估算
// 名次永远排在精确榜内玩家之后(直方图是约值,不能和精确区打架)。读路径纯只读、无锁,
// 直方图是可重建派生状态(不变量 §22),不参与任何权威写。
func (s *RedisBoardStore) Estimate(ctx context.Context, b BoardKey, entityID uint64, ascending bool) (Entry, int64, bool, error) {
	member := strconv.FormatUint(entityID, 10)
	scoreRaw, err := s.rdb.HGet(ctx, b.sKey(), member).Result()
	if err == redis.Nil {
		return Entry{}, 0, false, nil // 从未上报(或升级前旧榜未再上报)
	}
	if err != nil {
		return Entry{}, 0, false, errcode.New(errcode.ErrInternal, "lb estimate score board=%s entity=%d: %v", b.String(), entityID, err)
	}
	score, perr := strconv.ParseInt(scoreRaw, 10, 64)
	if perr != nil {
		return Entry{}, 0, false, errcode.New(errcode.ErrInternal, "lb estimate bad score board=%s entity=%d: %q", b.String(), entityID, scoreRaw)
	}
	bwRaw, err := s.rdb.HGet(ctx, b.mKey(), "bw").Result()
	if err == redis.Nil {
		return Entry{}, 0, false, nil // 升级前旧榜无直方图配置,不可估算
	}
	if err != nil {
		return Entry{}, 0, false, errcode.New(errcode.ErrInternal, "lb estimate bw board=%s: %v", b.String(), err)
	}
	bw, perr := strconv.ParseInt(bwRaw, 10, 64)
	if perr != nil || bw <= 0 {
		return Entry{}, 0, false, errcode.New(errcode.ErrInternal, "lb estimate bad bw board=%s: %q", b.String(), bwRaw)
	}
	hist, err := s.rdb.HGetAll(ctx, b.hKey()).Result()
	if err != nil {
		return Entry{}, 0, false, errcode.New(errcode.ErrInternal, "lb estimate hist board=%s: %v", b.String(), err)
	}
	myBucket := bucketOf(score, bw)
	var better, own, total int64
	for idxRaw, cntRaw := range hist {
		idx, e1 := strconv.ParseInt(idxRaw, 10, 64)
		cnt, e2 := strconv.ParseInt(cntRaw, 10, 64)
		if e1 != nil || e2 != nil || cnt <= 0 {
			continue // 脏 field 跳过,不影响其余桶
		}
		total += cnt
		if idx == myBucket {
			own = cnt
			continue
		}
		if (ascending && idx < myBucket) || (!ascending && idx > myBucket) {
			better += cnt
		}
	}
	est := better + (own+1)/2
	// 榜外估算名次不得落进精确区:钳到精确榜人数之后
	onBoard, err := s.rdb.ZCard(ctx, b.zKey()).Result()
	if err != nil {
		return Entry{}, 0, false, errcode.New(errcode.ErrInternal, "lb estimate zcard board=%s: %v", b.String(), err)
	}
	if est < onBoard+1 {
		est = onBoard + 1
	}
	return Entry{EntityID: entityID, Score: score, Rank: est}, total, true, nil
}

// Range 取榜区间。
func (s *RedisBoardStore) Range(ctx context.Context, b BoardKey, offset int64, limit int, ascending bool) ([]Entry, error) {
	if limit <= 0 || offset < 0 {
		return nil, nil
	}
	zkey := b.zKey()
	stop := offset + int64(limit) - 1
	var zs []redis.Z
	var rerr error
	if ascending {
		zs, rerr = s.rdb.ZRangeWithScores(ctx, zkey, offset, stop).Result()
	} else {
		zs, rerr = s.rdb.ZRevRangeWithScores(ctx, zkey, offset, stop).Result()
	}
	if rerr != nil {
		return nil, errcode.New(errcode.ErrInternal, "lb range board=%s: %v", b.String(), rerr)
	}
	return s.toEntries(ctx, b, zs, offset)
}

// Around 取某 entity 上下 radius 名。
func (s *RedisBoardStore) Around(ctx context.Context, b BoardKey, entityID uint64, radius int, ascending bool) ([]Entry, bool, error) {
	member := strconv.FormatUint(entityID, 10)
	zkey := b.zKey()
	var idx int64
	var rerr error
	if ascending {
		idx, rerr = s.rdb.ZRank(ctx, zkey, member).Result()
	} else {
		idx, rerr = s.rdb.ZRevRank(ctx, zkey, member).Result()
	}
	if rerr == redis.Nil {
		return nil, false, nil
	}
	if rerr != nil {
		return nil, false, errcode.New(errcode.ErrInternal, "lb around rank board=%s entity=%d: %v", b.String(), entityID, rerr)
	}
	start := idx - int64(radius)
	if start < 0 {
		start = 0
	}
	stop := idx + int64(radius)
	var zs []redis.Z
	if ascending {
		zs, rerr = s.rdb.ZRangeWithScores(ctx, zkey, start, stop).Result()
	} else {
		zs, rerr = s.rdb.ZRevRangeWithScores(ctx, zkey, start, stop).Result()
	}
	if rerr != nil {
		return nil, false, errcode.New(errcode.ErrInternal, "lb around range board=%s: %v", b.String(), rerr)
	}
	entries, eerr := s.toEntries(ctx, b, zs, start)
	if eerr != nil {
		return nil, false, eerr
	}
	return entries, true, nil
}

// toEntries 把 ZSET 区间结果 + updated_at 拼成 Entry 列表(startRank 为该批首项的 0-based 名次)。
func (s *RedisBoardStore) toEntries(ctx context.Context, b BoardKey, zs []redis.Z, startRank int64) ([]Entry, error) {
	if len(zs) == 0 {
		return nil, nil
	}
	members := make([]string, len(zs))
	for i, z := range zs {
		members[i], _ = z.Member.(string)
	}
	updated, err := s.rdb.HMGet(ctx, b.tKey(), members...).Result()
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "lb hmget board=%s: %v", b.String(), err)
	}
	out := make([]Entry, 0, len(zs))
	for i, z := range zs {
		id, perr := strconv.ParseUint(members[i], 10, 64)
		if perr != nil {
			continue
		}
		var up int64
		if i < len(updated) {
			if sv, ok := updated[i].(string); ok {
				up, _ = strconv.ParseInt(sv, 10, 64)
			}
		}
		out = append(out, Entry{
			EntityID:    id,
			Score:       unpackReal(z.Score),
			Rank:        startRank + int64(i) + 1,
			UpdatedAtMs: up,
		})
	}
	return out, nil
}

// Total 返回榜总人数。
func (s *RedisBoardStore) Total(ctx context.Context, b BoardKey) (int64, error) {
	n, err := s.rdb.ZCard(ctx, b.zKey()).Result()
	if err != nil {
		return 0, errcode.New(errcode.ErrInternal, "lb total board=%s: %v", b.String(), err)
	}
	return n, nil
}

// GetMeta 读榜元信息(ascending / tie-break)。
func (s *RedisBoardStore) GetMeta(ctx context.Context, b BoardKey) (bool, bool, bool, error) {
	vals, err := s.rdb.HMGet(ctx, b.mKey(), "asc", "tie").Result()
	if err != nil {
		return false, false, false, errcode.New(errcode.ErrInternal, "lb meta board=%s: %v", b.String(), err)
	}
	if len(vals) < 2 || vals[0] == nil {
		return false, false, false, nil // 榜不存在
	}
	asc := vals[0] == "1"
	tie := len(vals) > 1 && vals[1] == "1"
	return asc, tie, true, nil
}

// removeLua 原子移除某 entity(封号 / 作弊清理):按 :s 记录的分回扣直方图,再清 z/t/s。
// KEYS[1]=zkey KEYS[2]=tkey KEYS[3]=mkey KEYS[4]=skey KEYS[5]=hkey
// ARGV: 1 member
const removeLua = `
local zkey, tkey, mkey, skey, hkey = KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5]
local member = ARGV[1]

local MAXB = 1048576
local function bucketOf(v, w)
  local q = math.floor(v / w)
  if q > MAXB then q = MAXB end
  if q < -MAXB then q = -MAXB end
  return q
end

local sRaw = redis.call('HGET', skey, member)
if sRaw then
  local bwRaw = redis.call('HGET', mkey, 'bw')
  if bwRaw then
    local bw = tonumber(bwRaw)
    if bw and bw > 0 then
      local bkt = bucketOf(tonumber(sRaw), bw)
      local c = redis.call('HINCRBY', hkey, bkt, -1)
      if c <= 0 then redis.call('HDEL', hkey, bkt) end
    end
  end
  redis.call('HDEL', skey, member)
end
redis.call('ZREM', zkey, member)
redis.call('HDEL', tkey, member)
return 1
`

// Remove 移除某 entity(直方图同步回扣)。
func (s *RedisBoardStore) Remove(ctx context.Context, b BoardKey, entityID uint64) error {
	member := strconv.FormatUint(entityID, 10)
	if err := s.removeFunc.Run(ctx, s.rdb,
		[]string{b.zKey(), b.tKey(), b.mKey(), b.sKey(), b.hKey()}, member,
	).Err(); err != nil {
		return errcode.New(errcode.ErrInternal, "lb remove board=%s entity=%d: %v", b.String(), entityID, err)
	}
	return nil
}

// Delete 删整个榜(z + t + meta + 全员分 + 直方图)。
func (s *RedisBoardStore) Delete(ctx context.Context, b BoardKey) error {
	if err := s.rdb.Del(ctx, b.zKey(), b.tKey(), b.mKey(), b.sKey(), b.hKey()).Err(); err != nil {
		return errcode.New(errcode.ErrInternal, "lb delete board=%s: %v", b.String(), err)
	}
	return nil
}

// Clear 清空榜分数(周期 reset;保留 meta 以延续榜配置,全员分 / 直方图随周期一并清零)。
func (s *RedisBoardStore) Clear(ctx context.Context, b BoardKey) error {
	if err := s.rdb.Del(ctx, b.zKey(), b.tKey(), b.sKey(), b.hKey()).Err(); err != nil {
		return errcode.New(errcode.ErrInternal, "lb clear board=%s: %v", b.String(), err)
	}
	return nil
}
