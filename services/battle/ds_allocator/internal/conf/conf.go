// Package conf 是 ds_allocator 服务的私有配置结构。
package conf

import (
	"time"

	"github.com/luyuancpp/pandora/pkg/config"
)

// Config 是 ds_allocator 服务的完整配置。
type Config struct {
	config.Base `yaml:",inline" mapstructure:",squash"`

	Allocator AllocatorConf `yaml:"allocator" json:"allocator"`
	Agones    AgonesConf    `yaml:"agones" json:"agones"`
	LocalDS   LocalDSConf   `yaml:"local_ds" json:"local_ds"`
}

// LocalDSConf 是「本机拉起 Windows Dedicated Server 进程」的调试后端配置。
//
// 这是与 Agones(Linux 生产)并列的第二种 DS 启动方式,专供本机联调:匹配成局后
// ds_allocator 直接 exec 打包好的 UE Windows DS 可执行文件,分配一个本机端口,返回
// 真实地址(host:port)给客户端 NetDriver 连入;Release / 心跳超时 abandoned 时 Kill 进程。
//
// 三种 DS 启动方式互斥,按 main.go 优先级选装配:
//   - agones.enabled=true   → AgonesGameServerAllocator(Linux 生产)
//   - local_ds.enabled=true → LocalGameServerAllocator(本机 Windows 调试,本结构)
//   - 都为 false            → MockGameServerAllocator(确定性假地址,无真实 DS)
//
// agones.enabled 与 local_ds.enabled 不可同时为 true(main.go 会 fatal)。
type LocalDSConf struct {
	// Enabled 打开本机拉起 Windows DS 进程(默认 false)。
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`

	// ExecutablePath 打包好的 UE Windows Dedicated Server 可执行文件绝对路径
	// (例如 C:\work\Pandora-Client-SVN\...\PandoraServer.exe)。Enabled=true 时必填且必须存在。
	ExecutablePath string `yaml:"executable_path,omitempty" json:"executable_path,omitempty"`

	// MapName 启动时加载的 UE 关卡(DS 命令行首个位置参数,例如 /Game/Maps/BattleMap)。
	// 留空则不带关卡参数,由 DS 自身默认关卡决定。
	MapName string `yaml:"map_name,omitempty" json:"map_name,omitempty"`

	// AdvertiseHost 返回给客户端的可连接 host(默认 127.0.0.1,本机联调)。
	AdvertiseHost string `yaml:"advertise_host,omitempty" json:"advertise_host,omitempty"`

	// PortBase 分配给 DS 进程的端口基址(默认 7777)。
	PortBase int `yaml:"port_base,omitempty" json:"port_base,omitempty"`

	// PortRange 端口池大小(默认 100),实际端口在 [PortBase, PortBase+PortRange) 内取空闲。
	PortRange int `yaml:"port_range,omitempty" json:"port_range,omitempty"`

	// WorkingDir DS 进程工作目录(留空用 ds_allocator 当前目录)。
	WorkingDir string `yaml:"working_dir,omitempty" json:"working_dir,omitempty"`

	// LogDir DS 进程 stdout/stderr 落盘目录(默认 run/dev/logs/ds);每进程一个 <pod>.log。
	LogDir string `yaml:"log_dir,omitempty" json:"log_dir,omitempty"`

	// ExtraArgs 追加到 DS 命令行末尾的额外参数(例如后端 gRPC-Web 入口地址覆盖)。
	ExtraArgs []string `yaml:"extra_args,omitempty" json:"extra_args,omitempty"`

	// ExtraEnv 注入 DS 进程的额外环境变量(在 PANDORA_MATCH_ID 等内置变量之后追加)。
	ExtraEnv map[string]string `yaml:"extra_env,omitempty" json:"extra_env,omitempty"`
}

// AgonesConf 是真 Agones GameServerAllocation 后端配置(W4 ⑫)。
//
// Enabled=false(默认)→ 用 MockGameServerAllocator;Enabled=true → 用
// AgonesGameServerAllocator(经 k8s apiserver REST 调 allocation.agones.dev/v1
// GameServerAllocation,provider 无关:ACK / 自建 / minikube 上跑的 Agones 都一致)。
//
// 集群内运行时 token_path / ca_path / api_server / namespace 留空即用 in-cluster 默认;
// 集群外联调可显式指定 api_server + token_path(或经 kubectl proxy 不带 token)。
type AgonesConf struct {
	// Enabled 打开真 Agones 分配(默认 false → Mock)。
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`

	// APIServer k8s apiserver 地址(默认 https://kubernetes.default.svc,in-cluster)。
	APIServer string `yaml:"api_server,omitempty" json:"api_server,omitempty"`

	// Namespace GameServerAllocation / GameServer 所在命名空间(默认 default)。
	Namespace string `yaml:"namespace,omitempty" json:"namespace,omitempty"`

	// FleetName 选择 GameServer 的 Fleet 名(selector agones.dev/fleet=<FleetName>)。
	// Enabled=true 时必填,否则构造失败。
	FleetName string `yaml:"fleet_name,omitempty" json:"fleet_name,omitempty"`

	// TokenPath ServiceAccount bearer token 文件路径
	// (默认 /var/run/secrets/kubernetes.io/serviceaccount/token;留 "-" 显式禁用 token)。
	TokenPath string `yaml:"token_path,omitempty" json:"token_path,omitempty"`

	// CAPath apiserver CA 证书路径
	// (默认 /var/run/secrets/kubernetes.io/serviceaccount/ca.crt)。
	CAPath string `yaml:"ca_path,omitempty" json:"ca_path,omitempty"`

	// InsecureSkipTLSVerify 跳过 apiserver TLS 校验(仅 dev,生产禁用)。
	InsecureSkipTLSVerify bool `yaml:"insecure_skip_tls_verify,omitempty" json:"insecure_skip_tls_verify,omitempty"`

	// AllocateTimeout 单次 allocate / release REST 调用超时(默认 5s)。
	AllocateTimeout config.Duration `yaml:"allocate_timeout,omitempty" json:"allocate_timeout,omitempty"`
}

// AllocatorConf 是 ds_allocator 服务私有配置。
type AllocatorConf struct {
	// HeartbeatTimeout DS 心跳超时阈值(默认 15s,不变量 §4)。
	// 超过此时长没收到 Heartbeat → 标记 abandoned + 释放(W4 ② 仅释放,补偿留 W4 ③)。
	HeartbeatTimeout config.Duration `yaml:"heartbeat_timeout,omitempty" json:"heartbeat_timeout,omitempty"`

	// SweepInterval 心跳超时扫描间隔(默认 5s)。
	SweepInterval config.Duration `yaml:"sweep_interval,omitempty" json:"sweep_interval,omitempty"`

	// BattleTTL 战斗 DS 镜像 Redis key 的 TTL(默认 2h,防僵尸镜像)。
	BattleTTL config.Duration `yaml:"battle_ttl,omitempty" json:"battle_ttl,omitempty"`

	// MockDSAddrHost W4 ② MockGameServerAllocator 返回的假 DS host(默认 127.0.0.1)。
	// W4 ③ 接 Agones 后此字段废弃,addr 由 GameServerAllocation status 返回。
	MockDSAddrHost string `yaml:"mock_ds_addr_host,omitempty" json:"mock_ds_addr_host,omitempty"`

	// MockDSPortBase W4 ② MockGameServerAllocator 端口基址(默认 30000)。
	// 每场 match 端口 = MockDSPortBase + (match_id % MockDSPortRange)。
	MockDSPortBase int `yaml:"mock_ds_port_base,omitempty" json:"mock_ds_port_base,omitempty"`

	// MockDSPortRange Mock 端口取模范围(默认 1000)。
	MockDSPortRange int `yaml:"mock_ds_port_range,omitempty" json:"mock_ds_port_range,omitempty"`
}

// Defaults 填默认值。
func (c *Config) Defaults() {
	if c.Allocator.HeartbeatTimeout == 0 {
		c.Allocator.HeartbeatTimeout = config.Duration(15 * time.Second)
	}
	if c.Allocator.SweepInterval == 0 {
		c.Allocator.SweepInterval = config.Duration(5 * time.Second)
	}
	if c.Allocator.BattleTTL == 0 {
		c.Allocator.BattleTTL = config.Duration(2 * time.Hour)
	}
	if c.Allocator.MockDSAddrHost == "" {
		c.Allocator.MockDSAddrHost = "127.0.0.1"
	}
	if c.Allocator.MockDSPortBase == 0 {
		c.Allocator.MockDSPortBase = 30000
	}
	if c.Allocator.MockDSPortRange == 0 {
		c.Allocator.MockDSPortRange = 1000
	}
	if c.Agones.APIServer == "" {
		c.Agones.APIServer = "https://kubernetes.default.svc"
	}
	if c.Agones.Namespace == "" {
		c.Agones.Namespace = "default"
	}
	if c.Agones.TokenPath == "" {
		c.Agones.TokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	}
	if c.Agones.CAPath == "" {
		c.Agones.CAPath = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	}
	if c.Agones.AllocateTimeout == 0 {
		c.Agones.AllocateTimeout = config.Duration(5 * time.Second)
	}
	if c.LocalDS.AdvertiseHost == "" {
		c.LocalDS.AdvertiseHost = "127.0.0.1"
	}
	if c.LocalDS.PortBase == 0 {
		c.LocalDS.PortBase = 7777
	}
	if c.LocalDS.PortRange == 0 {
		c.LocalDS.PortRange = 100
	}
	if c.LocalDS.LogDir == "" {
		c.LocalDS.LogDir = "run/dev/logs/ds"
	}
	if c.Server.Grpc.Addr == "" {
		c.Server.Grpc.Addr = ":50020"
	}
	if c.Server.Http.Addr == "" {
		c.Server.Http.Addr = ":51020"
	}
}
