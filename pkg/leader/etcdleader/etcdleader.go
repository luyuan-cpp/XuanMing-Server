// Package etcdleader 用 etcd 选举让一个后台单例任务只在一个副本上运行。
//
// 背景(docs/design/decision-revisit-matchmaker-single-writer.md):贪心批量撮合是"在一个
// 共享池上做全局优化",天然是单写者问题。多副本部署时,若每个副本都无条件跑撮合循环,会
// 在同一队列上重复成局(同一玩家进两场 match,违反不变量 §1)。本包用 etcd concurrency.Election
// 选出唯一 leader,仅当选副本跑循环,其余副本继续服务 RPC + 热备。
//
// 与 pkg/snowflake/etcdnode 的关键区别(失主语义):
//   - snowflake 失租必须 os.Exit(防同 nodeID 双活发号);
//   - 本包失去领导权**不退出进程**——只取消传给 run 的 ctx,让后台循环停下;本副本继续服务
//     RPC,新 leader 在 lease TTL 内接管。这正是不停机滚动更新(不变量 §16):任意副本可随时
//     被杀被替换,leader 自动转移。
//
// 用法(多副本阶段的服务 main.go):
//
//	go func() {
//	    err := etcdleader.Run(ctx, etcdleader.Config{
//	        Endpoints: cfg.Leader.EtcdEndpoints,
//	        Election:  "matchmaker/5v5_ranked/r1", // 分片键:mode/region,隔离不同部署
//	    }, uc.RunMatchLoop) // run 必须遵守 ctx.Done() 退出
//	    if err != nil && ctx.Err() == nil {
//	        log.Error("leader run failed", err)
//	    }
//	}()
//
// 单副本 / dev 不引入本包,直接 go uc.RunMatchLoop(ctx)。
package etcdleader

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	klog "github.com/go-kratos/kratos/v2/log"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

const (
	// DefaultPrefix 是选举 key 的前缀。实际 election key = <prefix><election>。
	DefaultPrefix = "/pandora/leader/"
	// DefaultLeaseTTLSec 是 session lease 默认 TTL(秒),与 infra.md Leader Election 一致。
	DefaultLeaseTTLSec = 15
	// DefaultDialTimeout 是 etcd 连接默认超时。
	DefaultDialTimeout = 5 * time.Second
	// reconnectBackoff 是重连 / 重竞选前的退避,避免 etcd 抖动时忙等。
	reconnectBackoff = 2 * time.Second
)

// Config 是选举器配置。
type Config struct {
	// Endpoints etcd 地址(必填)。
	Endpoints []string
	// Election 选举名(分片键),用于隔离不同的单例任务 / 不同分片。
	// 例:"matchmaker/5v5_ranked/r1"(mode/region)。必填。
	Election string
	// Prefix key 前缀,留空用 DefaultPrefix。
	Prefix string
	// LeaseTTLSec session lease TTL(秒),留空用 DefaultLeaseTTLSec。失主检测粒度 ≈ 此值。
	LeaseTTLSec int
	// DialTimeout etcd 连接超时,留空用 DefaultDialTimeout。
	DialTimeout time.Duration
}

// Run 竞选领导权,并在持有领导权期间运行 run(leaderCtx)。阻塞直到 parentCtx 取消。
//
// 语义:
//   - 未当选时阻塞在 Campaign,不占用资源;
//   - 当选 → 调 run(leaderCtx);leaderCtx 在失去领导权(session/lease 丢失)或 parentCtx 取消时取消;
//   - run 必须遵守 leaderCtx.Done() 及时返回(如 RunMatchLoop 的 select ctx.Done());
//   - 失去领导权后**不退出进程**,内部重连并重新竞选,交由本副本或他人接管;
//   - parentCtx 取消 → 主动 Resign 让位后返回 nil(正常下线路径)。
//
// 返回 error 仅表示"parentCtx 未取消但内部反复失败"的极端情形(当前实现总是重试到 parentCtx
// 取消,故正常返回 nil;签名保留 error 供未来扩展 fail-fast 策略)。
func Run(parentCtx context.Context, cfg Config, run func(ctx context.Context)) error {
	if len(cfg.Endpoints) == 0 {
		return fmt.Errorf("etcdleader: empty endpoints")
	}
	if cfg.Election == "" {
		return fmt.Errorf("etcdleader: empty election name")
	}
	prefix := cfg.Prefix
	if prefix == "" {
		prefix = DefaultPrefix
	}
	ttl := cfg.LeaseTTLSec
	if ttl <= 0 {
		ttl = DefaultLeaseTTLSec
	}
	dial := cfg.DialTimeout
	if dial <= 0 {
		dial = DefaultDialTimeout
	}
	electionKey := prefix + cfg.Election
	identity := holderIdentity()

	for parentCtx.Err() == nil {
		campaignAndServe(parentCtx, cfg.Endpoints, dial, ttl, electionKey, identity, run)
		if parentCtx.Err() != nil {
			return nil
		}
		// 失主 / 连接问题:退避后重连重竞选(不退出进程)。
		select {
		case <-parentCtx.Done():
			return nil
		case <-time.After(reconnectBackoff):
		}
	}
	return nil
}

// campaignAndServe 完成一轮"连 etcd → 建 session → 竞选 → 当选后跑 run → 失主/退出清理"。
// 任意步骤失败都返回(由 Run 退避重试),绝不 panic / 退出进程。
func campaignAndServe(
	parentCtx context.Context,
	endpoints []string,
	dial time.Duration,
	ttl int,
	electionKey, identity string,
	run func(ctx context.Context),
) {
	cli, err := clientv3.New(clientv3.Config{Endpoints: endpoints, DialTimeout: dial})
	if err != nil {
		klog.Warnf("[leader] dial etcd failed election=%s err=%v", electionKey, err)
		return
	}
	defer func() { _ = cli.Close() }()

	session, err := concurrency.NewSession(cli, concurrency.WithTTL(ttl))
	if err != nil {
		klog.Warnf("[leader] new session failed election=%s err=%v", electionKey, err)
		return
	}
	defer func() { _ = session.Close() }()

	election := concurrency.NewElection(session, electionKey)

	// Campaign 阻塞直到当选或 parentCtx 取消。用可取消 ctx 保证 parent 关闭时不卡死。
	if err := election.Campaign(parentCtx, identity); err != nil {
		if parentCtx.Err() == nil {
			klog.Warnf("[leader] campaign failed election=%s err=%v", electionKey, err)
		}
		return
	}
	if parentCtx.Err() != nil {
		return
	}
	klog.Infof("[leader] elected election=%s identity=%s ttl=%ds", electionKey, identity, ttl)

	// 当选:leaderCtx 在 parent 取消或 session(lease)丢失时取消,驱动 run 退出。
	leaderCtx, cancel := context.WithCancel(parentCtx)
	defer cancel()
	go func() {
		select {
		case <-session.Done():
			klog.Warnf("[leader] session lost election=%s — stepping down (process stays alive)", electionKey)
			cancel()
		case <-leaderCtx.Done():
		}
	}()

	// 阻塞运行后台单例任务,直到失去领导权 / 下线。
	run(leaderCtx)
	cancel()

	// 主动让位(best-effort),让新 leader 立即接管而非等 lease 过期。
	resignCtx, resignCancel := context.WithTimeout(context.Background(), dial)
	_ = election.Resign(resignCtx)
	resignCancel()
	klog.Infof("[leader] resigned election=%s", electionKey)
}

// holderIdentity 返回本副本在选举里的标识(hostname + pid),仅用于可观测 / 排障,
// 不参与正确性判断(正确性由 etcd session lease 保证)。
func holderIdentity() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return host + "-" + strconv.Itoa(os.Getpid())
}
