// agones_allocator.go — 真 Agones GameServerAllocator(W4 ⑫)。
//
// 用 k8s apiserver REST 直连 allocation.agones.dev/v1 GameServerAllocation,
// 不引入 agones / client-go 重依赖(只用标准库 net/http + crypto/tls + encoding/json),
// 保持 ds_allocator go.mod 干净、本地可编译可单测。Agones 的分配 API 与 k8s provider
// 无关(ACK / 自建 / minikube 上的 Agones controller 一致),故本实现 provider-agnostic。
//
// 职责切分(对齐 biz.GameServerAllocator 接口,biz 零改):
//   - Allocate:POST GameServerAllocation,从 status 取 gameServerName + address:port。
//     status.state != "Allocated"(无空闲 GameServer)→ ErrDSNoAvailable。
//   - Release:DELETE 该 GameServer,Fleet 自动补一个新的;404 视作已释放(幂等)。
//
// 鉴权:in-cluster 读 ServiceAccount token(每次请求重读,容忍 token 轮转)+ CA;
// 集群外联调可显式配 api_server + token_path(或经 kubectl proxy 不带 token)。
package data

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/conf"
)

// fleetLabelKey 是 Agones Fleet 给其 GameServer 打的标签 key(selector 用)。
const fleetLabelKey = "agones.dev/fleet"

// AgonesGameServerAllocator 经 k8s REST 调 Agones GameServerAllocation。
type AgonesGameServerAllocator struct {
	apiServer       string // 已去尾部 /
	namespace       string
	fleetName       string
	mapFleets       map[uint32]string // map_id → 专属预热 Fleet(可选;命中时作首选 selector)
	advertiseHost   string
	tokenPath       string // "" 或 "-" → 不带 Authorization
	allocateTimeout time.Duration
	httpClient      *http.Client
}

// NewAgonesGameServerAllocator 构造真 Agones 分配器。
//
// 失败场景(返 error,main 据此 fatal 或回退):
//   - Enabled 但 FleetName 空(无法选择 GameServer)
//   - CA 文件配置了却读不出 / 解析失败
func NewAgonesGameServerAllocator(cfg conf.AgonesConf) (*AgonesGameServerAllocator, error) {
	if cfg.FleetName == "" {
		return nil, fmt.Errorf("agones: fleet_name required when enabled")
	}

	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.InsecureSkipTLSVerify, //nolint:gosec // 仅 dev 显式开启
	}
	if !cfg.InsecureSkipTLSVerify && cfg.CAPath != "" {
		// CA 文件存在才加载;in-cluster 默认路径在集群外不存在 → 跳过用系统根证书池。
		if pem, err := os.ReadFile(cfg.CAPath); err == nil {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("agones: parse CA %s failed", cfg.CAPath)
			}
			tlsCfg.RootCAs = pool
		}
	}

	timeout := cfg.AllocateTimeout.Std()
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	mapFleets := make(map[uint32]string, len(cfg.MapFleets))
	for _, mf := range cfg.MapFleets {
		if mf.MapID > 0 && mf.FleetName != "" {
			mapFleets[mf.MapID] = mf.FleetName
		}
	}

	return &AgonesGameServerAllocator{
		apiServer:       strings.TrimRight(cfg.APIServer, "/"),
		namespace:       cfg.Namespace,
		fleetName:       cfg.FleetName,
		mapFleets:       mapFleets,
		advertiseHost:   strings.TrimSpace(cfg.AdvertiseHost),
		tokenPath:       cfg.TokenPath,
		allocateTimeout: timeout,
		httpClient: &http.Client{
			Timeout:   timeout,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}, nil
}

// ── GameServerAllocation 请求 / 响应 JSON(只声明用到的字段)──────────────────

type gsaSelector struct {
	MatchLabels map[string]string `json:"matchLabels,omitempty"`
}

type gsaMetadata struct {
	Labels map[string]string `json:"labels,omitempty"`
}

type gsaSpec struct {
	Selectors []gsaSelector `json:"selectors,omitempty"`
	Metadata  *gsaMetadata  `json:"metadata,omitempty"`
}

type gsaRequest struct {
	APIVersion string  `json:"apiVersion"`
	Kind       string  `json:"kind"`
	Spec       gsaSpec `json:"spec"`
}

type gsaPort struct {
	Name string `json:"name"`
	Port int32  `json:"port"`
}

type gsaStatus struct {
	State          string    `json:"state"`
	GameServerName string    `json:"gameServerName"`
	Address        string    `json:"address"`
	Ports          []gsaPort `json:"ports"`
}

type gsaResponse struct {
	Status gsaStatus `json:"status"`
}

// Allocate POST 一个 GameServerAllocation,返回 (gameServerName, address:port)。
//
// selectors 有序(Agones 按顺序尝试,选中首个有空闲 GameServer 的):
//  1. 若 mapID 配了专属预热 Fleet(map_fleets)→ 首选它(Pod 已预加载目标图,分配即可玩);
//  2. 通用 Fleet(Loader 模式,分配后按 map-id label travel)作兜底。
func (a *AgonesGameServerAllocator) Allocate(ctx context.Context, matchID uint64, mapID uint32, gameMode string) (string, string, error) {
	selectors := make([]gsaSelector, 0, 2)
	if dedicated := a.mapFleets[mapID]; dedicated != "" && dedicated != a.fleetName {
		selectors = append(selectors, gsaSelector{MatchLabels: map[string]string{fleetLabelKey: dedicated}})
	}
	selectors = append(selectors, gsaSelector{MatchLabels: map[string]string{fleetLabelKey: a.fleetName}})

	reqBody := gsaRequest{
		APIVersion: "allocation.agones.dev/v1",
		Kind:       "GameServerAllocation",
		Spec: gsaSpec{
			Selectors: selectors,
			// 把业务标识打到被分配的 GameServer 上,便于运维 / 排障关联对局。
			Metadata: &gsaMetadata{Labels: map[string]string{
				"pandora.dev/match-id":  fmt.Sprintf("%d", matchID),
				"pandora.dev/map-id":    fmt.Sprintf("%d", mapID),
				"pandora.dev/game-mode": sanitizeLabelValue(gameMode),
			}},
		},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", "", errcode.New(errcode.ErrDSAllocationFailed, "agones: marshal request: %v", err)
	}

	url := fmt.Sprintf("%s/apis/allocation.agones.dev/v1/namespaces/%s/gameserverallocations",
		a.apiServer, a.namespace)

	respBytes, status, err := a.do(ctx, http.MethodPost, url, payload)
	if err != nil {
		return "", "", errcode.New(errcode.ErrDSAllocationFailed, "agones: allocate match %d: %v", matchID, err)
	}
	if status < 200 || status >= 300 {
		return "", "", errcode.New(errcode.ErrDSAllocationFailed,
			"agones: allocate match %d http %d: %s", matchID, status, truncate(respBytes, 256))
	}

	var resp gsaResponse
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return "", "", errcode.New(errcode.ErrDSAllocationFailed, "agones: decode response: %v", err)
	}
	// state 只有 "Allocated" 才表示拿到了 GameServer;UnAllocated / Contention = 无空闲。
	if resp.Status.State != "Allocated" {
		return "", "", errcode.New(errcode.ErrDSNoAvailable,
			"agones: no gameserver for match %d (state=%q)", matchID, resp.Status.State)
	}
	if resp.Status.GameServerName == "" || resp.Status.Address == "" || len(resp.Status.Ports) == 0 {
		return "", "", errcode.New(errcode.ErrDSAllocationFailed,
			"agones: incomplete status for match %d: name=%q addr=%q ports=%d",
			matchID, resp.Status.GameServerName, resp.Status.Address, len(resp.Status.Ports))
	}

	host := resp.Status.Address
	if a.advertiseHost != "" {
		host = a.advertiseHost
	}
	addr := fmt.Sprintf("%s:%d", host, resp.Status.Ports[0].Port)
	return resp.Status.GameServerName, addr, nil
}

// Release DELETE 该 GameServer(Fleet 自动补新);404 视作已释放(幂等)。
func (a *AgonesGameServerAllocator) Release(ctx context.Context, podName string) error {
	if podName == "" {
		return nil
	}
	url := fmt.Sprintf("%s/apis/agones.dev/v1/namespaces/%s/gameservers/%s",
		a.apiServer, a.namespace, podName)

	respBytes, status, err := a.do(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return errcode.New(errcode.ErrDSAllocationFailed, "agones: release %s: %v", podName, err)
	}
	if status == http.StatusNotFound {
		return nil // 已不存在 = 已释放,幂等
	}
	if status < 200 || status >= 300 {
		return errcode.New(errcode.ErrDSAllocationFailed,
			"agones: release %s http %d: %s", podName, status, truncate(respBytes, 256))
	}
	return nil
}

// ── Fleet 容量巡检(K8s 快上限预警,2026-07-10)──────────────────────────────

// FleetCapacity 一个 Agones Fleet 的容量快照(取自 Fleet status 子对象)。
type FleetCapacity struct {
	Fleet     string // Fleet 名
	Replicas  uint32 // status.replicas:当前总副本数(容量上限)
	Ready     uint32 // status.readyReplicas:空闲可分配
	Allocated uint32 // status.allocatedReplicas:已被对局占用
}

// fleetStatusResponse 只声明容量巡检用到的 Fleet status 字段。
type fleetStatusResponse struct {
	Status struct {
		Replicas          uint32 `json:"replicas"`
		ReadyReplicas     uint32 `json:"readyReplicas"`
		AllocatedReplicas uint32 `json:"allocatedReplicas"`
	} `json:"status"`
}

// WatchedFleets 返回容量巡检要盯的 Fleet 集合:通用池 fleetName + 全部 map_fleets 专属预热池
// (去重;通用池在前,专属池按名字典序,保证顺序稳定)。
func (a *AgonesGameServerAllocator) WatchedFleets() []string {
	out := make([]string, 0, 1+len(a.mapFleets))
	seen := map[string]bool{a.fleetName: true}
	out = append(out, a.fleetName)
	dedicated := make([]string, 0, len(a.mapFleets))
	for _, name := range a.mapFleets {
		if !seen[name] {
			seen[name] = true
			dedicated = append(dedicated, name)
		}
	}
	sort.Strings(dedicated)
	return append(out, dedicated...)
}

// ListFleetCapacities GET 每个受管 Fleet 的 status,返回容量快照。
// 单个 Fleet 查询失败不影响其余(部分成功也返回),错误经 errors.Join 汇总供上层打日志。
func (a *AgonesGameServerAllocator) ListFleetCapacities(ctx context.Context) ([]FleetCapacity, error) {
	var (
		out  []FleetCapacity
		errs []error
	)
	for _, fleet := range a.WatchedFleets() {
		url := fmt.Sprintf("%s/apis/agones.dev/v1/namespaces/%s/fleets/%s",
			a.apiServer, a.namespace, fleet)
		respBytes, status, err := a.do(ctx, http.MethodGet, url, nil)
		if err != nil {
			errs = append(errs, fmt.Errorf("agones: get fleet %s: %w", fleet, err))
			continue
		}
		if status < 200 || status >= 300 {
			errs = append(errs, fmt.Errorf("agones: get fleet %s http %d: %s",
				fleet, status, truncate(respBytes, 256)))
			continue
		}
		var resp fleetStatusResponse
		if err := json.Unmarshal(respBytes, &resp); err != nil {
			errs = append(errs, fmt.Errorf("agones: decode fleet %s: %w", fleet, err))
			continue
		}
		out = append(out, FleetCapacity{
			Fleet:     fleet,
			Replicas:  resp.Status.Replicas,
			Ready:     resp.Status.ReadyReplicas,
			Allocated: resp.Status.AllocatedReplicas,
		})
	}
	return out, errors.Join(errs...)
}

// do 发一次带鉴权的 REST 请求,返回 (body, statusCode, transportErr)。
func (a *AgonesGameServerAllocator) do(ctx context.Context, method, url string, body []byte) ([]byte, int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, a.allocateTimeout)
	defer cancel()

	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, url, rdr)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	// 每次请求重读 token(容忍 in-cluster 投影 token 轮转);"-" 或空 → 不带。
	if a.tokenPath != "" && a.tokenPath != "-" {
		if tok, terr := os.ReadFile(a.tokenPath); terr == nil {
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
		}
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return respBytes, resp.StatusCode, nil
}

// sanitizeLabelValue 把 game_mode 收敛成合法 k8s label value(≤63 字符,首尾字母数字,
// 中间允许 -_.);非法字符替换为 '-',空值 / 全非法值返回 "unknown"。
func sanitizeLabelValue(s string) string {
	if s == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if len(out) > 63 {
		out = out[:63]
	}
	out = strings.Trim(out, "-_.")
	if out == "" {
		return "unknown"
	}
	return out
}

// truncate 截断 body 给错误信息用,避免日志过长。
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
