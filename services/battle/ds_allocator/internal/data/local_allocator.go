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
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	if e.cmd.Process == nil {
		return nil
	}
	// Windows:UE DS(PandoraServer.exe)可能派生子进程,只 kill 父进程会留下仍占监听端口的
	// 子进程(幽灵 DS),导致后续对局撞端口。taskkill /T 杀整棵进程树,/F 强制,确保端口真正释放。
	if runtime.GOOS == "windows" {
		kc := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(e.cmd.Process.Pid)) //nolint:gosec // pid 来自本进程派生的 DS
		if err := kc.Run(); err == nil {
			return nil
		}
		// taskkill 不可用/失败 → 回退直接 kill 父进程(至少不泄漏父进程)。
	}
	return e.cmd.Process.Kill()
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

	// portProbe 探测端口在本机是否真的空闲(可绑定)。nil=不探测(单测默认放行)。
	// 用于挡住「台账已释放但进程未退出(幽灵 DS)」或外部程序占用的端口:否则 allocator 把
	// -port=X 传给 UE DS,X 被占时 UE 会静默 fallback 到 X+1,导致 allocator 记录/返回的端口(X)
	// 与 DS 实际监听端口(X+1)不一致,新对局客户端拿新 ticket 却连到 X 上的旧 DS,被 PreLogin 拒。
	portProbe func(port int) bool
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
	l.portProbe = defaultPortProbe
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
// 除排除本 allocator 已分配的端口(usedPorts)外,还用 portProbe 实际探测端口在本机可绑定,
// 跳过被幽灵 DS / 外部程序占用的端口,保证发给 UE DS 的 -port 就是它能真正绑上的端口。
func (l *LocalGameServerAllocator) pickPortLocked() (int, bool) {
	for p := l.cfg.PortBase; p < l.cfg.PortBase+l.cfg.PortRange; p++ {
		if _, used := l.usedPorts[p]; used {
			continue
		}
		if l.portProbe != nil && !l.portProbe(p) {
			continue // 端口被占(幽灵 DS / 外部程序)→ 跳过,避免 UE 静默换端口
		}
		return p, true
	}
	return 0, false
}

// defaultPortProbe 探测端口在所有网卡上 UDP+TCP 是否可绑定(UE DS NetDriver 用 UDP,保守起见
// TCP 也探,兼容 TCP/WebSocket 传输)。绑定成功立即释放再交给 UE 启动;探测与 UE 真正绑定之间
// 有极短 TOCTOU 窗口,但幽灵 DS 是持续占用能被稳定挡住,usedPorts 又已防 allocator 自身重复分配。
func defaultPortProbe(port int) bool {
	uc, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
	if err != nil {
		return false
	}
	_ = uc.Close()
	tl, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	_ = tl.Close()
	return true
}

// defaultStart 是 startProc 的真实现:exec UE Windows DS 并把 stdout/stderr 落盘。
func (l *LocalGameServerAllocator) defaultStart(podName string, port int, matchID uint64, mapID uint32, gameMode string) (dsProcess, error) {
	cmd := exec.Command(l.cfg.ExecutablePath, l.buildArgs(port, mapID)...) //nolint:gosec // 路径来自受信本机配置
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
// 首个加载关卡由 cfg.ResolveStartupMap(mapID) 决定:配了 LoaderMap 则统一启到加载/分发关卡(UE 侧
// 读 PANDORA_MAP_ID → 查表 → ServerTravel);否则按 map_id 从 Maps 直接选副本图(未命中回退 MapName)。
func (l *LocalGameServerAllocator) buildArgs(port int, mapID uint32) []string {
	args := make([]string, 0, 4+len(l.cfg.ExtraArgs))
	if mapName := l.cfg.ResolveStartupMap(mapID); mapName != "" {
		args = append(args, mapName)
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
