// progress.go — 战斗中实时进度通道业务逻辑(实时成长,docs/design/realtime-progression.md)。
//
// 链路:DS 异步批量 ReportProgress(事实事件,seq 幂等)
//
//	→ 本文件:校验(roster / 上限 / 升序)→ 换算(怪物经验表 / 掉落白名单,DS 不可信)
//	→ repo.ApplyProgress(水位 CAS + 进度出箱同事务)
//	→ RunProgressPublisher:出箱 worker 幂等调 player.AddExperience / inventory.GrantInstances。
//
// 错误语义(DS 侧行为契约,battle.proto ReportProgress 注释):
//   - ErrUnavailable(水位竞争 / DB 瞬时)→ DS 原批重试;
//   - ErrInvalidArg / ErrUnauthorized(坏批 / 越权)→ DS 丢批告警,继续后续批;
//   - ErrInvalidState(对局已结算 / 通道关闭)→ DS 停流,回退局后结算路径。
package biz

import (
	"context"
	"strconv"
	"time"

	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"
	battlev1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/battle/v1"

	"github.com/luyuancpp/pandora/services/battle/battle_result/internal/data"
)

// ExperienceGranter 把击杀经验幂等入账到 player(AddExperience,系统 RPC)。
//
// 由 RunProgressPublisher 调用:失败 → 返回 error → 进度出箱行保留下轮重试
// (at-least-once,配合 player exp_history uk 去重)。实现可为 nil:player_addr 未配
// → 经验出箱行积压不丢(地址配好重启后补发,与掉落 granter 同语义)。
type ExperienceGranter interface {
	AddExperience(ctx context.Context, playerID uint64, expDelta uint64, reason, idempotencyKey string) error
}

// SetExperienceGranter 注入 player 经验入账器(nil-safe,风格同 SetInstanceGranter)。
func (u *BattleResultUsecase) SetExperienceGranter(g ExperienceGranter) {
	u.expGranter = g
}

// ReportProgress 处理 DS 的一批进度事实事件,返回已应用水位 acked_seq。
//
// roster 是凭据检查器从权威 BattleStorageRecord 取的本场玩家名单(service 层注入);
// nil = dev / guard off 模式,跳过成员校验(生产 authority_mode=redis 恒非 nil)。
func (u *BattleResultUsecase) ReportProgress(ctx context.Context, matchID uint64, roster []uint64, events []*battlev1.BattleProgressEvent) (uint64, error) {
	if matchID == 0 {
		return 0, errcode.New(errcode.ErrInvalidArg, "match_id required")
	}
	if len(events) == 0 {
		return 0, errcode.New(errcode.ErrInvalidArg, "events required")
	}
	if maxBatch := u.cfg.MaxProgressBatchOrDefault(); len(events) > maxBatch {
		return 0, errcode.New(errcode.ErrInvalidArg, "batch size %d exceeds max %d", len(events), maxBatch)
	}
	var rosterSet map[uint64]struct{}
	if roster != nil {
		rosterSet = make(map[uint64]struct{}, len(roster))
		for _, pid := range roster {
			rosterSet[pid] = struct{}{}
		}
	}

	wm, err := u.repo.GetProgressWatermark(ctx, matchID)
	if err != nil {
		return 0, err
	}
	if wm.Settled {
		return 0, errcode.New(errcode.ErrInvalidState, "match %d already settled, progress rejected", matchID)
	}
	// 每场模式以水位行存在性固化(§22 单一权威):行已存在 = 本场发放权已归实时通道,
	// killswitch 中途关闭不影响进行中对局(否则"部分实时 + 结算掉落被整体抑制"会丢奖);
	// 行不存在时由开关决定能否开流(默认关,混版发布纪律见 conf.ProgressEnabled)。
	if !wm.Existed && !u.cfg.ProgressEnabled {
		return 0, errcode.New(errcode.ErrInvalidState, "realtime progress channel disabled")
	}
	lastSeq := wm.LastAppliedSeq

	// 事实换算(DS 不可信):怪物经验查配置表,拾取过白名单;未知怪 / 非白名单物品跳过并告警
	// (只丢该事实的发放,水位照常推进 —— 坏配置不能把整条流卡死)。
	maxSeqCap := u.cfg.MaxProgressSeqPerMatchOrDefault()
	maxKill := u.cfg.MaxKillCountPerFactOrDefault()
	maxPickup := u.cfg.MaxPickupCountPerFactOrDefault()
	expByPlayer := make(map[uint64]uint64)
	var (
		itemRows    []data.ProgressOutboxRecord
		prevSeq     uint64
		newSeq      = lastSeq
		firstNew    uint64
		skippedFact int
		batchExp    uint64
		batchItems  uint32
	)
	for _, e := range events {
		seq := e.GetSeq()
		if seq == 0 || seq <= prevSeq {
			return 0, errcode.New(errcode.ErrInvalidArg, "event seq must be ascending (seq=%d prev=%d)", seq, prevSeq)
		}
		prevSeq = seq
		if seq > maxSeqCap {
			return 0, errcode.New(errcode.ErrInvalidArg, "event seq %d exceeds per-match cap %d", seq, maxSeqCap)
		}
		if seq <= lastSeq {
			continue // 旧事件重放(at-least-once),已入账,跳过
		}
		if firstNew == 0 {
			firstNew = seq
		}
		newSeq = seq
		playerID := e.GetPlayerId()
		if playerID == 0 {
			return 0, errcode.New(errcode.ErrInvalidArg, "event %d missing player_id", seq)
		}
		if rosterSet != nil {
			if _, ok := rosterSet[playerID]; !ok {
				return 0, errcode.New(errcode.ErrUnauthorized, "player %d not in match %d roster", playerID, matchID)
			}
		}
		switch fact := e.GetFact().(type) {
		case *battlev1.BattleProgressEvent_MonsterKill:
			cnt := fact.MonsterKill.GetCount()
			if cnt == 0 || cnt > maxKill {
				return 0, errcode.New(errcode.ErrInvalidArg, "kill count %d out of range (max %d)", cnt, maxKill)
			}
			expPer, ok := u.cfg.MonsterExpOf(fact.MonsterKill.GetMonsterConfigId())
			if !ok {
				skippedFact++
				plog.With(ctx).Warnw("msg", "progress_monster_exp_unconfigured",
					"match_id", matchID, "monster_config_id", fact.MonsterKill.GetMonsterConfigId())
				continue
			}
			expByPlayer[playerID] += expPer * uint64(cnt)
			batchExp += expPer * uint64(cnt)
		case *battlev1.BattleProgressEvent_ItemPickup:
			cnt := fact.ItemPickup.GetCount()
			if cnt == 0 || cnt > maxPickup {
				return 0, errcode.New(errcode.ErrInvalidArg, "pickup count %d out of range (max %d)", cnt, maxPickup)
			}
			itemID := fact.ItemPickup.GetItemConfigId()
			if itemID == 0 || !u.cfg.IsDroppable(itemID) {
				skippedFact++
				plog.With(ctx).Warnw("msg", "progress_pickup_not_whitelisted",
					"match_id", matchID, "player_id", playerID, "item_config_id", itemID)
				continue
			}
			// 每拾取事实一行出箱(Seq=事实自身 seq,uk 天然唯一):单事实 count 已被
			// 夹紧到 CSV 列宽内,合法掉落永不截断(审计 P1;拾取低频,行数有界)。
			items := make([]uint32, cnt)
			for i := range items {
				items[i] = itemID
			}
			itemRows = append(itemRows, data.ProgressOutboxRecord{
				MatchID: matchID, Seq: seq, PlayerID: playerID,
				Kind: data.ProgressGrantItem, ItemConfigIDs: items,
			})
			batchItems += cnt
		default:
			// 未知事实类型:整批拒收,绝不"跳过发放但推进水位"(静默 ACK = 新事实永久丢失,
			// 审计 P1)。新 DS 携带新事实类型必须先升级 battle_result 全 fleet 再放量
			// (与 conf.ProgressEnabled 混版纪律同向:Go 先行,DS 后行)。
			return 0, errcode.New(errcode.ErrInvalidArg, "unknown progress fact seq=%d (upgrade battle_result before new DS fact types)", seq)
		}
	}

	if newSeq == lastSeq {
		// 整批都是旧事件(原批重发)→ 纯重放 ACK,零副作用。
		return lastSeq, nil
	}
	if firstNew > lastSeq+1 {
		// seq 跳号合法(DS 有界缓冲满载丢最老事件,realtime-progression.md §3/§9),
		// 但必须留痕:跳过的 seq 永不再来,结算对账 gap 告警的先导信号。
		plog.With(ctx).Warnw("msg", "progress_seq_gap",
			"match_id", matchID, "last_applied", lastSeq, "first_new", firstNew)
	}

	// 单场累计上限(事务权威侧封顶,审计 P1):失陷 DS 跨大量 seq 累计巨额产出在此拦截。
	// 读-判-写受水位 CAS 保护无竞态(并发批次只有一个能 CAS 成功,失败方重读后重判)。
	if capExp := u.cfg.MaxProgressExpPerMatchOrDefault(); wm.TotalExp+batchExp > capExp {
		return 0, errcode.New(errcode.ErrInvalidArg,
			"match %d cumulative exp %d+%d exceeds per-match cap %d", matchID, wm.TotalExp, batchExp, capExp)
	}
	if capItems := u.cfg.MaxProgressItemsPerMatchOrDefault(); wm.TotalItems+batchItems > capItems {
		return 0, errcode.New(errcode.ErrInvalidArg,
			"match %d cumulative items %d+%d exceeds per-match cap %d", matchID, wm.TotalItems, batchItems, capItems)
	}

	rows := make([]data.ProgressOutboxRecord, 0, len(expByPlayer)+len(itemRows))
	for playerID, exp := range expByPlayer {
		rows = append(rows, data.ProgressOutboxRecord{
			MatchID: matchID, Seq: newSeq, PlayerID: playerID,
			Kind: data.ProgressGrantExp, ExpDelta: exp,
		})
	}
	rows = append(rows, itemRows...)

	if err := u.repo.ApplyProgress(ctx, matchID, lastSeq, newSeq, batchExp, batchItems, rows); err != nil {
		return 0, err
	}
	plog.With(ctx).Infow("msg", "battle_progress_applied",
		"match_id", matchID, "acked_seq", newSeq, "events", len(events),
		"grant_rows", len(rows), "skipped_facts", skippedFact,
		"total_exp", wm.TotalExp+batchExp, "total_items", wm.TotalItems+batchItems)
	return newSeq, nil
}

// ReconcileProgress 结算后对账:DS 上报的 final_progress_seq vs 服务端已应用水位。
// 缺口只告警不自动补(尾窗丢失,realtime-progression.md §9 明示的残余风险)。
func reconcileProgress(ctx context.Context, matchID, finalSeq uint64, info data.ProgressSettleInfo) {
	switch {
	case finalSeq == 0 && !info.StreamExisted:
		return // 本场未走实时通道(旧 DS / 通道关),无需对账
	case finalSeq > 0 && !info.StreamExisted:
		plog.With(ctx).Warnw("msg", "progress_reconcile_stream_missing",
			"match_id", matchID, "final_seq", finalSeq,
			"hint", "DS 声称走了实时通道但服务端无水位(全部批次丢失或伪造 final_seq)")
	case finalSeq != info.LastAppliedSeq:
		plog.With(ctx).Warnw("msg", "progress_reconcile_gap",
			"match_id", matchID, "final_seq", finalSeq, "applied_seq", info.LastAppliedSeq,
			"hint", "尾窗事件丢失(DS 崩溃/网络),只告警不自动补(realtime-progression.md §9)")
	default:
		plog.With(ctx).Infow("msg", "progress_reconcile_ok",
			"match_id", matchID, "applied_seq", info.LastAppliedSeq)
	}
}

// ── 进度发放事务出箱发布器(at-least-once + 下游幂等)──────────────────────────

// RunProgressPublisher 启动后台进度出箱发放循环,直到 ctx 取消。
//
// 纪律同 RunDropPublisher:单行失败仅记录并 continue(保留出箱行下轮重试),不阻塞其他行;
// 对应下游客户端未注入(player_addr / inventory_addr 未配)的行原样跳过积压不丢。
// 背包满 + 已配 mail → 溢出转个人邮件(同键去重,直发链与邮件链至多一次)。
func (u *BattleResultUsecase) RunProgressPublisher(ctx context.Context) {
	if u.expGranter == nil && u.granter == nil {
		plog.With(ctx).Infow("msg", "progress_publisher_disabled",
			"hint", "player_addr / inventory_addr 均未配置 → 进度出箱积压不丢,配置后重启补发")
		return
	}
	interval := u.cfg.ProgressPublishIntervalOrDefault()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	plog.With(ctx).Infow("msg", "progress_publisher_started",
		"interval", interval.String(), "batch", u.cfg.ProgressBatchSizeOrDefault())
	for {
		select {
		case <-ctx.Done():
			plog.With(ctx).Infow("msg", "progress_publisher_stopped")
			return
		case <-ticker.C:
			if n, err := u.publishProgressBatch(ctx); err != nil {
				plog.With(ctx).Warnw("msg", "progress_publish_batch_failed", "granted", n, "err", err)
			}
		}
	}
}

// publishProgressBatch 取一批进度出箱行发放,返回本轮成功发放并删除的条数。
// 单行失败 → deferRow 指数退避推迟(行不丢,坏行不会长期占满首批饿死后续正常行)。
func (u *BattleResultUsecase) publishProgressBatch(ctx context.Context) (int, error) {
	recs, err := u.repo.FetchProgressOutbox(ctx, u.cfg.ProgressBatchSizeOrDefault())
	if err != nil {
		return 0, err
	}
	granted := 0
	for _, r := range recs {
		switch r.Kind {
		case data.ProgressGrantExp:
			if u.expGranter == nil {
				u.deferProgressRow(ctx, r.ID) // player_addr 未配:积压不丢,退避防饿死 item 行
				continue
			}
			key := progressIdempotencyKey(r.MatchID, r.Seq, r.PlayerID, "exp")
			if gerr := u.expGranter.AddExperience(ctx, r.PlayerID, r.ExpDelta, "monster_kill", key); gerr != nil {
				plog.With(ctx).Warnw("msg", "progress_exp_grant_failed",
					"player_id", r.PlayerID, "exp", r.ExpDelta, "err", gerr)
				u.deferProgressRow(ctx, r.ID)
				continue
			}
		case data.ProgressGrantItem:
			if u.granter == nil {
				u.deferProgressRow(ctx, r.ID) // inventory_addr 未配:积压不丢
				continue
			}
			key := progressIdempotencyKey(r.MatchID, r.Seq, r.PlayerID, "item")
			if gerr := u.granter.GrantInstances(ctx, r.PlayerID, r.ItemConfigIDs, key); gerr != nil {
				// 背包满且已配 mail → 转个人邮件(同键,直发链与邮件链至多一次),成功后删行。
				if u.mailSender != nil && errcode.As(gerr) == errcode.ErrInventoryCapacityFull {
					if merr := u.mailSender.SendOverflowMail(ctx, r.PlayerID, r.ItemConfigIDs, key); merr != nil {
						plog.With(ctx).Warnw("msg", "progress_overflow_mail_failed",
							"player_id", r.PlayerID, "items", len(r.ItemConfigIDs), "err", merr)
						u.deferProgressRow(ctx, r.ID)
						continue
					}
					plog.With(ctx).Infow("msg", "progress_overflow_mailed",
						"player_id", r.PlayerID, "items", len(r.ItemConfigIDs))
				} else {
					plog.With(ctx).Warnw("msg", "progress_item_grant_failed",
						"player_id", r.PlayerID, "items", len(r.ItemConfigIDs), "err", gerr)
					u.deferProgressRow(ctx, r.ID)
					continue
				}
			}
		default:
			// 未知类型行(未来扩展 / 脏数据):告警并退避推迟,不删(人工介入)。
			plog.With(ctx).Warnw("msg", "progress_outbox_unknown_kind", "id", r.ID, "kind", r.Kind)
			u.deferProgressRow(ctx, r.ID)
			continue
		}
		if derr := u.repo.DeleteProgressOutbox(ctx, r.ID); derr != nil {
			return granted, derr
		}
		granted++
	}
	if granted > 0 {
		plog.With(ctx).Infow("msg", "progress_outbox_granted", "count", granted)
	}
	return granted, nil
}

// deferProgressRow 推迟一条发放失败的出箱行(失败本身只告警:推迟失败下轮 Fetch 仍会
// 取到该行重试,不影响 at-least-once)。
func (u *BattleResultUsecase) deferProgressRow(ctx context.Context, id int64) {
	if err := u.repo.DeferProgressOutbox(ctx, id); err != nil {
		plog.With(ctx).Warnw("msg", "progress_outbox_defer_failed", "id", id, "err", err)
	}
}

// progressIdempotencyKey 组装进度发放幂等键:progress:{match_id}:{seq}:{player_id}:{kind}。
// 与 realtime-progression.md §3 / player.proto AddExperienceRequest 注释保持同一口径。
func progressIdempotencyKey(matchID, seq, playerID uint64, kind string) string {
	return "progress:" + strconv.FormatUint(matchID, 10) +
		":" + strconv.FormatUint(seq, 10) +
		":" + strconv.FormatUint(playerID, 10) +
		":" + kind
}
