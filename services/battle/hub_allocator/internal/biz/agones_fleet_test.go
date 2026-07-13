// agones_fleet_test.go — AgonesHubFleetProvider 单测(W4 ⑬)。
//
// 用 httptest 模拟 k8s apiserver,不连真集群:
//   - ListShards: 校验请求方法/路径/labelSelector + 解析 GameServer 列表 → ShardCandidate
//   - ListShards: 过滤非 Ready/无地址的 GameServer
//   - ListShards: apiserver 5xx → error
//   - shard_id / capacity 标签解析与 fallback
package biz

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/conf"
)

func newTestFleetProvider(t *testing.T, serverURL string) *AgonesHubFleetProvider {
	t.Helper()
	cfg := conf.Config{}
	cfg.Hub.DefaultCapacity = 500
	cfg.Agones = conf.AgonesConf{
		Enabled:   true,
		APIServer: serverURL,
		Namespace: "pandora",
		FleetName: "pandora-hub",
		TokenPath: "-", // 不带 token
	}
	p, err := NewAgonesHubFleetProvider(cfg)
	if err != nil {
		t.Fatalf("NewAgonesHubFleetProvider: %v", err)
	}
	return p
}

func TestNewAgonesHubFleetProvider_RequiresFleet(t *testing.T) {
	cfg := conf.Config{}
	cfg.Agones = conf.AgonesConf{Enabled: true}
	if _, err := NewAgonesHubFleetProvider(cfg); err == nil {
		t.Fatal("expected error when fleet_name empty, got nil")
	}
}

func TestAgonesListShards_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method: got %s want GET", r.Method)
		}
		wantPath := "/apis/agones.dev/v1/namespaces/pandora/gameservers"
		if r.URL.Path != wantPath {
			t.Errorf("path: got %s want %s", r.URL.Path, wantPath)
		}
		sel := r.URL.Query().Get("labelSelector")
		if !strings.Contains(sel, "agones.dev/fleet=pandora-hub") {
			t.Errorf("labelSelector missing fleet: %s", sel)
		}
		if !strings.Contains(sel, "pandora.dev/region=cn") {
			t.Errorf("labelSelector missing region: %s", sel)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{ // Ready + 显式 shard-id/capacity 标签
					"metadata": map[string]any{
						"name": "pandora-hub-aaa",
						"labels": map[string]any{
							"pandora.dev/shard-id": "7",
							"pandora.dev/capacity": "300",
						},
					},
					"status": map[string]any{
						"state":   "Ready",
						"address": "10.0.0.5",
						"ports":   []map[string]any{{"name": "default", "port": 7777}},
					},
				},
				{ // Ready 但无标签 → shard_id 哈希派生 / capacity fallback
					"metadata": map[string]any{"name": "pandora-hub-bbb"},
					"status": map[string]any{
						"state":   "Allocated",
						"address": "10.0.0.6",
						"ports":   []map[string]any{{"name": "default", "port": 7778}},
					},
				},
				{ // Scheduled(未就绪)→ 过滤
					"metadata": map[string]any{"name": "pandora-hub-ccc"},
					"status":   map[string]any{"state": "Scheduled"},
				},
				{ // Ready 但无 address → 过滤
					"metadata": map[string]any{"name": "pandora-hub-ddd"},
					"status":   map[string]any{"state": "Ready"},
				},
			},
		})
	}))
	defer srv.Close()

	p := newTestFleetProvider(t, srv.URL)
	shards, err := p.ListShards(context.Background(), "cn")
	if err != nil {
		t.Fatalf("ListShards: %v", err)
	}
	if len(shards) != 2 {
		t.Fatalf("shard count: got %d want 2 (%+v)", len(shards), shards)
	}

	s0 := shards[0]
	if s0.PodName != "pandora-hub-aaa" || s0.Addr != "10.0.0.5:7777" {
		t.Errorf("shard0: got pod=%s addr=%s", s0.PodName, s0.Addr)
	}
	if s0.ShardID != 7 {
		t.Errorf("shard0 shard_id: got %d want 7 (from label)", s0.ShardID)
	}
	if s0.Capacity != 300 {
		t.Errorf("shard0 capacity: got %d want 300 (from label)", s0.Capacity)
	}
	if s0.Region != "cn" {
		t.Errorf("shard0 region: got %s want cn", s0.Region)
	}

	s1 := shards[1]
	if s1.PodName != "pandora-hub-bbb" || s1.Addr != "10.0.0.6:7778" {
		t.Errorf("shard1: got pod=%s addr=%s", s1.PodName, s1.Addr)
	}
	if s1.ShardID == 0 {
		t.Errorf("shard1 shard_id: hash-derived must be non-zero")
	}
	if s1.Capacity != 500 {
		t.Errorf("shard1 capacity: got %d want 500 (fallback)", s1.Capacity)
	}
}

// TestAgonesListShards_EnsureDSToken:发现式签发(审核 P1 #1)。
//   - GameServer 无 ds-token annotation → 签发并 PATCH(merge-patch 两个 annotation)
//   - annotation 在且剩余寿命充足 → 不 PATCH
//   - annotation 在但剩余寿命 < renewBefore → 重签 PATCH 续期
func TestAgonesListShards_EnsureDSToken(t *testing.T) {
	type patchCall struct {
		path string
		body string
	}
	var patches []patchCall
	nowMs := time.Now().UnixMilli()
	freshExp := strconv.FormatInt(nowMs+20*int64(time.Hour/time.Millisecond), 10) // 剩 20h,充足
	staleExp := strconv.FormatInt(nowMs+1*int64(time.Hour/time.Millisecond), 10)  // 剩 1h < 8h,须续期

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			body, _ := io.ReadAll(r.Body)
			patches = append(patches, patchCall{path: r.URL.Path, body: string(body)})
			if ct := r.Header.Get("Content-Type"); ct != "application/merge-patch+json" {
				t.Errorf("patch content-type: got %s", ct)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{ // 无 annotation → 签发
					"metadata": map[string]any{"name": "hub-no-token"},
					"status": map[string]any{
						"state": "Ready", "address": "10.0.0.1",
						"ports": []map[string]any{{"name": "default", "port": 7777}},
					},
				},
				{ // 有 token 且寿命充足 → 不动
					"metadata": map[string]any{
						"name": "hub-fresh",
						"annotations": map[string]any{
							"pandora.dev/ds-token":        "tok-fresh",
							"pandora.dev/ds-token-exp-ms": freshExp,
						},
					},
					"status": map[string]any{
						"state": "Ready", "address": "10.0.0.2",
						"ports": []map[string]any{{"name": "default", "port": 7777}},
					},
				},
				{ // 有 token 但剩余寿命不足 → 续期
					"metadata": map[string]any{
						"name": "hub-stale",
						"annotations": map[string]any{
							"pandora.dev/ds-token":        "tok-stale",
							"pandora.dev/ds-token-exp-ms": staleExp,
						},
					},
					"status": map[string]any{
						"state": "Ready", "address": "10.0.0.3",
						"ports": []map[string]any{{"name": "default", "port": 7777}},
					},
				},
			},
		})
	}))
	defer srv.Close()

	p := newTestFleetProvider(t, srv.URL)
	p.SetDSTokenIssuer(func(pod string) (string, int64, uint64, error) {
		return "tok-for-" + pod, nowMs + 24*int64(time.Hour/time.Millisecond), 0, nil
	}, 8*time.Hour, false)

	shards, err := p.ListShards(context.Background(), "")
	if err != nil {
		t.Fatalf("ListShards: %v", err)
	}
	if len(shards) != 3 {
		t.Fatalf("shard count: got %d want 3", len(shards))
	}
	if len(patches) != 2 {
		t.Fatalf("patch count: got %d want 2 (%+v)", len(patches), patches)
	}
	if !strings.HasSuffix(patches[0].path, "/gameservers/hub-no-token") ||
		!strings.Contains(patches[0].body, "tok-for-hub-no-token") {
		t.Errorf("patch0 wrong: %+v", patches[0])
	}
	if !strings.HasSuffix(patches[1].path, "/gameservers/hub-stale") ||
		!strings.Contains(patches[1].body, "tok-for-hub-stale") {
		t.Errorf("patch1 wrong: %+v", patches[1])
	}
	for _, pc := range patches {
		if !strings.Contains(pc.body, "pandora.dev/ds-token-exp-ms") {
			t.Errorf("patch missing exp annotation: %s", pc.body)
		}
	}
}

// 审核 P1(1/2):enforce 下,寿命充足但 annotation **缺合法代际**的令牌必须强制重签补齐 gen,
// 挡「legacy gen0 令牌被当有效而关闭代际门」。重签 PATCH 必须写入 pandora.dev/ds-token-gen。
func TestAgonesEnsureDSToken_EnforceResignWhenMissingGen(t *testing.T) {
	var patches []string
	nowMs := time.Now().UnixMilli()
	freshExp := strconv.FormatInt(nowMs+20*int64(time.Hour/time.Millisecond), 10) // 剩 20h,寿命充足

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			body, _ := io.ReadAll(r.Body)
			patches = append(patches, string(body))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{ // 寿命充足但**无 gen annotation**(legacy):enforce 下须强制重签
					"metadata": map[string]any{
						"name": "hub-legacy-nogen",
						"annotations": map[string]any{
							"pandora.dev/ds-token":        "tok-legacy",
							"pandora.dev/ds-token-exp-ms": freshExp,
						},
					},
					"status": map[string]any{
						"state": "Ready", "address": "10.0.0.9",
						"ports": []map[string]any{{"name": "default", "port": 7777}},
					},
				},
			},
		})
	}))
	defer srv.Close()

	p := newTestFleetProvider(t, srv.URL)
	// enforce(required=true);issuer 发新代际 9。
	p.SetDSTokenIssuer(func(pod string) (string, int64, uint64, error) {
		return "tok-for-" + pod, nowMs + 24*int64(time.Hour/time.Millisecond), 9, nil
	}, 8*time.Hour, true)

	shards, err := p.ListShards(context.Background(), "")
	if err != nil {
		t.Fatalf("ListShards: %v", err)
	}
	if len(patches) != 1 {
		t.Fatalf("enforce+missing-gen must force resign PATCH, got %d patches", len(patches))
	}
	if !strings.Contains(patches[0], "pandora.dev/ds-token-gen") || !strings.Contains(patches[0], "\"9\"") {
		t.Fatalf("resign PATCH must write ds-token-gen=9: %s", patches[0])
	}
	if len(shards) != 1 || shards[0].TokenGen != 9 || !shards[0].TokenReady {
		t.Fatalf("resigned shard must carry gen=9 & TokenReady: %+v", shards)
	}
}

// 审核 P1(9/10/11/12/20):两副本乱序写同一 GameServer 时,resourceVersion CAS 必须挡住
// 「后到低代际 PATCH 覆盖」。模拟:首个 PATCH 返回 409 Conflict → 冲突方重读对象 → 重读结果里
// 对方已写好当前代际(gen=7)有效令牌 → 直接复用不再重签,只发生 1 次 PATCH。
func TestAgonesEnsureDSToken_ResourceVersionConflictReread(t *testing.T) {
	var patchCount int
	nowMs := time.Now().UnixMilli()
	staleExp := strconv.FormatInt(nowMs+1*int64(time.Hour/time.Millisecond), 10) // 剩 1h < 8h,须续期
	freshExp := strconv.FormatInt(nowMs+20*int64(time.Hour/time.Millisecond), 10)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPatch:
			patchCount++
			w.WriteHeader(http.StatusConflict) // 首个(也是唯一)PATCH:CAS 冲突
			_, _ = w.Write([]byte(`{"kind":"Status","status":"Failure","reason":"Conflict"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/gameservers/hub-cas"):
			// 冲突后重读:对方已写入当前代际(gen=7)有效令牌 → 本副本应复用,不再重签。
			_ = json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]any{
					"name":            "hub-cas",
					"resourceVersion": "222",
					"annotations": map[string]any{
						"pandora.dev/ds-token":        "tok-peer-gen7",
						"pandora.dev/ds-token-exp-ms": freshExp,
						"pandora.dev/ds-token-gen":    "7",
					},
				},
				"status": map[string]any{
					"state": "Ready", "address": "10.0.0.7",
					"ports": []map[string]any{{"name": "default", "port": 7777}},
				},
			})
		default: // LIST:一个 rv=111、缺 gen 的分片,enforce 下须重签
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"metadata": map[string]any{
							"name":            "hub-cas",
							"resourceVersion": "111",
							"annotations": map[string]any{
								"pandora.dev/ds-token":        "tok-old-nogen",
								"pandora.dev/ds-token-exp-ms": staleExp,
							},
						},
						"status": map[string]any{
							"state": "Ready", "address": "10.0.0.7",
							"ports": []map[string]any{{"name": "default", "port": 7777}},
						},
					},
				},
			})
		}
	}))
	defer srv.Close()

	p := newTestFleetProvider(t, srv.URL)
	p.SetDSTokenIssuer(func(pod string) (string, int64, uint64, error) {
		return "tok-for-" + pod, nowMs + 24*int64(time.Hour/time.Millisecond), 99, nil
	}, 8*time.Hour, true)

	shards, err := p.ListShards(context.Background(), "")
	if err != nil {
		t.Fatalf("ListShards: %v", err)
	}
	if patchCount != 1 {
		t.Fatalf("CAS conflict must lead to exactly 1 PATCH then reread-reuse, got %d", patchCount)
	}
	if len(shards) != 1 || shards[0].TokenGen != 7 || !shards[0].TokenReady {
		t.Fatalf("after CAS conflict must reuse peer gen=7: %+v", shards)
	}
}

func TestAgonesListShards_NoRegionSelector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sel := r.URL.Query().Get("labelSelector")
		if strings.Contains(sel, "pandora.dev/region") {
			t.Errorf("region empty should not add region selector: %s", sel)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
	}))
	defer srv.Close()

	p := newTestFleetProvider(t, srv.URL)
	shards, err := p.ListShards(context.Background(), "")
	if err != nil {
		t.Fatalf("ListShards: %v", err)
	}
	if len(shards) != 0 {
		t.Errorf("shard count: got %d want 0", len(shards))
	}
}

func TestAgonesListShards_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := newTestFleetProvider(t, srv.URL)
	if _, err := p.ListShards(context.Background(), "cn"); err == nil {
		t.Fatal("expected error on apiserver 5xx, got nil")
	}
}

func TestShardIDFor_LabelOverridesHash(t *testing.T) {
	withLabel := &gameServer{}
	withLabel.Metadata.Name = "pandora-hub-x"
	withLabel.Metadata.Labels = map[string]string{shardIDLabelKey: "42"}
	if got := shardIDFor(withLabel); got != 42 {
		t.Errorf("label shard_id: got %d want 42", got)
	}

	noLabel := &gameServer{}
	noLabel.Metadata.Name = "pandora-hub-x"
	if got := shardIDFor(noLabel); got == 0 {
		t.Error("hash-derived shard_id must be non-zero")
	}
	// 同 pod 名哈希稳定
	noLabel2 := &gameServer{}
	noLabel2.Metadata.Name = "pandora-hub-x"
	if shardIDFor(noLabel) != shardIDFor(noLabel2) {
		t.Error("hash-derived shard_id must be stable for same pod name")
	}
}
