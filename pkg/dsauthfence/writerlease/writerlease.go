// Package writerlease 是「单活跃代际写者」继任租约(INC-20260722-004 R9 P0-7 收口;
// docs/design/session-generation-rollout.md §5)。
//
// 背景:hub_allocator 是 assignment/容量账本的单写者(dsauthfence V3 契约)。部署层
// Recreate 只能用「先杀旧再起新」保证单写者,滚动更新(RollingUpdate)会出现新旧两个
// 二进制并行的双写窗口。本包把单写者约束从部署策略下沉为运行时协议:
//
//   - 所有副本竞选同一个 etcd election(concurrency.Election,session lease TTL 内
//     自动失效);任一时刻至多一个副本持有领导权;
//   - 当选副本获得一个 fencing token = 本届 leader key 的 CreateRevision。etcd 选举
//     按 CreateRevision 最小者当选、删除后由次小者接任,因此**历届 leader 的 token
//     严格单调递增**(Chubby sequencer 语义);
//   - 业务写路径在入口检查 Current()(快速拒绝),存储层把 token 写进与业务事务同
//     slot 的 fence key 并做「只进不退」比较(迟到旧写者一旦落后于已推进的 fence
//     立即被拒,见 hub_allocator data 层 guardWriterFence);
//   - 失去领导权(session 过期/etcd 断连)**不退出进程**:Current() 立即转为不持有,
//     副本降级为热备并自动重新竞选。这正是 RollingUpdate 所需语义——新副本 Ready 后
//     旧副本收 SIGTERM 主动 Resign,新副本亚秒接任;崩溃场景由 lease TTL 兜底。
//
// 与 pkg/leader/etcdleader 的区别:etcdleader 是回调式(当选后跑一个循环),不暴露
// 持有权句柄与 fencing token;本包是句柄式,供 RPC 写路径逐请求查询。放在 dsauthfence
// module 子包:同属 DS 授权生产栅栏体系,且复用其 etcd client 依赖与 mTLS 安全构造,
// 业务服务(已依赖 dsauthfence)零新增 go.mod 条目。
package writerlease

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	klog "github.com/go-kratos/kratos/v2/log"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"

	"github.com/luyuancpp/pandora/pkg/dsauthfence"
)

const (
	// DefaultPrefix 是选举 key 前缀(与 dsauthfence required/capability、
	// pkg/leader/etcdleader 的 /pandora/leader/ 均隔离)。
	DefaultPrefix = "/pandora/writerlease/"
	// DefaultLeaseTTLSec 与 etcdleader/infra.md Leader Election 对齐:崩溃场景的
	// 最大接任延迟 ≈ 此值;正常滚动更新走主动 Resign,亚秒接任。
	DefaultLeaseTTLSec = 15
	// DefaultDialTimeout 是 etcd 连接默认超时。
	DefaultDialTimeout = 5 * time.Second
	// recampaignBackoff 是失主/出错后重新竞选前的退避,防 etcd 抖动忙等。
	recampaignBackoff = 2 * time.Second
	// resignTimeout 是让位/清理操作的独立超时(不继承已取消的父 ctx)。
	resignTimeout = 3 * time.Second
	// campaignErrEscalateAfter:连续竞选失败达此次数后日志从 Warn 升级 Error
	// (复审 P0-6:无限重试不能 fail-silent——长时间无写者必须可告警)。按默认
	// 2s 退避,15 次 ≈ 30s 无主,超过 lease TTL 兜底接任窗口即异常。
	campaignErrEscalateAfter = 15
)

// Config 是继任租约配置。
type Config struct {
	// Endpoints etcd 地址(必填;生产复用 ds_auth.fence.etcd_endpoints)。
	Endpoints []string
	// Election 选举名(必填),如 "hub_allocator/writer"。同一写者域必须全体副本一致。
	Election string
	// Identity 本副本标识,仅用于可观测/排障(正确性由 lease + fencing token 保证)。
	Identity string
	// Prefix 留空用 DefaultPrefix。
	Prefix string
	// LeaseTTLSec 留空用 DefaultLeaseTTLSec。
	LeaseTTLSec int
	// DialTimeout 留空用 DefaultDialTimeout。
	DialTimeout time.Duration
}

// Term 是一届领导任期:token 单调、失主通知、主动让位。
type Term interface {
	// Token 是本届 fencing token(历届严格单调递增,恒 >0)。
	Token() uint64
	// Lost 在任期失效(lease 过期/连接丢失)时关闭。
	Lost() <-chan struct{}
	// Resign 主动让位并释放本届资源(幂等)。
	Resign(ctx context.Context) error
}

// Backend 隔离 etcd 细节,允许用确定性 fake 覆盖竞选/失主/让位全部分支。
type Backend interface {
	// Campaign 阻塞直到当选(返回本届 Term)或 ctx 取消/出错。
	Campaign(ctx context.Context, identity string) (Term, error)
	Close() error
}

// Lease 是继任租约句柄。业务写路径逐请求调用 Current();失主时 Current() 立即转
// 不持有,进程保持存活并自动重新竞选(新任期 token 更大)。
type Lease struct {
	backend  Backend
	identity string

	// current:0 = 不持有;>0 = 当前任期 fencing token。etcd CreateRevision 恒 >0,
	// 0 可安全作哨兵。
	current atomic.Uint64

	// consecutiveCampaignErrs:连续竞选失败次数(当选即清零;复审 P0-6 可观测)。
	consecutiveCampaignErrs atomic.Uint64
	// lastCampaignErr:最近一次竞选失败原因(atomic.Value[string];当选清空)。
	lastCampaignErr atomic.Value

	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
}

// Health 返回竞选健康度快照(复审 P0-6:竞选无限重试不得 fail-silent,运维/探针
// 可轮询此接口把「长期无主」暴露为告警)。consecutiveErrs>0 且持续增长 = etcd
// 不可达/配置错误,lastErr 携带最近失败原因。
func (l *Lease) Health() (consecutiveErrs uint64, lastErr string) {
	if v, ok := l.lastCampaignErr.Load().(string); ok {
		lastErr = v
	}
	return l.consecutiveCampaignErrs.Load(), lastErr
}

// Current 返回 (fencing token, 是否持有领导权)。数据层把 token 写进同 slot fence key
// 做只进不退比较;biz 层用 held 快速拒绝非写者副本上的写请求。
func (l *Lease) Current() (uint64, bool) {
	token := l.current.Load()
	return token, token != 0
}

// Close 主动让位并停止竞选(进程下线路径)。幂等。
func (l *Lease) Close() error {
	l.closeOnce.Do(func() {
		l.cancel()
		<-l.done
		_ = l.backend.Close()
	})
	return nil
}

// Start 连接 etcd 并启动竞选循环,立即返回(领导权异步获得,调用方以 Current() 判定)。
// 连接/配置错误 fail-fast 返回 error;竞选期出错内部退避重试,绝不退出进程。
func Start(ctx context.Context, cfg Config) (*Lease, error) {
	if len(cfg.Endpoints) == 0 {
		return nil, errors.New("writerlease: empty endpoints")
	}
	if cfg.Election == "" {
		return nil, errors.New("writerlease: empty election name")
	}
	normalize(&cfg)
	cli, err := dsauthfence.DialSecureEtcdClient(cfg.Endpoints, cfg.DialTimeout, cfg.Prefix)
	if err != nil {
		return nil, err
	}
	backend := &etcdBackend{cli: cli, electionKey: cfg.Prefix + cfg.Election, ttlSec: cfg.LeaseTTLSec}
	return StartWithBackend(ctx, backend, cfg), nil
}

// StartWithBackend 使用已构造 Backend 启动竞选循环(生产走 Start;测试注入 fake)。
func StartWithBackend(ctx context.Context, backend Backend, cfg Config) *Lease {
	normalize(&cfg)
	runCtx, cancel := context.WithCancel(ctx)
	l := &Lease{backend: backend, identity: cfg.Identity, cancel: cancel, done: make(chan struct{})}
	go l.run(runCtx, cfg.Election)
	return l
}

func normalize(cfg *Config) {
	if cfg.Prefix == "" {
		cfg.Prefix = DefaultPrefix
	}
	if cfg.LeaseTTLSec <= 0 {
		cfg.LeaseTTLSec = DefaultLeaseTTLSec
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = DefaultDialTimeout
	}
	if cfg.Identity == "" {
		cfg.Identity = "unknown"
	}
}

func (l *Lease) run(ctx context.Context, election string) {
	defer close(l.done)
	for ctx.Err() == nil {
		term, err := l.backend.Campaign(ctx, l.identity)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// 复审 P0-6:连续失败计数 + 升级日志。重试本身不变(热备语义),但长期
			// 无主必须从 Warn 升级 Error 以触发日志告警;Health() 供探针/运维轮询。
			fails := l.consecutiveCampaignErrs.Add(1)
			l.lastCampaignErr.Store(err.Error())
			if fails >= campaignErrEscalateAfter {
				klog.Errorf("[writerlease] campaign failing persistently election=%s identity=%s consecutive=%d err=%v — no writer may be active, check etcd connectivity/config",
					election, l.identity, fails, err)
			} else {
				klog.Warnf("[writerlease] campaign failed election=%s identity=%s consecutive=%d err=%v", election, l.identity, fails, err)
			}
			if !sleepCtx(ctx, recampaignBackoff) {
				return
			}
			continue
		}
		l.consecutiveCampaignErrs.Store(0)
		l.lastCampaignErr.Store("")
		l.current.Store(term.Token())
		klog.Infof("[writerlease] elected election=%s identity=%s token=%d", election, l.identity, term.Token())
		select {
		case <-term.Lost():
			// 失主:先撤销本地持有权(Current() 立即转不持有,快速挡住后续写入口),
			// 再 best-effort 清理旧任期;存储层 fence 兜住撤销瞬间仍在途的迟到写。
			l.current.Store(0)
			klog.Warnf("[writerlease] term lost election=%s identity=%s token=%d — stepping down (process stays alive)",
				election, l.identity, term.Token())
			resignTerm(term)
		case <-ctx.Done():
			l.current.Store(0)
			resignTerm(term)
			klog.Infof("[writerlease] resigned election=%s identity=%s (shutdown)", election, l.identity)
			return
		}
		if !sleepCtx(ctx, recampaignBackoff) {
			return
		}
	}
}

func resignTerm(term Term) {
	rctx, cancel := context.WithTimeout(context.Background(), resignTimeout)
	defer cancel()
	_ = term.Resign(rctx)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// ── etcd Backend ─────────────────────────────────────────────────────────────

type etcdBackend struct {
	cli         *clientv3.Client
	electionKey string
	ttlSec      int
}

// Campaign 每届新建 session(lease)+ election,当选后 token = 本届 leader key 的
// CreateRevision(concurrency.Election.Rev())。选举按 CreateRevision 排队接任,
// 历届 token 严格单调递增。
func (b *etcdBackend) Campaign(ctx context.Context, identity string) (Term, error) {
	session, err := concurrency.NewSession(b.cli, concurrency.WithTTL(b.ttlSec), concurrency.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	election := concurrency.NewElection(session, b.electionKey)
	if err := election.Campaign(ctx, identity); err != nil {
		_ = session.Close()
		return nil, err
	}
	rev := election.Rev()
	if rev <= 0 {
		// 防御:当选后 leaderRev 理应恒 >0;异常时绝不能以 token=0(哨兵)冒充持有。
		_ = session.Close()
		return nil, errors.New("writerlease: elected but leader key revision is not positive")
	}
	return &etcdTerm{session: session, election: election, token: uint64(rev)}, nil
}

func (b *etcdBackend) Close() error { return b.cli.Close() }

type etcdTerm struct {
	session    *concurrency.Session
	election   *concurrency.Election
	token      uint64
	resignOnce sync.Once
}

func (t *etcdTerm) Token() uint64         { return t.token }
func (t *etcdTerm) Lost() <-chan struct{} { return t.session.Done() }
func (t *etcdTerm) Resign(ctx context.Context) error {
	var err error
	t.resignOnce.Do(func() {
		err = t.election.Resign(ctx)
		_ = t.session.Close()
	})
	return err
}
