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
	"sync/atomic"
	"testing"
	"time"

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

// TestAgonesAllocate_DSTokenAnnotation:注入 dsTokenIssuer 后,分配请求的 metadata.annotations
// 必须携带 pandora.dev/ds-token(DS 回调服务令牌下发通道,审核 P1 #1);未注入时不出现该字段。
func TestAgonesAllocate_DSTokenAnnotation(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
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

	// 未注入 issuer:不带 annotations。
	a := newTestAllocator(t, srv.URL)
	if _, _, err := a.Allocate(context.Background(), 42, 1, "moba"); err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if strings.Contains(string(gotBody), "pandora.dev/ds-token") {
		t.Errorf("annotations must be absent without issuer: %s", gotBody)
	}

	// 注入 issuer:annotation 带上令牌。
	a.SetDSTokenIssuer(func(matchID uint64) (string, error) { return "tok-for-42", nil }, false)
	if _, _, err := a.Allocate(context.Background(), 42, 1, "moba"); err != nil {
		t.Fatalf("Allocate with issuer: %v", err)
	}
	var req struct {
		Spec struct {
			Metadata struct {
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if got := req.Spec.Metadata.Annotations["pandora.dev/ds-token"]; got != "tok-for-42" {
		t.Errorf("ds-token annotation: got %q want tok-for-42", got)
	}

	// issuer 报错 + off/permissive(required=false):降级为无令牌分配,不阻断。
	a.SetDSTokenIssuer(func(matchID uint64) (string, error) { return "", context.DeadlineExceeded }, false)
	if _, _, err := a.Allocate(context.Background(), 42, 1, "moba"); err != nil {
		t.Fatalf("Allocate with failing issuer must not fail: %v", err)
	}
	if strings.Contains(string(gotBody), "pandora.dev/ds-token") {
		t.Errorf("annotations must be absent when issuer fails: %s", gotBody)
	}

	// issuer 报错 + enforce(required=true):fail-closed,Allocate 返回分配失败。
	a.SetDSTokenIssuer(func(matchID uint64) (string, error) { return "", context.DeadlineExceeded }, true)
	if _, _, err := a.Allocate(context.Background(), 42, 1, "moba"); err == nil {
		t.Fatal("Allocate under enforce with failing issuer must fail")
	} else if got := errcode.As(err); got != errcode.ErrDSAllocationFailed {
		t.Errorf("enforce sign-fail code: got %d want ErrDSAllocationFailed", got)
	}
}

func TestAllocateAuthoritative_POSTWithoutTokenThenStrictGETIdentity(t *testing.T) {
	var issuerCalled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "pandora.dev/ds-token") {
				t.Errorf("Model B GSA POST must not contain token: %s", body)
			}
			if !strings.Contains(string(body), "pandora.dev/allocation-id") {
				t.Errorf("GSA POST missing persistent allocation-id label: %s", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{
				"state": "Allocated", "gameServerName": "battle-fleet-auth1",
				"address": "10.0.0.8", "ports": []map[string]any{{"port": 7777}},
			}})
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{
				"name": "battle-fleet-auth1", "uid": "uid-auth1", "resourceVersion": "101",
				"labels": map[string]string{"pandora.dev/match-id": "42", "pandora.dev/allocation-id": "alloc-42"},
			}})
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer srv.Close()
	a := newTestAllocator(t, srv.URL)
	a.SetDSTokenIssuer(func(uint64) (string, error) {
		issuerCalled.Store(true)
		return "must-not-be-used", nil
	}, true)
	got, err := a.AllocateAuthoritative(context.Background(), 42, "alloc-42", 1, "ranked")
	if err != nil {
		t.Fatalf("AllocateAuthoritative: %v", err)
	}
	if issuerCalled.Load() {
		t.Fatal("Model B signed token before selected GameServer UID was known")
	}
	if got.PodName != "battle-fleet-auth1" || got.Addr != "10.0.0.8:7777" ||
		got.InstanceUID != "uid-auth1" || got.ResourceVersion != "101" {
		t.Fatalf("authoritative allocation mismatch: %+v", got)
	}
}

func TestAllocateAuthoritative_StrictGETRejectsMissingIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{
				"state": "Allocated", "gameServerName": "battle-fleet-bad",
				"address": "10.0.0.8", "ports": []map[string]any{{"port": 7777}},
			}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{
			"name": "battle-fleet-bad", "uid": "", "resourceVersion": "",
		}})
	}))
	defer srv.Close()
	a := newTestAllocator(t, srv.URL)
	if _, err := a.AllocateAuthoritative(context.Background(), 42, "alloc-42", 1, "ranked"); err == nil {
		t.Fatal("missing UID/RV must fail closed")
	}
}

func TestAllocateAuthoritative_POSTUnknownReturnsAllocationFence(t *testing.T) {
	a := newTestAllocator(t, "http://127.0.0.1:1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := a.AllocateAuthoritative(ctx, 42, "alloc-unknown", 1, "ranked")
	if err == nil || got == nil || got.AllocationID != "alloc-unknown" || got.InstanceUID != "" {
		t.Fatalf("partial=%+v err=%v", got, err)
	}
}

func TestDeliverCredential_StrictGETConfirmationMatrix(t *testing.T) {
	tests := []struct {
		name       string
		patchCode  int
		patchDelay time.Duration
		applied    bool
		wantOK     bool
	}{
		{name: "2xx_bad_body_but_applied", patchCode: http.StatusOK, applied: true, wantOK: true},
		{name: "409_already_applied", patchCode: http.StatusConflict, applied: true, wantOK: true},
		{name: "409_wrong_object", patchCode: http.StatusConflict, applied: false, wantOK: false},
		{name: "2xx_without_expected_annotations", patchCode: http.StatusOK, applied: false, wantOK: false},
		{name: "transport_timeout_but_applied", patchCode: http.StatusOK, patchDelay: 80 * time.Millisecond, applied: true, wantOK: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var patchSeen atomic.Bool
			annotations := map[string]string{
				"pandora.dev/ds-token": "jwt-value", "pandora.dev/ds-token-jti": "jti-1",
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodPatch:
					patchSeen.Store(true)
					if got := r.Header.Get("Content-Type"); got != "application/json-patch+json" {
						t.Errorf("patch content-type=%q", got)
					}
					body, _ := io.ReadAll(r.Body)
					if !strings.Contains(string(body), `"path":"/metadata/uid"`) ||
						!strings.Contains(string(body), `"path":"/metadata/resourceVersion"`) {
						t.Errorf("patch missing UID/RV tests: %s", body)
					}
					if tc.patchDelay > 0 {
						time.Sleep(tc.patchDelay)
					}
					w.WriteHeader(tc.patchCode)
					_, _ = w.Write([]byte("not-a-k8s-object"))
				case http.MethodGet:
					gotAnnotations := map[string]string{"pandora.dev/ds-token": "wrong"}
					if tc.applied {
						gotAnnotations = annotations
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{
						"name": "battle-fleet-auth1", "uid": "uid-auth1",
						"resourceVersion": "102", "annotations": gotAnnotations,
					}})
				default:
					t.Errorf("unexpected method %s", r.Method)
				}
			}))
			defer srv.Close()
			a := newTestAllocator(t, srv.URL)
			if tc.patchDelay > 0 {
				a.allocateTimeout = 20 * time.Millisecond
			}
			allocation := &AuthoritativeGameServerAllocation{
				PodName: "battle-fleet-auth1", InstanceUID: "uid-auth1",
				ResourceVersion: "101", AnnotationsPresent: true,
			}
			rv, err := a.DeliverCredential(context.Background(), allocation, annotations)
			if tc.wantOK && (err != nil || rv != "102") {
				t.Fatalf("DeliverCredential: rv=%q err=%v", rv, err)
			}
			if !tc.wantOK && err == nil {
				t.Fatalf("DeliverCredential unexpectedly succeeded rv=%q", rv)
			}
			if !patchSeen.Load() {
				t.Fatal("PATCH not sent")
			}
		})
	}
}

func TestDeliverCredential_ConfirmationSurvivesCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	annotations := map[string]string{
		"pandora.dev/ds-token":     "jwt-value",
		"pandora.dev/ds-token-jti": "jti-1",
	}
	var patchSeen atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPatch:
			patchSeen.Store(true)
			// 模拟入站 RPC 在 PATCH 已被 apiserver 应用后恰好取消。确认 GET 必须使用
			// 独立有界上下文，否则会把真实已应用误报为未知并留下永久 pending。
			cancel()
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{
				"name": "battle-fleet-auth1", "uid": "uid-auth1",
				"resourceVersion": "102", "annotations": annotations,
			}})
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer srv.Close()
	a := newTestAllocator(t, srv.URL)
	rv, err := a.DeliverCredential(ctx, &AuthoritativeGameServerAllocation{
		PodName: "battle-fleet-auth1", InstanceUID: "uid-auth1",
		ResourceVersion: "101", AnnotationsPresent: true,
	}, annotations)
	if err != nil || rv != "102" {
		t.Fatalf("DeliverCredential: rv=%q err=%v", rv, err)
	}
	if !patchSeen.Load() {
		t.Fatal("PATCH not sent")
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

func TestAgonesReleaseExpected_UsesUIDPrecondition(t *testing.T) {
	var gotUID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Preconditions struct {
				UID string `json:"uid"`
			} `json:"preconditions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotUID = body.Preconditions.UID
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	a := newTestAllocator(t, srv.URL)
	if err := a.ReleaseExpected(context.Background(), &AuthoritativeGameServerAllocation{
		PodName: "battle-fleet-abc12", InstanceUID: "uid-old",
	}); err != nil {
		t.Fatalf("ReleaseExpected: %v", err)
	}
	if gotUID != "uid-old" {
		t.Fatalf("delete uid precondition=%q, want uid-old", gotUID)
	}
}

func TestAgonesReleaseExpected_UIDConflictDoesNotSucceed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "UID precondition failed", http.StatusConflict)
	}))
	defer srv.Close()
	a := newTestAllocator(t, srv.URL)
	if err := a.ReleaseExpected(context.Background(), &AuthoritativeGameServerAllocation{
		PodName: "battle-fleet-rebuilt", InstanceUID: "uid-old",
	}); err == nil {
		t.Fatal("same-name rebuilt GameServer UID conflict must not be treated as released")
	}
}

func TestAgonesReleaseExpected_UnknownUIDUsesAllocationLabel(t *testing.T) {
	var gotPath, gotSelector string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotSelector = r.URL.Query().Get("labelSelector")
		if r.Method == http.MethodDelete {
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "preconditions") {
				t.Errorf("unknown UID collection delete must not invent UID precondition: %s", body)
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
	}))
	defer srv.Close()
	a := newTestAllocator(t, srv.URL)
	if err := a.ReleaseExpected(context.Background(), &AuthoritativeGameServerAllocation{
		PodName: "do-not-delete-by-name", AllocationID: "alloc-42",
	}); err != nil {
		t.Fatalf("ReleaseExpected by allocation label: %v", err)
	}
	if gotPath != "/apis/agones.dev/v1/namespaces/pandora/gameservers" ||
		gotSelector != "pandora.dev/allocation-id=alloc-42" {
		t.Fatalf("collection delete path=%q selector=%q", gotPath, gotSelector)
	}
}

func TestAgonesReleaseExpected_UnknownUIDRequiresEmptyListConfirmation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK) // 2xx 不能冒充已删除
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{
			map[string]any{"metadata": map[string]any{"name": "still-there"}},
		}})
	}))
	defer srv.Close()
	a := newTestAllocator(t, srv.URL)
	if err := a.ReleaseExpected(context.Background(), &AuthoritativeGameServerAllocation{
		AllocationID: "alloc-still-there",
	}); err == nil {
		t.Fatal("2xx DeleteCollection with remaining object must fail closed")
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
