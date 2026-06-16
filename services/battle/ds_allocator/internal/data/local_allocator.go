// local_allocator.go — 本机拉起 Windows Dedicated Server 进程的调试用 GameServerAllocator。
//
// 这是与 AgonesGameServerAllocator(Linux 生产,见 agones_allocator.go)并列的第二种
// DS 启动方式,专供本机联调:匹配成局后 ds_allocator 直接 exec 打包好的 UE Windows DS,
// 分配一个本机端口,返回真实地址(host:port)给客户端 NetDriver;Release / 心跳超时
// abandoned 时 Kill 进程。三种方式共用 biz.GameServerAllocator 接口,biz 逻辑零改。
//
// 设计要点:
//   - 进程台账(podName → 进程 + 端口)在内存维护,带互斥锁;ds_allocator 退出时 Close 全杀。
//   - 每个 DS 进程一个 reaper goroutine Wait(),进程自行退出(崩溃)时清理台账释放端口
//     (镜像仍靠心跳超时 sweep 标 abandoned,与 Agones pod 崩溃同语义)。
//   - Allocate 幂等:同 podName(由 matchID 派生)已在台账则直接返回原地址,不重复拉进程。
//   - 启动函数 startProc 抽成字段,单测可注入假进程,避免真的 exec UE。
package data

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/conf"
)

// dsProcess 抽象一个已拉起的 DS 进程,便于单测注入假实现。
type dsProcess interface {
	// Kill 终止进程(已退出则应为 no-op 不报致命错)。
	Kill() error
	// Wait 阻塞直到进程退出。
	Wait() error
}

// execProcess 是 dsProcess 的真实现,包一个 *exec.Cmd。
type execProcess struct {
	cmd  *exec.Cmd
	logF *os.File // 日志文件句柄,进程退出后关闭
}

func (e *execProcess) Kill() error {
	if e.cmd.Process != nil {
		return e.cmd.Process.Kill()
	}
	return nil
}

func (e *execProcess) Wait() error {
	err := e.cmd.Wait()
	if e.logF != nil {
		_ = e.logF.Close()
	}
	return err
}

// launchedProc 是台账里的一条记录。
type launchedProc struct {
	proc dsProcess
	port int
	addr string
}

// LocalGameServerAllocator 在本机 exec UE Windows Dedicated Server 进程。
type LocalGameServerAllocator struct {
	cfg conf.LocalDSConf

	mu        sync.Mutex
	procs     map[string]*launchedProc // podName → 进程记录
	usedPorts map[int]struct{}

	// startProc 拉起一个 DS 进程;单测注入假实现绕过真 exec。
	startProc func(podName string, port int, matchID uint64, mapID uint32, gameMode string) (dsProcess, error)
}

// NewLocalGameServerAllocator 构造本机 DS 拉起器。
//
// 失败场景(返 error,main 据此 fatal):
//   - ExecutablePath 空
//   - ExecutablePath 指向的文件不存在
//   - PortRange <= 0
func NewLocalGameServerAllocator(cfg conf.LocalDSConf) (*LocalGameServerAllocator, error) {
	if cfg.ExecutablePath == "" {
		return nil, fmt.Errorf("local_ds: executable_path required when enabled")
	}
	if _, err := os.Stat(cfg.ExecutablePath); err != nil {
		return nil, fmt.Errorf("local_ds: executable_path %q not found: %w", cfg.ExecutablePath, err)
	}
	if cfg.PortRange <= 0 {
		return nil, fmt.Errorf("local_ds: port_range must be > 0")
	}
	l := &LocalGameServerAllocator{
		cfg:       cfg,
		procs:     make(map[string]*launchedProc),
		usedPorts: make(map[int]struct{}),
	}
	l.startProc = l.defaultStart
	return l, nil
}

// Allocate 拉起一个本机 DS 进程,返回 (podName, host:port)。
func (l *LocalGameServerAllocator) Allocate(_ context.Context, matchID uint64, mapID uint32, gameMode string) (string, string, error) {
	podName := fmt.Sprintf("pandora-battle-local-%d", matchID)

	l.mu.Lock()
	defer l.mu.Unlock()

	// 幂等:同对局已拉起 → 直接返回原地址。
	if p, ok := l.procs[podName]; ok {
		return podName, p.addr, nil
	}

	port, ok := l.pickPortLocked()
	if !ok {
		return "", "", errcode.New(errcode.ErrDSNoAvailable,
			"local_ds: no free port in [%d,%d) for match %d",
			l.cfg.PortBase, l.cfg.PortBase+l.cfg.PortRange, matchID)
	}

	proc, err := l.startProc(podName, port, matchID, mapID, gameMode)
	if err != nil {
		return "", "", errcode.New(errcode.ErrDSAllocationFailed,
			"local_ds: launch match %d on port %d: %v", matchID, port, err)
	}

	addr := fmt.Sprintf("%s:%d", l.cfg.AdvertiseHost, port)
	lp := &launchedProc{proc: proc, port: port, addr: addr}
	l.procs[podName] = lp
	l.usedPorts[port] = struct{}{}

	go l.reap(podName, lp)

	return podName, addr, nil
}

// Release 终止指定 DS 进程;台账无此记录视作已释放(幂等)。
func (l *LocalGameServerAllocator) Release(_ context.Context, podName string) error {
	if podName == "" {
		return nil
	}
	l.mu.Lock()
	lp, ok := l.procs[podName]
	if ok {
		delete(l.procs, podName)
		delete(l.usedPorts, lp.port)
	}
	l.mu.Unlock()

	if !ok {
		return nil
	}
	if err := lp.proc.Kill(); err != nil {
		return errcode.New(errcode.ErrDSAllocationFailed, "local_ds: kill %s: %v", podName, err)
	}
	return nil
}

// Close 终止全部在管 DS 进程(ds_allocator 退出时调用,避免遗留孤儿 DS)。
func (l *LocalGameServerAllocator) Close() error {
	l.mu.Lock()
	procs := l.procs
	l.procs = make(map[string]*launchedProc)
	l.usedPorts = make(map[int]struct{})
	l.mu.Unlock()

	for _, lp := range procs {
		_ = lp.proc.Kill()
	}
	return nil
}

// reap 等待进程退出后清理台账释放端口(仅当台账里仍是同一条记录,避免与 Release/重拉竞态)。
func (l *LocalGameServerAllocator) reap(podName string, lp *launchedProc) {
	_ = lp.proc.Wait()
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.procs[podName]; ok && cur == lp {
		delete(l.procs, podName)
		delete(l.usedPorts, lp.port)
	}
}

// pickPortLocked 在端口池里取一个空闲端口(调用方须持锁)。
func (l *LocalGameServerAllocator) pickPortLocked() (int, bool) {
	for p := l.cfg.PortBase; p < l.cfg.PortBase+l.cfg.PortRange; p++ {
		if _, used := l.usedPorts[p]; !used {
			return p, true
		}
	}
	return 0, false
}

// defaultStart 是 startProc 的真实现:exec UE Windows DS 并把 stdout/stderr 落盘。
func (l *LocalGameServerAllocator) defaultStart(podName string, port int, matchID uint64, mapID uint32, gameMode string) (dsProcess, error) {
	cmd := exec.Command(l.cfg.ExecutablePath, l.buildArgs(port, gameMode)...) //nolint:gosec // 路径来自受信本机配置
	if l.cfg.WorkingDir != "" {
		cmd.Dir = l.cfg.WorkingDir
	}
	cmd.Env = l.buildEnv(podName, matchID, mapID, gameMode)

	var logF *os.File
	if l.cfg.LogDir != "" {
		if err := os.MkdirAll(l.cfg.LogDir, 0o755); err == nil {
			if f, ferr := os.Create(filepath.Join(l.cfg.LogDir, podName+".log")); ferr == nil {
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
		return nil, err
	}
	return &execProcess{cmd: cmd, logF: logF}, nil
}

// buildArgs 拼 UE DS 命令行:关卡 + -server -log -port=<port> + 额外参数。
func (l *LocalGameServerAllocator) buildArgs(port int, _ string) []string {
	args := make([]string, 0, 4+len(l.cfg.ExtraArgs))
	if l.cfg.MapName != "" {
		args = append(args, l.cfg.MapName)
	}
	args = append(args, "-server", "-log", fmt.Sprintf("-port=%d", port))
	args = append(args, l.cfg.ExtraArgs...)
	return args
}

// buildEnv 在当前进程环境基础上注入 DS 身份变量(对齐 UE DS 侧 PandoraAgonesProvider 读取的 env)。
func (l *LocalGameServerAllocator) buildEnv(podName string, matchID uint64, mapID uint32, gameMode string) []string {
	env := os.Environ()
	env = append(env,
		"AGONES_GAMESERVER_NAME="+podName,
		"PANDORA_MATCH_ID="+strconv.FormatUint(matchID, 10),
		"PANDORA_MAP_ID="+strconv.FormatUint(uint64(mapID), 10),
		"PANDORA_GAME_MODE="+gameMode,
	)
	for k, v := range l.cfg.ExtraEnv {
		env = append(env, k+"="+v)
	}
	return env
}
