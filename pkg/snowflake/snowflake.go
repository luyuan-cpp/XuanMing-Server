// Package snowflake 提供 64 位全局唯一 ID 生成器。
//
// Layout: [time:32][node:17][step:15]
// Epoch:  2026-06-11 14:59:25 UTC (1781161165)
//
// 实现为无锁 CAS:把 (lastTime, step) 打包进单个 atomic 状态字
// (布局与 ID 本身一致),Generate 的临界区只有一条 CompareAndSwap。
// 线程安全;同一节点产出严格单调递增。
//
// 容量上界:秒级时间戳 + 15 bit step = 每节点每秒最多 32768 个 ID,
// 超出后阻塞等待下一秒。
package snowflake

import (
	"fmt"
	"sync/atomic"
	"time"
)

const (
	Epoch    uint64 = 1781161165
	NodeBits uint64 = 17
	StepBits uint64 = 15

	timeShift = NodeBits + StepBits
	nodeShift = StepBits
	stepMask  = (1 << StepBits) - 1
	NodeMask  = (1 << NodeBits) - 1
)

// Node 是单一节点的 ID 生成器。线程安全(无锁 CAS)。
type Node struct {
	nodeShifted uint64        // 预移位好的 node 段,免去每次 Generate 重复移位
	state       atomic.Uint64 // 上一次发出的 ID:[lastTime:32][node:17][step:15]
}

// NewNode 创建一个 SnowFlake 生成器。nodeID 超过 17 位会 panic。
func NewNode(nodeID uint64) *Node {
	if nodeID > NodeMask {
		panic(fmt.Sprintf("snowflake: node ID %d exceeds max %d", nodeID, NodeMask))
	}
	n := &Node{nodeShifted: nodeID << nodeShift}
	n.state.Store(n.nodeShifted) // lastTime=0, step=0
	return n
}

// Generate 产生一个全局唯一、严格单调递增的 uint64 ID。
func (n *Node) Generate() uint64 {
	for {
		old := n.state.Load()
		lastTime := old >> timeShift
		now := nowEpoch()

		var next uint64
		switch {
		case now > lastTime:
			// 时钟前进:换新秒,step 归零
			next = now<<timeShift | n.nodeShifted
		case old&stepMask < stepMask:
			// 同一秒(或时钟回拨):继续消费 lastTime 秒剩余的 step 池,
			// 回拨期间无需自旋等待,单调性由 old+1 保证
			next = old + 1
		default:
			// 当前秒 step 耗尽:阻塞等真实时钟越过 lastTime 后重试
			n.waitNextTime(lastTime)
			continue
		}

		// 两条路径均满足 next > old,CAS 成功即独占该 ID:
		// 唯一性与严格单调性同时成立
		if n.state.CompareAndSwap(old, next) {
			return next
		}
		// CAS 失败说明并发竞争,重读状态再来
	}
}

// waitNextTime 阻塞直到真实时钟越过 last(秒级粒度,sleep 1ms 轮询)。
func (n *Node) waitNextTime(last uint64) {
	for nowEpoch() <= last {
		time.Sleep(time.Millisecond)
	}
}

func nowEpoch() uint64 {
	ts := unixNow()
	if ts < int64(Epoch) {
		// 时钟早于 Epoch 时 uint64 减法会下溢出垃圾时间位,
		// 产出的 ID 会与历史 ID 冲突——宁可 panic 也不能发错 ID
		panic(fmt.Sprintf("snowflake: system clock %d is before epoch %d (%s)",
			ts, Epoch, time.Unix(int64(Epoch), 0).UTC().Format(time.RFC3339)))
	}
	return uint64(ts) - Epoch
}

// MinIDAt 返回 unixSec 时刻(Unix 秒)可能生成的最小 ID:时间段取该秒、node/step 全零。
// 用途:按"创建时间早于 cutoff"做范围清理(如 player_mail_claim 按 mail_id < MinIDAt(cutoff)
// 批量删),把时间条件转成 ID 范围条件,走主键/索引扫描。
// unixSec 早于 Epoch 时返回 0(全部 ID 都晚于该时刻)。
func MinIDAt(unixSec int64) uint64 {
	if unixSec < int64(Epoch) {
		return 0
	}
	return (uint64(unixSec) - Epoch) << timeShift
}

// unixNow 返回当前 Unix 秒。抽成包级变量仅为便于测试注入虚拟时钟
// (绕开 32768 ID/s 的真实容量墙做大规模并发验证);生产路径恒等于
// time.Now().Unix()。
var unixNow = func() int64 { return time.Now().Unix() }
