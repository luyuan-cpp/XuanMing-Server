// agones_fleet.go — 真 Agones HubFleetProvider(W4 ⑬,2026-06-09)。
//
// 与 ds_allocator 的战斗 DS 模型不同:大厅 Hub DS 是「常驻分片」而非「按需分配」。
// Hub DS GameServer 持续以 Ready 状态运行,hub_allocator 自己在 Redis 里维护各分片的
// player_count 做容量判定(不走 Agones GameServerAllocation)。因此本 provider 的职责是
// 「发现拓扑」——LIST Fleet 下的 GameServer(按 region 标签过滤),把可承载玩家的实例
// 映射成 ShardCandidate,供 biz.ensureShards lazy-seed 到 Redis。
//
// 实现路线与 ds_allocator/data/agones_allocator.go 一致:用标准库 net/http 直连
// k8s apiserver REST 查 agones.dev/v1 GameServer 列表,不引入 agones/client-go 重依赖,
// 保持 hub_allocator go.mod 干净、本地可编译可单测;Agones API 与 k8s provider 无关
// (ACK / 自建 / minikube 一致),故 provider-agnostic。
//
// 阶段限制(W4 ⑬):ensureShards 仅在 region 首次无分片时 lazy-seed,Fleet 扩缩容后的
// 新 GameServer 不会被自动发现(周期性 reconcile 留后续)。本地 dev 联调够用。
package biz

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/conf"
)

const (
	// fleetLabelKey 是 Agones Fleet 给其 GameServer 打的标签 key(selector 用)。
	fleetLabelKey = "agones.dev/fleet"
	// regionLabelKey 是 Pandora 给 Hub DS GameServer 打的分区标签(按 region 过滤分片)。
	regionLabelKey = "pandora.dev/region"
	// shardIDLabelKey 可选:Hub DS 显式声明的稳定 shard_id(缺省则按 pod 名哈希派生)。
	shardIDLabelKey = "pandora.dev/shard-id"
	// capacityLabelKey 可选:单分片人数上限(缺省用 cfg.DefaultCapacity)。
	capacityLabelKey = "pandora.dev/capacity"
)

// hubReadyStates 是「可承载玩家」的 GameServer 状态集合。
// Hub DS 常驻 Ready;运维若对其做过 Allocation 保护(防缩容)则为 Allocated;
// Reserved 也短暂可用。其余(Shutdown / Error / Unhealthy / Scheduled 等)排除。
var hubReadyStates = map[string]struct{}{
	"Ready":     {},
	"Allocated": {},
	"Reserved":  {},
}

// AgonesHubFleetProvider 经 k8s apiserver REST 查 Agones GameServer 列表发现 Hub 分片拓扑。
type AgonesHubFleetProvider struct {
	apiServer   string // 已去尾部 /
	namespace   string
	fleetName   string
	tokenPath   string // "" 或 "-" → 不带 Authorization
	listTimeout time.Duration
	capacity    int32
	httpClient  *http.Client
}

// NewAgonesHubFleetProvider 构造真 Agones 分片发现器。
//
// 失败场景(返 error,main 据此 fatal 或回退):
//   - Enabled 但 FleetName 空(无法选择 GameServer)
//   - CA 文件配置了却解析失败
func NewAgonesHubFleetProvider(cfg conf.Config) (*AgonesHubFleetProvider, error) {
	ag := cfg.Agones
	if ag.FleetName == "" {
		return nil, fmt.Errorf("agones: fleet_name required when enabled")
	}

	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: ag.InsecureSkipTLSVerify, //nolint:gosec // 仅 dev 显式开启
	}
	if !ag.InsecureSkipTLSVerify && ag.CAPath != "" {
		// CA 文件存在才加载;in-cluster 默认路径在集群外不存在 → 跳过用系统根证书池。
		if pem, err := os.ReadFile(ag.CAPath); err == nil {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("agones: parse CA %s failed", ag.CAPath)
			}
			tlsCfg.RootCAs = pool
		}
	}

	timeout := ag.ListTimeout.Std()
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	capacity := cfg.Hub.DefaultCapacity
	if capacity <= 0 {
		capacity = 500
	}

	return &AgonesHubFleetProvider{
		apiServer:   strings.TrimRight(ag.APIServer, "/"),
		namespace:   ag.Namespace,
		fleetName:   ag.FleetName,
		tokenPath:   ag.TokenPath,
		listTimeout: timeout,
		capacity:    capacity,
		httpClient: &http.Client{
			Timeout:   timeout,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}, nil
}

// ── GameServer list 响应 JSON(只声明用到的字段)──────────────────────────────

type gsPort struct {
	Name string `json:"name"`
	Port int32  `json:"port"`
}

type gsStatus struct {
	State   string   `json:"state"`
	Address string   `json:"address"`
	Ports   []gsPort `json:"ports"`
}

type gsMetadata struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
}

type gameServer struct {
	Metadata gsMetadata `json:"metadata"`
	Status   gsStatus   `json:"status"`
}

type gsListResponse struct {
	Items []gameServer `json:"items"`
}

// ListShards LIST 指定 Fleet + region 下的 GameServer,映射成候选分片拓扑。
func (a *AgonesHubFleetProvider) ListShards(ctx context.Context, region string) ([]ShardCandidate, error) {
	selector := fleetLabelKey + "=" + a.fleetName
	if region != "" {
		selector += "," + regionLabelKey + "=" + region
	}
	q := url.Values{}
	q.Set("labelSelector", selector)
	listURL := fmt.Sprintf("%s/apis/agones.dev/v1/namespaces/%s/gameservers?%s",
		a.apiServer, a.namespace, q.Encode())

	respBytes, status, err := a.do(ctx, http.MethodGet, listURL)
	if err != nil {
		return nil, fmt.Errorf("agones: list gameservers region=%s: %w", region, err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("agones: list gameservers region=%s http %d: %s",
			region, status, truncateBody(respBytes, 256))
	}

	var resp gsListResponse
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return nil, fmt.Errorf("agones: decode gameserver list: %w", err)
	}

	out := make([]ShardCandidate, 0, len(resp.Items))
	for i := range resp.Items {
		gs := &resp.Items[i]
		if _, ok := hubReadyStates[gs.Status.State]; !ok {
			continue
		}
		if gs.Status.Address == "" || len(gs.Status.Ports) == 0 {
			continue // 尚未就绪(无 address/port),跳过
		}
		out = append(out, ShardCandidate{
			PodName:  gs.Metadata.Name,
			Addr:     fmt.Sprintf("%s:%d", gs.Status.Address, gs.Status.Ports[0].Port),
			Region:   region,
			ShardID:  shardIDFor(gs),
			Capacity: capacityFor(gs, a.capacity),
		})
	}
	return out, nil
}

// do 发一次带鉴权的 REST 请求,返回 (body, statusCode, transportErr)。
func (a *AgonesHubFleetProvider) do(ctx context.Context, method, reqURL string) ([]byte, int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, a.listTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, method, reqURL, nil)
	if err != nil {
		return nil, 0, err
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

// shardIDFor 取 GameServer 的稳定 shard_id:优先读 pandora.dev/shard-id 标签,
// 缺省/非法则按 pod 名 FNV-1a 哈希派生非零 uint32(同 pod 名稳定,仅作并列 tiebreak/展示)。
func shardIDFor(gs *gameServer) uint32 {
	if v, ok := gs.Metadata.Labels[shardIDLabelKey]; ok {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil && n != 0 {
			return uint32(n)
		}
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(gs.Metadata.Name))
	id := h.Sum32()
	if id == 0 {
		id = 1
	}
	return id
}

// capacityFor 取 GameServer 的容量:优先读 pandora.dev/capacity 标签,缺省/非法用 fallback。
func capacityFor(gs *gameServer, fallback int32) int32 {
	if v, ok := gs.Metadata.Labels[capacityLabelKey]; ok {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil && n > 0 {
			return int32(n)
		}
	}
	return fallback
}

// truncateBody 截断 body 给错误信息用,避免日志过长。
func truncateBody(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
