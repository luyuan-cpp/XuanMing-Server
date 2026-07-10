// agones_allocator_test.go — AgonesGameServerAllocator 单测(W4 ⑫)。
//
// 用 httptest 模拟 k8s apiserver,不连真集群:
//   - Allocate: 校验请求方法/路径/body selector + 解析 Allocated status → podName/addr
//   - Allocate: status=UnAllocated → ErrDSNoAvailable
//   - Allocate: apiserver 5xx → ErrDSAllocationFailed
//   - Release: DELETE 正确路径 → nil;404 → nil(幂等)
package data

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/conf"
)

func newTestAllocator(t *testing.T, serverURL string) *AgonesGameServerAllocator {
	t.Helper()
	a, err := NewAgonesGameServerAllocator(conf.AgonesConf{
		Enabled:   true,
		APIServer: serverURL,
		Namespace: "pandora",
		FleetName: "battle-fleet",
		TokenPath: "-", // 不带 token
	})
	if err != nil {
		t.Fatalf("NewAgonesGameServerAllocator: %v", err)
	}
	return a
}

// TestWatchedFleets 校验容量巡检盯的 Fleet 集合:通用池在前 + 专属池去重按字典序。
func TestWatchedFleets(t *testing.T) {
	a, err := NewAgonesGameServerAllocator(conf.AgonesConf{
		Enabled:   true,
		APIServer: "http://127.0.0.1:1",
		Namespace: "pandora",
		FleetName: "battle-fleet",
		TokenPath: "-",
		MapFleets: []conf.AgonesMapFleet{
			{MapID: 7, FleetName: "songlin-fleet"},
			{MapID: 8, FleetName: "arena-fleet"},
			{MapID: 9, FleetName: "battle-fleet"}, // 与通用池重名 → 去重
		},
	})
	if err != nil {
		t.Fatalf("NewAgonesGameServerAllocator: %v", err)
	}
	got := a.WatchedFleets()
	want := []string{"battle-fleet", "arena-fleet", "songlin-fleet"}
	if len(got) != len(want) {
		t.Fatalf("WatchedFleets len: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("WatchedFleets[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

// TestListFleetCapacities_OK 解析 Fleet status 容量快照,并对每个受管 Fleet 各发一次 GET。
func TestListFleetCapacities_OK(t *testing.T) {
	byFleet := map[string]map[string]any{
		"battle-fleet":  {"replicas": 10, "readyReplicas": 3, "allocatedReplicas": 7},
		"songlin-fleet": {"replicas": 4, "readyReplicas": 0, "allocatedReplicas": 4},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method: got %s want GET", r.Method)
		}
		// 路径尾段是 fleet 名
		parts := strings.Split(r.URL.Path, "/")
		fleet := parts[len(parts)-1]
		st, ok := byFleet[fleet]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": st})
	}))
	defer srv.Close()

	a, err := NewAgonesGameServerAllocator(conf.AgonesConf{
		Enabled:   true,
		APIServer: srv.URL,
		Namespace: "pandora",
		FleetName: "battle-fleet",
		TokenPath: "-",
		MapFleets: []conf.AgonesMapFleet{{MapID: 7, FleetName: "songlin-fleet"}},
	})
	if err != nil {
		t.Fatalf("NewAgonesGameServerAllocator: %v", err)
	}

	caps, err := a.ListFleetCapacities(context.Background())
	if err != nil {
		t.Fatalf("ListFleetCapacities: %v", err)
	}
	if len(caps) != 2 {
		t.Fatalf("caps len: got %d want 2", len(caps))
	}
	got := map[string]FleetCapacity{}
	for _, c := range caps {
		got[c.Fleet] = c
	}
	if c := got["battle-fleet"]; c.Replicas != 10 || c.Ready != 3 || c.Allocated != 7 {
		t.Errorf("battle-fleet: got %+v want replicas=10 ready=3 allocated=7", c)
	}
	if c := got["songlin-fleet"]; c.Replicas != 4 || c.Ready != 0 || c.Allocated != 4 {
		t.Errorf("songlin-fleet: got %+v want replicas=4 ready=0 allocated=4", c)
	}
}

// TestListFleetCapacities_PartialFailure 单 Fleet 5xx 不影响其余,错误经 error 汇总返回。
func TestListFleetCapacities_PartialFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "songlin-fleet") {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": map[string]any{"replicas": 5, "readyReplicas": 5, "allocatedReplicas": 0},
		})
	}))
	defer srv.Close()

	a, err := NewAgonesGameServerAllocator(conf.AgonesConf{
		Enabled:   true,
		APIServer: srv.URL,
		Namespace: "pandora",
		FleetName: "battle-fleet",
		TokenPath: "-",
		MapFleets: []conf.AgonesMapFleet{{MapID: 7, FleetName: "songlin-fleet"}},
	})
	if err != nil {
		t.Fatalf("NewAgonesGameServerAllocator: %v", err)
	}

	caps, err := a.ListFleetCapacities(context.Background())
	if err == nil {
		t.Fatal("expected aggregated error for failing fleet, got nil")
	}
	if len(caps) != 1 || caps[0].Fleet != "battle-fleet" {
		t.Fatalf("expected 1 successful cap(battle-fleet), got %+v", caps)
	}
}

func TestNewAgonesGameServerAllocator_RequiresFleet(t *testing.T) {
	if _, err := NewAgonesGameServerAllocator(conf.AgonesConf{Enabled: true}); err == nil {
		t.Fatal("expected error when fleet_name empty, got nil")
	}
}

func TestAgonesAllocate_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method: got %s want POST", r.Method)
		}
		wantPath := "/apis/allocation.agones.dev/v1/namespaces/pandora/gameserverallocations"
		if r.URL.Path != wantPath {
			t.Errorf("path: got %s want %s", r.URL.Path, wantPath)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "agones.dev/fleet") || !strings.Contains(string(body), "battle-fleet") {
			t.Errorf("request body missing fleet selector: %s", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": map[string]any{
				"state":          "Allocated",
				"gameServerName": "battle-fleet-abc12",
				"address":        "10.0.0.7",
				"ports":          []map[string]any{{"name": "default", "port": 7777}},
			},
		})
	}))
	defer srv.Close()

	a := newTestAllocator(t, srv.URL)
	pod, addr, err := a.Allocate(context.Background(), 12345, 2, "moba_5v5")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if pod != "battle-fleet-abc12" {
		t.Errorf("pod: got %q want battle-fleet-abc12", pod)
	}
	if addr != "10.0.0.7:7777" {
		t.Errorf("addr: got %q want 10.0.0.7:7777", addr)
	}
}

func TestAgonesAllocate_NoAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": map[string]any{"state": "UnAllocated"},
		})
	}))
	defer srv.Close()

	a := newTestAllocator(t, srv.URL)
	_, _, err := a.Allocate(context.Background(), 1, 1, "moba")
	if err == nil {
		t.Fatal("expected ErrDSNoAvailable, got nil")
	}
	if got := errcode.As(err); got != errcode.ErrDSNoAvailable {
		t.Errorf("code: got %d want ErrDSNoAvailable(5001)", got)
	}
}

// TestAgonesAllocate_MapFleetSelectorOrder 校验混合形态路由:
//   - map_id 命中 map_fleets → selectors 有序 [专属预热 Fleet, 通用 Fleet](Agones 按序尝试);
//   - 未命中 → 仅通用 Fleet 一个 selector(行为与未配置 map_fleets 完全一致)。
func TestAgonesAllocate_MapFleetSelectorOrder(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": map[string]any{
				"state":          "Allocated",
				"gameServerName": "songlin-fleet-x1",
				"address":        "10.0.0.9",
				"ports":          []map[string]any{{"name": "default", "port": 7788}},
			},
		})
	}))
	defer srv.Close()

	a, err := NewAgonesGameServerAllocator(conf.AgonesConf{
		Enabled:   true,
		APIServer: srv.URL,
		Namespace: "pandora",
		FleetName: "battle-fleet",
		TokenPath: "-",
		MapFleets: []conf.AgonesMapFleet{{MapID: 7, FleetName: "songlin-fleet"}},
	})
	if err != nil {
		t.Fatalf("NewAgonesGameServerAllocator: %v", err)
	}

	// 命中 map_id=7:两个 selector,专属在前、通用兜底在后。
	if _, _, err := a.Allocate(context.Background(), 1, 7, "pve_coop"); err != nil {
		t.Fatalf("Allocate(map 7): %v", err)
	}
	var req struct {
		Spec struct {
			Selectors []struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"selectors"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if len(req.Spec.Selectors) != 2 {
		t.Fatalf("selectors: got %d want 2 (dedicated first, generic fallback)", len(req.Spec.Selectors))
	}
	if got := req.Spec.Selectors[0].MatchLabels["agones.dev/fleet"]; got != "songlin-fleet" {
		t.Errorf("selector[0]: got %q want songlin-fleet", got)
	}
	if got := req.Spec.Selectors[1].MatchLabels["agones.dev/fleet"]; got != "battle-fleet" {
		t.Errorf("selector[1]: got %q want battle-fleet", got)
	}

	// 未命中 map_id=6:只有通用 selector。
	if _, _, err := a.Allocate(context.Background(), 2, 6, "pvp_5v5"); err != nil {
		t.Fatalf("Allocate(map 6): %v", err)
	}
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if len(req.Spec.Selectors) != 1 {
		t.Fatalf("selectors: got %d want 1 (generic only)", len(req.Spec.Selectors))
	}
	if got := req.Spec.Selectors[0].MatchLabels["agones.dev/fleet"]; got != "battle-fleet" {
		t.Errorf("selector[0]: got %q want battle-fleet", got)
	}
}

func TestAgonesAllocate_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := newTestAllocator(t, srv.URL)
	_, _, err := a.Allocate(context.Background(), 1, 1, "moba")
	if err == nil {
		t.Fatal("expected ErrDSAllocationFailed, got nil")
	}
	if got := errcode.As(err); got != errcode.ErrDSAllocationFailed {
		t.Errorf("code: got %d want ErrDSAllocationFailed(5002)", got)
	}
}

func TestAgonesRelease_OK(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := newTestAllocator(t, srv.URL)
	if err := a.Release(context.Background(), "battle-fleet-abc12"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method: got %s want DELETE", gotMethod)
	}
	wantPath := "/apis/agones.dev/v1/namespaces/pandora/gameservers/battle-fleet-abc12"
	if gotPath != wantPath {
		t.Errorf("path: got %s want %s", gotPath, wantPath)
	}
}

func TestAgonesRelease_NotFoundIdempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	a := newTestAllocator(t, srv.URL)
	if err := a.Release(context.Background(), "ghost-gs"); err != nil {
		t.Fatalf("Release on 404 should be nil(idempotent), got %v", err)
	}
}

func TestAgonesRelease_EmptyPodNoop(t *testing.T) {
	a := newTestAllocator(t, "http://127.0.0.1:1") // 不会被调用
	if err := a.Release(context.Background(), ""); err != nil {
		t.Fatalf("Release(\"\") should be noop nil, got %v", err)
	}
}

func TestSanitizeLabelValue(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"moba_5v5", "moba_5v5"},
		{" mode/5v5 ", "mode-5v5"},
		{"---", "unknown"},
		{strings.Repeat("a", 70), strings.Repeat("a", 63)},
	}
	for _, c := range cases {
		if got := sanitizeLabelValue(c.in); got != c.want {
			t.Errorf("sanitizeLabelValue(%q): got %q want %q", c.in, got, c.want)
		}
	}
}
