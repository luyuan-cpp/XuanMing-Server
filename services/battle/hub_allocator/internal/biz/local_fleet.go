// local_fleet.go — 本机 exec 常驻 Windows Hub DS 的 HubFleetProvider(mode=local)。
//
// 与 ds_allocator/data/local_allocator.go 对称,是 Windows 单机自测时大厅 DS 的来源:
// 首次 ListShards 时懒拉起「一个」常驻 Hub DS 进程(加载 hub 关卡 / PandoraHubGameMode),
// 把它作为该 region 唯一的 ShardCandidate 返回;进程随 hub_allocator 退出由 Close Kill。
//
// 与战斗 DS 的差异:Hub DS 是常驻分片(不按对局回收),所以这里只起一个进程并长期持有,
// 不做端口池 / 多实例 / reaper 回收。topology-only:不实现 HubFleetScaler,故本机模式下
// autoscale/consolidation 不会运行(与 Mock 同语义)。
package biz

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/google/uuid"

	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/conf"
)

// LocalHubFleetProvider 在本机 exec 一个常驻 UE Windows Hub Dedicated Server 进程。
type LocalHubFleetProvider struct {
	cfg     conf.LocalHubConf
	podName string
	addr    string

	once sync.Once // 懒拉起只执行一次
	mu   sync.Mutex
	cmd  *exec.Cmd
	logF *os.File
}

// NewLocalHubFleetProvider 构造本机 Hub DS 拉起器。
//
// 失败场景(返 error,main 据此 fatal):
//   - ExecutablePath 空
//   - ExecutablePath 指向的文件不存在
func NewLocalHubFleetProvider(cfg conf.LocalHubConf) (*LocalHubFleetProvider, error) {
	if cfg.ExecutablePath == "" {
		return nil, fmt.Errorf("local_hub: executable_path required when mode=local")
	}
	if _, err := os.Stat(cfg.ExecutablePath); err != nil {
		return nil, fmt.Errorf("local_hub: executable_path %q not found: %w", cfg.ExecutablePath, err)
	}
	return &LocalHubFleetProvider{
		cfg: cfg,
		// 每次进程启动生成唯一实例名(对齐线上 Agones「GameServer 名每次唯一」语义):
		// 旧进程被杀后残留的 Redis 分片记录会因名字不再匹配而成为「不在 Fleet live 集」的孤儿,
		// 被 reconcileShardTopology 清理;新进程用新名建一条全新 ready 记录,不复用旧的 draining 状态。
		// 与 HeartbeatShard 存活复活是双保险:UUID 治「身份复用」,复活治「活 pod 被误判超时」。
		podName: "pandora-hub-local-" + uuid.NewString()[:8],
		addr:    fmt.Sprintf("%s:%d", cfg.AdvertiseHost, cfg.Port),
	}, nil
}

// ListShards 返回本机唯一的 Hub 分片(并在首次调用时懒拉起常驻 Hub DS 进程)。
func (l *LocalHubFleetProvider) ListShards(_ context.Context, region string) ([]ShardCandidate, error) {
	l.ensureStarted()
	if region == "" {
		region = l.cfg.Region
	}
	return []ShardCandidate{{
		PodName:  l.podName,
		Addr:     l.addr,
		Region:   region,
		ShardID:  1,
		Capacity: l.cfg.Capacity,
	}}, nil
}

// ensureStarted 懒拉起常驻 Hub DS 进程(仅一次)。拉起失败只记日志:
// 客户端仍会拿到分片地址,连接失败时由其自身重试/报错,便于排查 DS 启动问题。
func (l *LocalHubFleetProvider) ensureStarted() {
	l.once.Do(func() {
		if err := l.start(); err != nil {
			plog.With(context.Background()).Errorw("msg", "local_hub_ds_start_failed",
				"err", err, "executable", l.cfg.ExecutablePath, "addr", l.addr,
				"hint", "检查 local_hub.executable_path / map_name;客户端连 Hub 会失败")
			return
		}
		plog.With(context.Background()).Infow("msg", "local_hub_ds_started",
			"pod", l.podName, "addr", l.addr, "map", l.cfg.MapName)
	})
}

// start 真正 exec UE Windows Hub DS 并把 stdout/stderr 落盘。
func (l *LocalHubFleetProvider) start() error {
	cmd := exec.Command(l.cfg.ExecutablePath, l.buildArgs()...) //nolint:gosec // 路径来自受信本机配置
	if l.cfg.WorkingDir != "" {
		cmd.Dir = l.cfg.WorkingDir
	}
	cmd.Env = l.buildEnv()

	var logF *os.File
	if l.cfg.LogDir != "" {
		if err := os.MkdirAll(l.cfg.LogDir, 0o755); err == nil {
			if f, ferr := os.Create(filepath.Join(l.cfg.LogDir, l.podName+".log")); ferr == nil {
				logF = f
				cmd.Stdout = f
				cmd.Stderr = f
			}
		}
	}

	if err := cmd.Start(); err != nil {
		if logF != nil {
			_ = logF.Close()
		}
		return err
	}

	l.mu.Lock()
	l.cmd = cmd
	l.logF = logF
	l.mu.Unlock()
	return nil
}

// buildArgs 拼 UE DS 命令行:大厅关卡 + -server -log -port=<port> + 额外参数。
func (l *LocalHubFleetProvider) buildArgs() []string {
	args := make([]string, 0, 4+len(l.cfg.ExtraArgs))
	if l.cfg.MapName != "" {
		args = append(args, l.cfg.MapName)
	}
	args = append(args, "-server", "-log", fmt.Sprintf("-port=%d", l.cfg.Port))
	args = append(args, l.cfg.ExtraArgs...)
	return args
}

// buildEnv 注入 Hub DS 身份变量(对齐 UE DS 侧读取的 env)。
func (l *LocalHubFleetProvider) buildEnv() []string {
	env := os.Environ()
	env = append(env,
		"AGONES_GAMESERVER_NAME="+l.podName,
		"PANDORA_DS_TYPE=hub",
		"PANDORA_REGION="+l.cfg.Region,
	)
	for k, v := range l.cfg.ExtraEnv {
		env = append(env, k+"="+v)
	}
	return env
}

// Close 终止常驻 Hub DS 进程(hub_allocator 退出时调用,避免遗留孤儿)。
func (l *LocalHubFleetProvider) Close() error {
	l.mu.Lock()
	cmd, logF := l.cmd, l.logF
	l.cmd, l.logF = nil, nil
	l.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	if logF != nil {
		_ = logF.Close()
	}
	return nil
}
