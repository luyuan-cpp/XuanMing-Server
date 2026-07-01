// Package etcdnode 用 etcd Lease 自动分配 snowflake 的 nodeID。
//
// 背景(docs/design/infra.md §8.1):静态 node.node_id 在单副本 / dev 下够用,但进入
// k8s 多副本动态扩缩后,同一服务跑 N 个 pod,人工排号必然撞号 → 发重复 ID。本包用 etcd
// Lease 在 [0, MaxNodeID) 区间里**抢占一个独占 nodeID**,并以 KeepAlive 续租维持独占权。
//
// ⚠️ fencing 契约(必须遵守,否则同 nodeID 双活发重号):
//   - etcd Lease 是 nodeID 独占权的**事实来源**;KeepAlive 不是普通健康检查,而是独占权信号。
//   - 一旦 KeepAlive channel 关闭 / 续租失败 / lease 被 revoke,Holder.Lost() 会被关闭。
//   - 调用方(main.go)**必须** select Lost(),收到信号后立即停止发号并主动退出进程,
//     不能只打日志继续 Generate——否则与领走同 nodeID 的新 holder 形成双活。
//
// 用法(进入 k8s 多副本阶段的服务 main.go):
//
//	holder, err := etcdnode.Acquire(ctx, etcdnode.Config{
//	    Endpoints: cfg.Snowflake.EtcdEndpoints,
//	    Service:   "matchmaker",
//	})
//	if err != nil { log.Fatal(err) }
//	defer holder.Close()
//	sf := holder.Node() // 直接当 *snowflake.Node 用
//
//	go func() {
//	    <-holder.Lost()
//	    log.Error("snowflake nodeID lease lost, exiting to avoid dual-active")
//	    os.Exit(1) // 停止发号并退出,交给 k8s 重新拉起重新抢号
//	}()
//
// 单副本 / dev 仍走 snowflake.NewNode(cfg.Node.NodeId),不引入本包。
package etcdnode

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	klog "github.com/go-kratos/kratos/v2/log"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/luyuancpp/pandora/pkg/snowflake"
)

const (
	// DefaultPrefix 是 nodeID 注册 key 的前缀。实际 key = <prefix><service>/<id>。
	DefaultPrefix = "/pandora/snowflake/node/"
	// DefaultLeaseTTLSec 是 lease 默认 TTL(秒)。15s 与 docs/design/infra.md §8.1 一致。
	DefaultLeaseTTLSec = 15
	// DefaultDialTimeout 是 etcd 连接默认超时。
	DefaultDialTimeout = 5 * time.Second
)

// Config 是 nodeID 自动分配的配置。
type Config struct {
	// Endpoints etcd 地址(必填)。
	Endpoints []string
	// Service 服务名,用于 key 命名空间隔离:不同服务各自一套 [0, MaxNodeID) 空间。
	// 留空回退 "default"。
	Service string
	// Prefix key 前缀,留空用 DefaultPrefix。
	Prefix string
	// LeaseTTLSec lease TTL(秒),留空用 DefaultLeaseTTLSec。
	LeaseTTLSec int64
	// DialTimeout etcd 连接超时,留空用 DefaultDialTimeout。
	DialTimeout time.Duration
	// MaxNodeID 候选 nodeID 上界(exclusive)。留空用 snowflake.NodeMask+1(即 17bit 全空间)。
	// 同一服务的副本数不会超过该上界,否则 Acquire 会返回 ErrNoFreeNode。
	MaxNodeID uint64
}

// ErrNoFreeNode 表示 [0, MaxNodeID) 区间已被占满,没有空闲 nodeID 可抢。
var ErrNoFreeNode = fmt.Errorf("etcdnode: no free node id in range")

// Holder 持有一个抢占成功的 nodeID + 其 etcd lease。
type Holder struct {
	node    *snowflake.Node
	nodeID  uint64
	key     string
	cli     *clientv3.Client
	leaseID clientv3.LeaseID

	lost        chan struct{}
	lostOnce    sync.Once
	intentional atomic.Bool // Close() 主动关闭时置位,避免误报 Lost
	cancel      context.CancelFunc
	closeOnce   sync.Once
}

// Acquire 连接 etcd,在 [0, MaxNodeID) 里抢占一个独占 nodeID,启动 KeepAlive 续租。
//
// 成功返回的 Holder:
//   - Node() 是绑定该 nodeID 的 *snowflake.Node;
//   - Lost() 在续租失败 / lease 丢失时关闭,调用方必须据此停发并退出;
//   - Close() 释放 lease 并停止续租(进程正常退出时调用)。
func Acquire(ctx context.Context, cfg Config) (*Holder, error) {
	if len(cfg.Endpoints) == 0 {
		return nil, fmt.Errorf("etcdnode: empty endpoints")
	}
	prefix := cfg.Prefix
	if prefix == "" {
		prefix = DefaultPrefix
	}
	service := cfg.Service
	if service == "" {
		service = "default"
	}
	ttl := cfg.LeaseTTLSec
	if ttl <= 0 {
		ttl = DefaultLeaseTTLSec
	}
	dial := cfg.DialTimeout
	if dial <= 0 {
		dial = DefaultDialTimeout
	}
	maxNode := cfg.MaxNodeID
	if maxNode == 0 || maxNode > snowflake.NodeMask+1 {
		maxNode = snowflake.NodeMask + 1
	}

	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   cfg.Endpoints,
		DialTimeout: dial,
	})
	if err != nil {
		return nil, fmt.Errorf("etcdnode: dial etcd: %w", err)
	}

	// 1. Grant lease。
	grantCtx, cancelGrant := context.WithTimeout(ctx, dial)
	lease, err := cli.Grant(grantCtx, ttl)
	cancelGrant()
	if err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("etcdnode: grant lease: %w", err)
	}

	keyPrefix := strings.TrimSuffix(prefix, "/") + "/" + service + "/"
	value := holderIdentity()

	// 2. 顺序扫描候选 nodeID,事务抢占第一个空闲的。
	var (
		acquiredID  uint64
		acquiredKey string
		acquired    bool
	)
	for id := uint64(0); id < maxNode; id++ {
		key := keyPrefix + strconv.FormatUint(id, 10)
		txnCtx, cancelTxn := context.WithTimeout(ctx, dial)
		resp, txnErr := cli.Txn(txnCtx).
			// key 不存在(CreateRevision==0)才写,实现独占抢占。
			If(clientv3.Compare(clientv3.CreateRevision(key), "=", 0)).
			Then(clientv3.OpPut(key, value, clientv3.WithLease(lease.ID))).
			Commit()
		cancelTxn()
		if txnErr != nil {
			// 单个 key 抢占失败(网络抖动),继续试下一个;不直接放弃整次 Acquire。
			klog.Warnf("[snowflake] etcdnode txn id=%d err=%v", id, txnErr)
			continue
		}
		if resp.Succeeded {
			acquiredID, acquiredKey, acquired = id, key, true
			break
		}
	}

	if !acquired {
		_, _ = cli.Revoke(context.Background(), lease.ID)
		_ = cli.Close()
		return nil, ErrNoFreeNode
	}

	h := &Holder{
		node:    snowflake.NewNode(acquiredID),
		nodeID:  acquiredID,
		key:     acquiredKey,
		cli:     cli,
		leaseID: lease.ID,
		lost:    make(chan struct{}),
	}

	// 3. 启动 KeepAlive 续租。channel 关闭 = 租约丢失 → 触发 Lost。
	kaCtx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	kaCh, err := cli.KeepAlive(kaCtx, lease.ID)
	if err != nil {
		cancel()
		_, _ = cli.Revoke(context.Background(), lease.ID)
		_ = cli.Close()
		return nil, fmt.Errorf("etcdnode: keepalive: %w", err)
	}
	go h.keepAliveLoop(kaCh)

	klog.Infof("[snowflake] etcdnode acquired node_id=%d service=%s lease=%x ttl=%ds",
		acquiredID, service, lease.ID, ttl)
	return h, nil
}

// keepAliveLoop 持续消费 KeepAlive 确认;channel 关闭即视为租约丢失,触发 Lost。
func (h *Holder) keepAliveLoop(kaCh <-chan *clientv3.LeaseKeepAliveResponse) {
	for range kaCh {
		// 仅需 drain 续租确认;丢一两拍由 etcd 自身重试,channel 不关就仍持有独占权。
	}
	// channel 关闭:要么 Close() 主动取消(intentional),要么真丢租约。
	if h.intentional.Load() {
		return
	}
	klog.Errorf("[snowflake] etcdnode lease LOST node_id=%d key=%s — caller must stop generating and exit",
		h.nodeID, h.key)
	h.signalLost()
}

func (h *Holder) signalLost() {
	h.lostOnce.Do(func() { close(h.lost) })
}

// Node 返回绑定该 nodeID 的雪花生成器。
func (h *Holder) Node() *snowflake.Node { return h.node }

// NodeID 返回抢占到的 nodeID。
func (h *Holder) NodeID() uint64 { return h.nodeID }

// Lost 在 lease 丢失时关闭。调用方必须 select 它,收到后停止发号并退出进程。
func (h *Holder) Lost() <-chan struct{} { return h.lost }

// Close 主动释放 lease 并停止续租(进程正常退出路径)。幂等。
func (h *Holder) Close() error {
	var err error
	h.closeOnce.Do(func() {
		h.intentional.Store(true)
		if h.cancel != nil {
			h.cancel()
		}
		// 主动 revoke,让 nodeID 立刻可被其他副本复用,不必等 TTL 过期。
		revokeCtx, cancel := context.WithTimeout(context.Background(), DefaultDialTimeout)
		_, err = h.cli.Revoke(revokeCtx, h.leaseID)
		cancel()
		err2 := h.cli.Close()
		if err == nil {
			err = err2
		}
	})
	return err
}

// holderIdentity 组装 lease value,便于运维排查"这个 nodeID 现在被谁占着"。
func holderIdentity() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("host=%s pid=%d ts=%d", host, os.Getpid(), time.Now().Unix())
}
