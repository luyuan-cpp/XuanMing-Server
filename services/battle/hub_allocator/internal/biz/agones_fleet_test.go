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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
