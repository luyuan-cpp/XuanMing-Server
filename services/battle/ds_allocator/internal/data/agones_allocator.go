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
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/luyuancpp/pandora/pkg/dsmetadata"
	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/releasetrack"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/conf"
)

// fleetLabelKey 是 Agones Fleet 给其 GameServer 打的标签 key(selector 用)。
const (
	fleetLabelKey               = "agones.dev/fleet"
	releaseTrackMetadataKey     = "pandora.dev/release-track"
	battleRosterAnnotationKey   = "pandora.dev/roster"
	battleAllocationMetadataKey = "pandora.dev/allocation-id"
)

type battleFleetRoute struct {
	stable string
	canary string
}

// AgonesGameServerAllocator 经 k8s REST 调 Agones GameServerAllocation。
type AgonesGameServerAllocator struct {
	apiServer       string // 已去尾部 /
	namespace       string
	fleetName       string
	canaryFleetName string
	mapFleets       map[uint32]battleFleetRoute // map_id → stable/canary 专属预热 Fleet
	advertiseHost   string
	tokenPath       string // "" 或 "-" → 不带 Authorization
	allocateTimeout time.Duration
	httpClient      *http.Client

	// dsTokenIssuer 签发 DS 回调服务令牌(审核 P1 #1;main 在 ds_auth.secret 已配时注入)。
	// 非 nil 时 Allocate 把令牌写进 GameServerAllocation 的 metadata.annotations
	// pandora.dev/ds-token,DS 经 Agones SDK GameServer() 读到后回调时带 Bearer 头。
	dsTokenIssuer func(matchID uint64) (token string, err error)
	// dsTokenRequired 为 guard=enforce 时 true:签发失败必须 fail-closed(不分配无令牌的 DS,
	// 否则该 DS 回调会被 enforce 守卫全拒,等于开了个连不回来的对局)。off/permissive 下 false,
	// 签发失败降级为无令牌分配以保对局可开。
	dsTokenRequired bool
}

// dsTokenAnnotationKey 是下发 DS 回调令牌的 GameServer annotation key。
// label 有 63 字符 / 字符集限制放不下 JWT,annotation 无此限制。
const dsTokenAnnotationKey = "pandora.dev/ds-token"

// SetDSTokenIssuer 注入 DS 回调令牌签发器(可选依赖,main 在 ds_auth.secret 已配时调用)。
// required=true(guard=enforce)时签发失败会让 Allocate 返回错误(fail-closed)。
func (a *AgonesGameServerAllocator) SetDSTokenIssuer(f func(matchID uint64) (string, error), required bool) {
	a.dsTokenIssuer = f
	a.dsTokenRequired = required
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
	if cfg.CanaryPercent > 100 {
		return nil, fmt.Errorf("agones: canary_percent=%d out of range [0,100]", cfg.CanaryPercent)
	}
	if cfg.CanaryPercent > 0 && (cfg.CanaryFleetName == "" || cfg.CanarySeed == "") {
		return nil, fmt.Errorf("agones: canary_percent>0 requires canary_fleet_name and canary_seed")
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

	mapFleets := make(map[uint32]battleFleetRoute, len(cfg.MapFleets))
	for _, mf := range cfg.MapFleets {
		if mf.MapID > 0 && (mf.FleetName != "" || mf.CanaryFleetName != "") {
			mapFleets[mf.MapID] = battleFleetRoute{stable: mf.FleetName, canary: mf.CanaryFleetName}
		}
	}

	return &AgonesGameServerAllocator{
		apiServer:       strings.TrimRight(cfg.APIServer, "/"),
		namespace:       cfg.Namespace,
		fleetName:       cfg.FleetName,
		canaryFleetName: cfg.CanaryFleetName,
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
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
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

// AuthoritativeGameServerAllocation 是 Model B 分配结果。UID/resourceVersion 只来自选中后
// 的严格 GameServer GET；AnnotationsPresent 决定 JSON Patch 应新增整个 map 还是单独成员。
type AuthoritativeGameServerAllocation struct {
	PodName            string
	Addr               string
	InstanceUID        string
	InstanceEpoch      uint32
	ResourceVersion    string
	AllocationID       string
	ReleaseTrack       string
	AnnotationsPresent bool
}

type gameServerResponse struct {
	Metadata struct {
		Name            string            `json:"name"`
		UID             string            `json:"uid"`
		ResourceVersion string            `json:"resourceVersion"`
		Labels          map[string]string `json:"labels"`
		Annotations     map[string]string `json:"annotations"`
	} `json:"metadata"`
}

type gameServerListResponse struct {
	Items []gameServerResponse `json:"items"`
}

type jsonPatchOperation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

type deleteOptions struct {
	APIVersion    string               `json:"apiVersion"`
	Kind          string               `json:"kind"`
	Preconditions *deletePreconditions `json:"preconditions,omitempty"`
}

type deletePreconditions struct {
	UID string `json:"uid"`
}

// Allocate POST 一个 GameServerAllocation,返回 (gameServerName, address:port)。
//
// selectors 有序(Agones 按顺序尝试,选中首个有空闲 GameServer 的):
//  1. 若 mapID 配了专属预热 Fleet(map_fleets)→ 首选它(Pod 已预加载目标图,分配即可玩);
//  2. 通用 Fleet(Loader 模式,分配后按 map-id label travel)作兜底。
func (a *AgonesGameServerAllocator) Allocate(ctx context.Context, matchID uint64, mapID uint32, gameMode, releaseTrack string) (string, string, string, error) {
	if !releasetrack.Valid(releaseTrack) {
		return "", "", "", errcode.New(errcode.ErrInvalidArg, "agones: invalid release_track %q", releaseTrack)
	}
	meta := &gsaMetadata{Labels: map[string]string{
		"pandora.dev/match-id":  fmt.Sprintf("%d", matchID),
		"pandora.dev/map-id":    fmt.Sprintf("%d", mapID),
		"pandora.dev/game-mode": sanitizeLabelValue(gameMode),
	}}
	// DS 回调服务令牌经 annotation 下发(DS 拿不到签名密钥,只持有短期令牌)。
	// enforce(dsTokenRequired=true):签发失败 fail-closed,返回分配失败,不产生连不回来的对局;
	// off/permissive:签发失败降级为无令牌分配,先保对局可开。
	if a.dsTokenIssuer != nil {
		if tok, terr := a.dsTokenIssuer(matchID); terr != nil {
			if a.dsTokenRequired {
				plog.With(ctx).Errorw("msg", "ds_callback_token_sign_failed", "match_id", matchID, "err", terr,
					"mode", "enforce", "hint", "ds_auth.mode=enforce 下签发失败即 fail-closed;检查 ds_auth.secret / 签名配置")
				return "", "", "", errcode.New(errcode.ErrDSAllocationFailed,
					"ds_callback_token sign failed under enforce for match %d: %v", matchID, terr)
			}
			plog.With(ctx).Warnw("msg", "ds_callback_token_sign_failed", "match_id", matchID, "err", terr)
		} else {
			meta.Annotations = map[string]string{dsTokenAnnotationKey: tok}
		}
	}

	pod, addr, actualTrack, err := a.allocateWithMetadata(ctx, matchID, mapID, releaseTrack, meta)
	return pod, addr, actualTrack, err
}

// AllocateAuthoritative 执行 Model B 的 K8s 分配半段：GSA POST 永不携带令牌；选中后必须
// 严格 GET GameServer 取得 UID/resourceVersion，任一字段缺失都 fail-closed。
func (a *AgonesGameServerAllocator) AllocateAuthoritative(
	ctx context.Context,
	matchID uint64,
	allocationID string,
	playerIDs []uint64,
	mapID uint32,
	gameMode, releaseTrack string,
) (*AuthoritativeGameServerAllocation, error) {
	parsedAllocationID, parseErr := uuid.Parse(allocationID)
	if matchID == 0 || parseErr != nil || parsedAllocationID == uuid.Nil ||
		parsedAllocationID.Version() != uuid.Version(4) || parsedAllocationID.String() != allocationID ||
		!releasetrack.Valid(releaseTrack) {
		return nil, errcode.New(errcode.ErrInvalidArg, "agones: match_id and allocation_id required")
	}
	canonicalPlayers, roster, rosterErr := dsmetadata.CanonicalRoster(playerIDs)
	if rosterErr != nil || len(canonicalPlayers) == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "agones: invalid battle roster: %v", rosterErr)
	}
	partial := &AuthoritativeGameServerAllocation{AllocationID: allocationID}
	meta := &gsaMetadata{Labels: map[string]string{
		"pandora.dev/match-id":      fmt.Sprintf("%d", matchID),
		"pandora.dev/map-id":        fmt.Sprintf("%d", mapID),
		"pandora.dev/game-mode":     sanitizeLabelValue(gameMode),
		battleAllocationMetadataKey: sanitizeLabelValue(allocationID),
	}, Annotations: map[string]string{
		battleRosterAnnotationKey:   roster,
		battleAllocationMetadataKey: allocationID,
	}}
	podName, addr, selectedTrack, err := a.allocateWithMetadata(ctx, matchID, mapID, releaseTrack, meta)
	if err != nil {
		// 即使 POST 没有可解析响应，也必须把 allocation_id 交还调用方。它是未知结果
		// 对账/回收的唯一 fencing token；返回 nil 会让调用方删 claim 后再次分配第二个 Pod。
		return partial, err
	}
	partial.PodName, partial.Addr = podName, addr
	gs, err := a.getGameServer(ctx, podName)
	if err != nil {
		return partial, errcode.New(errcode.ErrDSAllocationFailed,
			"agones: strict GET selected gameserver %s failed: %v", podName, err)
	}
	actualReleaseTrack := gs.Metadata.Labels[releaseTrackMetadataKey]
	if gs.Metadata.Name != podName || gs.Metadata.UID == "" || gs.Metadata.ResourceVersion == "" ||
		gs.Metadata.Labels["pandora.dev/match-id"] != strconv.FormatUint(matchID, 10) ||
		gs.Metadata.Labels[battleAllocationMetadataKey] != sanitizeLabelValue(allocationID) ||
		gs.Metadata.Annotations[battleAllocationMetadataKey] != allocationID ||
		gs.Metadata.Annotations[battleRosterAnnotationKey] != roster ||
		!releasetrack.Valid(actualReleaseTrack) || actualReleaseTrack != selectedTrack ||
		gs.Metadata.Annotations[releaseTrackMetadataKey] != actualReleaseTrack {
		return partial, errcode.New(errcode.ErrDSAllocationFailed,
			"agones: selected gameserver identity/binding incomplete: want_name=%q name=%q uid=%q rv=%q",
			podName, gs.Metadata.Name, gs.Metadata.UID, gs.Metadata.ResourceVersion)
	}
	return &AuthoritativeGameServerAllocation{
		PodName:            podName,
		Addr:               addr,
		InstanceUID:        gs.Metadata.UID,
		ResourceVersion:    gs.Metadata.ResourceVersion,
		AllocationID:       allocationID,
		ReleaseTrack:       actualReleaseTrack,
		AnnotationsPresent: gs.Metadata.Annotations != nil,
	}, nil
}

func (a *AgonesGameServerAllocator) allocateWithMetadata(
	ctx context.Context,
	matchID uint64,
	mapID uint32,
	desiredReleaseTrack string,
	meta *gsaMetadata,
) (string, string, string, error) {
	if !releasetrack.Valid(desiredReleaseTrack) {
		return "", "", "", errcode.New(errcode.ErrInvalidArg,
			"agones: invalid desired release track %q", desiredReleaseTrack)
	}
	tracks := []string{desiredReleaseTrack}
	if desiredReleaseTrack == releasetrack.Canary {
		// 只有明确的 UnAllocated/NoAvailable 才进入第二次 stable POST；transport/
		// decode 等结果未知时立即停，绝不冒险产生第二个已分配 GameServer。
		tracks = append(tracks, releasetrack.Stable)
	}
	for i, track := range tracks {
		attemptMeta := cloneGSAMetadata(meta)
		attemptMeta.Labels[releaseTrackMetadataKey] = track
		attemptMeta.Annotations[releaseTrackMetadataKey] = track
		pod, addr, err := a.allocateOnceWithMetadata(ctx, matchID, mapID, track, attemptMeta)
		if err == nil {
			return pod, addr, track, nil
		}
		if errcode.As(err) != errcode.ErrDSNoAvailable || i == len(tracks)-1 {
			return "", "", "", err
		}
		plog.With(ctx).Warnw("msg", "battle_canary_capacity_fallback_stable",
			"match_id", matchID, "map_id", mapID)
	}
	return "", "", "", errcode.New(errcode.ErrDSNoAvailable, "agones: no gameserver")
}

func cloneGSAMetadata(meta *gsaMetadata) *gsaMetadata {
	out := &gsaMetadata{Labels: make(map[string]string), Annotations: make(map[string]string)}
	if meta == nil {
		return out
	}
	for key, value := range meta.Labels {
		out.Labels[key] = value
	}
	for key, value := range meta.Annotations {
		out.Annotations[key] = value
	}
	return out
}

func (a *AgonesGameServerAllocator) allocateOnceWithMetadata(
	ctx context.Context,
	matchID uint64,
	mapID uint32,
	releaseTrack string,
	meta *gsaMetadata,
) (string, string, error) {
	generalFleet := a.fleetName
	dedicatedFleet := a.mapFleets[mapID].stable
	if releaseTrack == releasetrack.Canary {
		generalFleet = a.canaryFleetName
		dedicatedFleet = a.mapFleets[mapID].canary
	}
	if generalFleet == "" {
		return "", "", errcode.New(errcode.ErrDSNoAvailable,
			"agones: no %s fleet configured", releaseTrack)
	}
	selectorLabels := func(fleet string) map[string]string {
		return map[string]string{fleetLabelKey: fleet, releaseTrackMetadataKey: releaseTrack}
	}
	selectors := make([]gsaSelector, 0, 2)
	if dedicatedFleet != "" && dedicatedFleet != generalFleet {
		selectors = append(selectors, gsaSelector{MatchLabels: selectorLabels(dedicatedFleet)})
	}
	selectors = append(selectors, gsaSelector{MatchLabels: selectorLabels(generalFleet)})

	reqBody := gsaRequest{
		APIVersion: "allocation.agones.dev/v1",
		Kind:       "GameServerAllocation",
		Spec: gsaSpec{
			Selectors: selectors,
			// 把业务标识打到被分配的 GameServer 上,便于运维 / 排障关联对局。
			Metadata: meta,
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

// DeliverCredential 用 UID+resourceVersion JSON Patch 投递 Redis pending 的镜像。PATCH 的
// HTTP 结果从不直接作为成功依据：无论 2xx 空/坏响应、409，还是 transport timeout，均再做
// 一次严格 GET；只有 UID 未变且全部 annotation 与期望精确相等才成功，绝不本地 fallback。
func (a *AgonesGameServerAllocator) DeliverCredential(
	ctx context.Context,
	allocation *AuthoritativeGameServerAllocation,
	annotations map[string]string,
) (string, error) {
	if allocation == nil || allocation.PodName == "" || allocation.InstanceUID == "" ||
		allocation.ResourceVersion == "" || len(annotations) == 0 {
		return "", errcode.New(errcode.ErrInvalidArg, "agones: incomplete credential delivery input")
	}
	for k, v := range annotations {
		if k == "" || v == "" {
			return "", errcode.New(errcode.ErrInvalidArg,
				"agones: credential annotation key/value must be non-empty")
		}
	}
	ops := []jsonPatchOperation{
		{Op: "test", Path: "/metadata/uid", Value: allocation.InstanceUID},
		{Op: "test", Path: "/metadata/resourceVersion", Value: allocation.ResourceVersion},
	}
	if !allocation.AnnotationsPresent {
		ops = append(ops, jsonPatchOperation{Op: "add", Path: "/metadata/annotations", Value: annotations})
	} else {
		keys := make([]string, 0, len(annotations))
		for key := range annotations {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			pathKey := strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
			ops = append(ops, jsonPatchOperation{Op: "add", Path: "/metadata/annotations/" + pathKey, Value: annotations[key]})
		}
	}
	payload, err := json.Marshal(ops)
	if err != nil {
		return "", errcode.New(errcode.ErrDSAllocationFailed, "agones: marshal credential patch: %v", err)
	}
	url := fmt.Sprintf("%s/apis/agones.dev/v1/namespaces/%s/gameservers/%s",
		a.apiServer, a.namespace, allocation.PodName)
	patchBody, patchStatus, patchErr := a.doWithContentType(
		ctx, http.MethodPatch, url, payload, "application/json-patch+json")

	// PATCH 结果未知时，确认读不能复用一个可能已因入站超时/取消而失效的 ctx。
	// 独立确认仍由 do() 的 allocateTimeout 严格限时；它只读 K8s 当前事实，不延长业务写。
	confirmed, confirmErr := a.getGameServer(context.WithoutCancel(ctx), allocation.PodName)
	if confirmErr == nil {
		confirmErr = confirmCredentialDelivery(confirmed, allocation, annotations)
	}
	if confirmErr == nil {
		return confirmed.Metadata.ResourceVersion, nil
	}
	return "", errcode.New(errcode.ErrDSAllocationFailed,
		"agones: credential PATCH not strictly confirmed: patch_status=%d patch_err=%v patch_body=%q confirm_err=%v",
		patchStatus, patchErr, truncate(patchBody, 256), confirmErr)
}

func (a *AgonesGameServerAllocator) getGameServer(ctx context.Context, podName string) (*gameServerResponse, error) {
	url := fmt.Sprintf("%s/apis/agones.dev/v1/namespaces/%s/gameservers/%s",
		a.apiServer, a.namespace, podName)
	body, status, err := a.do(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("GET gameserver http %d: %s", status, truncate(body, 256))
	}
	var gs gameServerResponse
	if err := json.Unmarshal(body, &gs); err != nil {
		return nil, fmt.Errorf("decode gameserver: %w", err)
	}
	return &gs, nil
}

func confirmCredentialDelivery(
	gs *gameServerResponse,
	allocation *AuthoritativeGameServerAllocation,
	annotations map[string]string,
) error {
	if gs == nil || gs.Metadata.Name != allocation.PodName || gs.Metadata.UID != allocation.InstanceUID ||
		gs.Metadata.ResourceVersion == "" {
		return fmt.Errorf("gameserver identity mismatch or incomplete")
	}
	for key, want := range annotations {
		if got := gs.Metadata.Annotations[key]; got != want {
			return fmt.Errorf("annotation %q mismatch: got=%q", key, got)
		}
	}
	return nil
}

// ReleaseExpected 只删除 UID 仍等于本次分配实例的 GameServer。同名对象重建后 UID 变化，
// apiserver 会拒绝 DeleteOptions precondition，旧 cleanup 不能误杀新实例。
func (a *AgonesGameServerAllocator) ReleaseExpected(
	ctx context.Context,
	allocation *AuthoritativeGameServerAllocation,
) error {
	if allocation == nil || (allocation.InstanceUID == "" && allocation.AllocationID == "") {
		return errcode.New(errcode.ErrInvalidArg,
			"agones: expected release requires uid or allocation_id")
	}
	if allocation.InstanceUID == "" {
		// POST 已选中但 UID GET 不确定时，不能按名字删除。allocation_id 是本次 GSA 写入
		// 选中对象的唯一 UUID label，用 DeleteCollection 精确回收；同名重建的新对象不会
		// 带旧 allocation_id，因此旧 cleanup 不会误杀新实例。
		selector := "pandora.dev/allocation-id=" + sanitizeLabelValue(allocation.AllocationID)
		deleteBody, err := json.Marshal(deleteOptions{APIVersion: "v1", Kind: "DeleteOptions"})
		if err != nil {
			return errcode.New(errcode.ErrDSAllocationFailed, "agones: marshal allocation delete: %v", err)
		}
		collectionURL := fmt.Sprintf("%s/apis/agones.dev/v1/namespaces/%s/gameservers?labelSelector=%s",
			a.apiServer, a.namespace, url.QueryEscape(selector))
		deleteResp, deleteStatus, deleteErr := a.do(ctx, http.MethodDelete, collectionURL, deleteBody)
		// DeleteCollection 的响应/超时同样不构成完成证据；严格 LIST 确认该唯一 label
		// 已无对象。timeout-but-applied 可幂等成功，2xx 但仍有对象则保留 Redis claim。
		listBody, listStatus, listErr := a.do(ctx, http.MethodGet, collectionURL, nil)
		if listErr == nil && listStatus >= 200 && listStatus < 300 {
			var list gameServerListResponse
			if uerr := json.Unmarshal(listBody, &list); uerr == nil && len(list.Items) == 0 {
				return nil
			}
		}
		return errcode.New(errcode.ErrDSAllocationFailed,
			"agones: allocation-id release not confirmed: allocation_id=%s delete_status=%d delete_err=%v delete_body=%q list_status=%d list_err=%v list_body=%q",
			allocation.AllocationID, deleteStatus, deleteErr, truncate(deleteResp, 128),
			listStatus, listErr, truncate(listBody, 128))
	}
	if allocation.PodName == "" {
		return errcode.New(errcode.ErrInvalidArg, "agones: UID release requires gameserver name")
	}
	body, err := json.Marshal(deleteOptions{
		APIVersion: "v1", Kind: "DeleteOptions",
		Preconditions: &deletePreconditions{UID: allocation.InstanceUID},
	})
	if err != nil {
		return errcode.New(errcode.ErrDSAllocationFailed, "agones: marshal expected delete: %v", err)
	}
	url := fmt.Sprintf("%s/apis/agones.dev/v1/namespaces/%s/gameservers/%s",
		a.apiServer, a.namespace, allocation.PodName)
	respBody, status, err := a.do(ctx, http.MethodDelete, url, body)
	if err != nil {
		return errcode.New(errcode.ErrDSAllocationFailed,
			"agones: expected release %s uid=%s: %v", allocation.PodName, allocation.InstanceUID, err)
	}
	if status == http.StatusNotFound {
		return nil
	}
	if status < 200 || status >= 300 {
		return errcode.New(errcode.ErrDSAllocationFailed,
			"agones: expected release %s uid=%s http %d: %s",
			allocation.PodName, allocation.InstanceUID, status, truncate(respBody, 256))
	}
	return nil
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
	out := make([]string, 0, 2+2*len(a.mapFleets))
	seen := make(map[string]bool)
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	add(a.fleetName)
	add(a.canaryFleetName)
	dedicated := make([]string, 0, 2*len(a.mapFleets))
	for _, route := range a.mapFleets {
		for _, name := range []string{route.stable, route.canary} {
			if name != "" && !seen[name] {
				seen[name] = true
				dedicated = append(dedicated, name)
			}
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
	return a.doWithContentType(ctx, method, url, body, "application/json")
}

func (a *AgonesGameServerAllocator) doWithContentType(
	ctx context.Context,
	method, url string,
	body []byte,
	contentType string,
) ([]byte, int, error) {
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
		req.Header.Set("Content-Type", contentType)
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
