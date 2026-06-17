// Package killswitch 提供 Pandora 服务的 RPC 级临时关停(Kill-Switch)能力。
//
// 解决的问题:某个 service 出重大问题、或某个 RPC 有 bug,想「临时关掉、修好再开」,
// 不发版、不重启、秒级热生效、可回滚、fail-open。这是大厂第 3 层(功能开关 / Kill-Switch)
// 的标准做法,比整服下线(第 2 层)更细、比 Envoy 网关挡流(第 1 层)更贴近业务。
//
// 三级粒度(用一套 key 通配统一,不做三套机制):
//
//	单 RPC   :"pandora.match.v1.MatchService/StartMatch"
//	整服      :"pandora.match.v1.MatchService/*"
//	功能/玩法  :"feature/<name>"(需先用 RegisterFeature 把一组 RPC 归到该 feature)
//	全局维护   :"*"(关掉所有 RPC,慎用)
//
// 开关源(Source)可插拔,走「driver 注册」模式(类似 database/sql):
//
//	file —— 本包内置(fsnotify 监听 yaml,改文件即生效,dev / 无 etcd 降级用)
//	etcd —— 由独立子包 pkg/killswitch/etcdkv 提供(避免核心包硬依赖重型 etcd client);
//	        服务在 main 里 blank import `_ ".../pkg/killswitch/etcdkv"` 即可启用。
//
// fail-open 铁律:开关源不可用 / 未初始化 / 未注册 → 一律放行,
// 绝不因 Kill-Switch 自身故障把全服打死。
//
// 装配:pkg/svc.BaseContext 在 MustNewBaseContext 里按配置调用 Setup,全服复用。
// 拦截:pkg/middleware.KillSwitch() 在 gRPC server 默认 middleware 链里查 Default。
package killswitch

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	klog "github.com/go-kratos/kratos/v2/log"
)

// Config 是 Kill-Switch 的运行配置(由 pkg/config.KillSwitchConf 翻译而来,避免跨包耦合)。
type Config struct {
	// Enabled 为 false 时 Setup 直接清空 Default(全放行),不构造任何源。
	Enabled bool

	// Source 选择开关源:"file" / "etcd"。留空默认 "file"。
	Source string

	// FilePath 是 file 源监听的 yaml 路径。
	FilePath string

	// EtcdEndpoints / EtcdPrefix / EtcdDialTimeout 给 etcd 源用(见 etcdkv 子包)。
	EtcdEndpoints   []string
	EtcdPrefix      string
	EtcdDialTimeout time.Duration

	// FailOpen 控制源构造失败时的行为:true(默认)= 放行不报错;false = Setup 返回错误。
	FailOpen bool
}

// Manager 持有当前规则快照并实现匹配。各开关源(file/etcd)拥有一个 Manager,
// 监测到规则变化时调用 Replace 原子换上新快照。读路径(Disabled)无锁。
type Manager struct {
	// rules 指向 map[normalizedPattern]reason 的不可变快照,原子替换。
	rules atomic.Pointer[map[string]string]
}

// NewManager 构造空 Manager(默认全放行)。
func NewManager() *Manager {
	m := &Manager{}
	empty := map[string]string{}
	m.rules.Store(&empty)
	return m
}

// Replace 原子换上新的规则快照。源在每次 reload / watch 事件后调用。
// 传入的 map 会被本包接管(调用方不应再改),nil 视为空集。
func (m *Manager) Replace(rules map[string]string) {
	if rules == nil {
		rules = map[string]string{}
	}
	m.rules.Store(&rules)
}

// Disabled 判断给定 operation(Kratos transport.Operation(),形如
// "/pandora.login.v1.LoginService/Login")当前是否被关停。
//
// 匹配优先级:全局 "*" → 精确 method → 整服 "<service>/*" → feature 组。
// 命中返回 (true, reason);未命中返回 (false, "")。
func (m *Manager) Disabled(operation string) (bool, string) {
	if m == nil {
		return false, ""
	}
	rp := m.rules.Load()
	if rp == nil || len(*rp) == 0 {
		return false, ""
	}
	rules := *rp
	op := strings.TrimPrefix(operation, "/")

	// 1. 全局维护开关
	if r, ok := rules["*"]; ok {
		return true, reasonOr(r, "all RPC temporarily disabled")
	}
	// 2. 精确 method
	if r, ok := rules[op]; ok {
		return true, reasonOr(r, "RPC temporarily disabled: "+op)
	}
	// 3. 整服通配 "<service>/*"
	if i := strings.LastIndex(op, "/"); i > 0 {
		svc := op[:i]
		if r, ok := rules[svc+"/*"]; ok {
			return true, reasonOr(r, "service temporarily disabled: "+svc)
		}
	}
	// 4. feature 组:被关的 feature key 形如 "feature/<name>",
	//    展开成该 feature 注册的 operation 列表再比对。
	for key, reason := range rules {
		name, ok := strings.CutPrefix(key, "feature/")
		if !ok {
			continue
		}
		if featureContains(name, op) {
			return true, reasonOr(reason, "feature temporarily disabled: "+name)
		}
	}
	return false, ""
}

func reasonOr(r, fallback string) string {
	if strings.TrimSpace(r) == "" {
		return fallback
	}
	return r
}

// ─────────────────────────────── 全局单例 ───────────────────────────────

// defaultManager 是被 middleware 查询的全局 Manager;nil = 未装配 = 全放行(fail-open)。
var defaultManager atomic.Pointer[Manager]

// SetDefault 设置全局 Manager(Setup 内部调用;传 nil 表示禁用 Kill-Switch 全放行)。
func SetDefault(m *Manager) {
	defaultManager.Store(m)
}

// Default 返回当前全局 Manager(可能为 nil)。
func Default() *Manager {
	return defaultManager.Load()
}

// Disabled 是包级便捷查询,middleware 用它。Default 为 nil 时 fail-open 放行。
func Disabled(operation string) (bool, string) {
	m := defaultManager.Load()
	if m == nil {
		return false, ""
	}
	return m.Disabled(operation)
}

// ─────────────────────────────── feature 注册 ───────────────────────────────

var (
	featureMu     sync.RWMutex
	featureGroups = map[string]map[string]struct{}{} // name → set(normalized operation)
)

// RegisterFeature 把一组 operation 归入名为 name 的 feature 组。
// operation 用规范化形式(不带前导 "/"),例如:
//
//	killswitch.RegisterFeature("match",
//	    "pandora.match.v1.MatchService/StartMatch",
//	    "pandora.match.v1.MatchService/ConfirmMatch",
//	)
//
// 之后只要把开关 "feature/match" 打开(置位),这组 RPC 一起被关停。
// 可在各服务 main 的 init / 装配处调用,重复 name 会合并而非覆盖。
func RegisterFeature(name string, operations ...string) {
	featureMu.Lock()
	defer featureMu.Unlock()
	set := featureGroups[name]
	if set == nil {
		set = map[string]struct{}{}
		featureGroups[name] = set
	}
	for _, op := range operations {
		set[strings.TrimPrefix(op, "/")] = struct{}{}
	}
}

func featureContains(name, op string) bool {
	featureMu.RLock()
	defer featureMu.RUnlock()
	set, ok := featureGroups[name]
	if !ok {
		return false
	}
	_, ok = set[op]
	return ok
}

// ─────────────────────────────── 源注册与装配 ───────────────────────────────

// Source 是一个已启动的开关源,持有 Manager 并能优雅关闭。
type Source interface {
	Manager() *Manager
	Close() error
}

// Builder 按配置构造并启动一个 Source。源应在返回前完成首次全量加载。
type Builder func(Config) (Source, error)

var (
	builderMu sync.RWMutex
	builders  = map[string]Builder{}
)

// RegisterSource 注册一个开关源构造器(driver 模式)。
// file 源在本包 init 注册;etcd 源在 etcdkv 子包 init 注册(blank import 启用)。
func RegisterSource(name string, b Builder) {
	builderMu.Lock()
	defer builderMu.Unlock()
	builders[name] = b
}

func lookupSource(name string) (Builder, bool) {
	builderMu.RLock()
	defer builderMu.RUnlock()
	b, ok := builders[name]
	return b, ok
}

// noopSource 是禁用 / fail-open 时返回的空源。
type noopSource struct{}

func (noopSource) Manager() *Manager { return nil }
func (noopSource) Close() error      { return nil }

// Setup 按配置构造开关源并设为全局 Default,返回的 Source 需在进程退出时 Close。
//
// 行为:
//   - Enabled=false        → 清空 Default(全放行),返回 noop。
//   - Source 未注册        → fail-open(Warn + 全放行),返回 noop;不报错(避免漏 blank import 把服务拖垮)。
//   - 构造失败 & FailOpen  → Warn + 全放行,返回 noop,err=nil。
//   - 构造失败 & !FailOpen → 返回 err(由调用方决定 fatal)。
func Setup(cfg Config) (Source, error) {
	if !cfg.Enabled {
		SetDefault(nil)
		klog.Infof("[killswitch] disabled (enabled=false), all RPC pass through")
		return noopSource{}, nil
	}
	if cfg.Source == "" {
		cfg.Source = "file"
	}
	b, ok := lookupSource(cfg.Source)
	if !ok {
		SetDefault(nil)
		klog.Warnf("[killswitch] source %q not registered (missing blank import?), fail-open: all RPC pass through", cfg.Source)
		return noopSource{}, nil
	}
	src, err := b(cfg)
	if err != nil {
		if cfg.FailOpen {
			SetDefault(nil)
			klog.Warnf("[killswitch] source %q build failed (fail-open, all RPC pass through): %v", cfg.Source, err)
			return noopSource{}, nil
		}
		return nil, err
	}
	SetDefault(src.Manager())
	klog.Infof("[killswitch] ready source=%s", cfg.Source)
	return src, nil
}
