// agones_credential_delivery_test.go 覆盖 Model B 的 K8s 严格投递边界。
// 测试使用 httptest 假 apiserver + miniredis 真 WATCH/CAS,不连接任何集群。
package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/auth"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/data"
)

type modelBTestAuthority struct {
	t       *testing.T
	mr      *miniredis.Miniredis
	repo    *data.RedisHubAuthRepo
	rdb     *redis.Client
	signer  *auth.Signer
	verify  *auth.Verifier
	now     time.Time
	mu      sync.Mutex
	nextGen uint64
}

func newModelBTestAuthority(t *testing.T) *modelBTestAuthority {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis run: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	cfg := auth.Config{
		Issuer:   auth.DSCallbackIssuer,
		Audience: auth.DSCallbackAudience,
		Secret:   []byte("model-b-delivery-test-secret-32-bytes-minimum"),
		NowFn:    func() time.Time { return now },
	}
	signer, err := auth.NewSigner(cfg)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	verifier, err := auth.NewVerifier(cfg)
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	return &modelBTestAuthority{
		t: t, mr: mr, repo: data.NewRedisHubAuthRepo(rdb), rdb: rdb, signer: signer, verify: verifier, now: now,
	}
}

func (a *modelBTestAuthority) issue(pod, uid string, epoch uint32) (string, *hubv1.HubDSCredential, error) {
	a.mu.Lock()
	a.nextGen++
	gen := a.nextGen
	a.mu.Unlock()
	return a.issueWithGen(pod, uid, epoch, gen, fmt.Sprintf("jti-%d", gen))
}

func (a *modelBTestAuthority) issueWithGen(pod, uid string, epoch uint32, gen uint64, jti string) (string, *hubv1.HubDSCredential, error) {
	res, err := a.signer.SignHubCredential(pod, uid, epoch, gen, jti, time.Hour)
	if err != nil {
		return "", nil, err
	}
	return res.Token, &hubv1.HubDSCredential{
		Gen: gen, Jti: jti, ExpMs: uint64(res.ExpMs), Kid: res.Kid,
		InstanceUid: uid, ProtocolEpoch: epoch, TokenSha256: res.TokenSHA256, WriterEpoch: res.WriterEpoch,
	}, nil
}

func (a *modelBTestAuthority) verifyToken(token string) (*HubCredentialClaims, error) {
	claims, err := a.verify.VerifyDSCallback(token)
	if err != nil {
		return nil, err
	}
	if claims.DSType != string(auth.DSTypeHub) || claims.MatchID != 0 || claims.ExpiresAt == nil {
		return nil, fmt.Errorf("unexpected hub credential scope")
	}
	return &HubCredentialClaims{
		Pod: claims.Pod(), InstanceUID: claims.UID(), ProtocolEpoch: claims.Epoch(),
		Gen: claims.Gen(), JTI: claims.JTI(), ExpMs: uint64(claims.ExpiresAt.Time.UnixMilli()),
		Kid: claims.Kid(), WriterEpoch: claims.WriterEpoch(),
	}, nil
}

func (a *modelBTestAuthority) configure(p *AgonesHubFleetProvider) {
	p.SetHubAuthority(a.repo, a.issue, a.verifyToken, 10*time.Minute, 2*time.Hour)
}

type fakeGSMode string

const (
	fakeGSNormal             fakeGSMode = "normal"
	fakeGSConflictApplied    fakeGSMode = "conflict_applied"
	fakeGSConflictNotApplied fakeGSMode = "conflict_not_applied"
	fakeGSBad2xxApplied      fakeGSMode = "bad_2xx_applied"
	fakeGSEmpty2xxApplied    fakeGSMode = "empty_2xx_applied"
	fakeGSMissing2xxApplied  fakeGSMode = "missing_2xx_applied"
	fakeGSBad2xxNotApplied   fakeGSMode = "bad_2xx_not_applied"
	fakeGSTimeoutApplied     fakeGSMode = "timeout_applied"
	fakeGSUIDRebuilt         fakeGSMode = "uid_rebuilt"
	fakeGSBadGet             fakeGSMode = "bad_get"
	fakeGSMissingGet         fakeGSMode = "missing_get"
)

type fakeGameServerAPI struct {
	t          *testing.T
	mu         sync.Mutex
	gs         gameServer
	mode       fakeGSMode
	patchCount int
	getCount   int
	lastPatch  []jsonPatchOperation
	afterApply func()
}

func newFakeGameServerAPI(t *testing.T, mode fakeGSMode) *fakeGameServerAPI {
	return &fakeGameServerAPI{
		t: t, mode: mode,
		gs: gameServer{
			Metadata: gsMetadata{
				Name: "hub-model-b", UID: "uid-A", ResourceVersion: "1",
				Labels: map[string]string{fleetLabelKey: "pandora-hub"},
			},
			Status: gsStatus{State: "Ready", Address: "10.0.0.9", Ports: []gsPort{{Name: "default", Port: 7777}}},
		},
	}
}

func (f *fakeGameServerAPI) handler(w http.ResponseWriter, r *http.Request) {
	isSingle := strings.HasSuffix(r.URL.Path, "/gameservers/"+f.gs.Metadata.Name)
	if !isSingle {
		f.mu.Lock()
		gs := cloneGameServer(f.gs)
		f.mu.Unlock()
		_ = writeJSON(w, map[string]any{"items": []gameServer{gs}})
		return
	}

	switch r.Method {
	case http.MethodGet:
		f.mu.Lock()
		f.getCount++
		mode := f.mode
		gs := cloneGameServer(f.gs)
		f.mu.Unlock()
		if mode == fakeGSBadGet {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{"))
			return
		}
		if mode == fakeGSMissingGet {
			_ = writeJSON(w, map[string]any{"metadata": map[string]any{"name": gs.Metadata.Name}})
			return
		}
		_ = writeJSON(w, gs)
	case http.MethodPatch:
		f.handlePatch(w, r)
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

func (f *fakeGameServerAPI) handlePatch(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Content-Type"); got != "application/json-patch+json" {
		f.t.Errorf("content-type got %q want application/json-patch+json", got)
	}
	var ops []jsonPatchOperation
	if err := readJSON(r, &ops); err != nil {
		f.t.Errorf("decode patch: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	f.patchCount++
	f.lastPatch = ops
	mode := f.mode
	if mode == fakeGSUIDRebuilt {
		f.gs.Metadata.UID = "uid-B"
		f.gs.Metadata.ResourceVersion = "2"
		f.mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		return
	}
	if !f.testsMatchLocked(ops) {
		f.mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		return
	}
	apply := mode != fakeGSConflictNotApplied && mode != fakeGSBad2xxNotApplied && mode != fakeGSBadGet
	if apply {
		f.applyPatchLocked(ops)
		rv, _ := strconv.Atoi(f.gs.Metadata.ResourceVersion)
		f.gs.Metadata.ResourceVersion = strconv.Itoa(rv + 1)
	}
	gs := cloneGameServer(f.gs)
	after := f.afterApply
	f.mu.Unlock()

	if apply && after != nil {
		after()
	}
	switch mode {
	case fakeGSConflictApplied, fakeGSConflictNotApplied:
		w.WriteHeader(http.StatusConflict)
	case fakeGSBad2xxApplied, fakeGSBad2xxNotApplied:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{"))
	case fakeGSEmpty2xxApplied:
		w.WriteHeader(http.StatusOK)
	case fakeGSMissing2xxApplied:
		_ = writeJSON(w, map[string]any{"metadata": map[string]any{"name": gs.Metadata.Name}})
	case fakeGSTimeoutApplied:
		<-r.Context().Done()
	default:
		_ = writeJSON(w, gs)
	}
}

func cloneGameServer(in gameServer) gameServer {
	out := in
	out.Metadata.Labels = make(map[string]string, len(in.Metadata.Labels))
	for k, v := range in.Metadata.Labels {
		out.Metadata.Labels[k] = v
	}
	if in.Metadata.Annotations != nil {
		out.Metadata.Annotations = make(map[string]string, len(in.Metadata.Annotations))
		for k, v := range in.Metadata.Annotations {
			out.Metadata.Annotations[k] = v
		}
	}
	out.Status.Ports = append([]gsPort(nil), in.Status.Ports...)
	return out
}

func (f *fakeGameServerAPI) testsMatchLocked(ops []jsonPatchOperation) bool {
	var uidOK, rvOK bool
	for _, op := range ops {
		if op.Op != "test" {
			continue
		}
		value, _ := op.Value.(string)
		switch op.Path {
		case "/metadata/uid":
			uidOK = value == f.gs.Metadata.UID
		case "/metadata/resourceVersion":
			rvOK = value == f.gs.Metadata.ResourceVersion
		}
	}
	return uidOK && rvOK
}

func (f *fakeGameServerAPI) applyPatchLocked(ops []jsonPatchOperation) {
	for _, op := range ops {
		if op.Op != "add" {
			continue
		}
		if op.Path == "/metadata/annotations" {
			f.gs.Metadata.Annotations = map[string]string{}
			if values, ok := op.Value.(map[string]any); ok {
				for k, v := range values {
					f.gs.Metadata.Annotations[k], _ = v.(string)
				}
			}
			continue
		}
		const prefix = "/metadata/annotations/"
		if strings.HasPrefix(op.Path, prefix) {
			if f.gs.Metadata.Annotations == nil {
				f.gs.Metadata.Annotations = map[string]string{}
			}
			key := unescapeJSONPointer(strings.TrimPrefix(op.Path, prefix))
			f.gs.Metadata.Annotations[key], _ = op.Value.(string)
		}
	}
}

func unescapeJSONPointer(s string) string {
	s = strings.ReplaceAll(s, "~1", "/")
	return strings.ReplaceAll(s, "~0", "~")
}

func writeJSON(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }

func runModelBDelivery(t *testing.T, mode fakeGSMode, configure func(*fakeGameServerAPI, *modelBTestAuthority)) (*fakeGameServerAPI, *modelBTestAuthority, []ShardCandidate) {
	t.Helper()
	authority := newModelBTestAuthority(t)
	fake := newFakeGameServerAPI(t, mode)
	if configure != nil {
		configure(fake, authority)
	}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	p := newTestFleetProvider(t, srv.URL)
	authority.configure(p)
	if mode == fakeGSTimeoutApplied {
		p.listTimeout = 40 * time.Millisecond
		p.httpClient.Timeout = 40 * time.Millisecond
	}
	shards, err := p.ListShards(context.Background(), "")
	if err != nil {
		t.Fatalf("ListShards: %v", err)
	}
	return fake, authority, shards
}

func TestModelBDelivery_JSONPatchBindsUIDAndRV(t *testing.T) {
	fake, authority, shards := runModelBDelivery(t, fakeGSNormal, nil)
	if len(shards) != 1 || !shards[0].TokenReady || shards[0].TokenGen != 1 {
		t.Fatalf("strict delivery should make candidate ready: %+v", shards)
	}
	fake.mu.Lock()
	ops := append([]jsonPatchOperation(nil), fake.lastPatch...)
	getCount := fake.getCount
	finalRV := fake.gs.Metadata.ResourceVersion
	fake.mu.Unlock()
	if len(ops) < 3 || ops[0].Op != "test" || ops[0].Path != "/metadata/uid" || ops[0].Value != "uid-A" ||
		ops[1].Op != "test" || ops[1].Path != "/metadata/resourceVersion" || ops[1].Value != "1" {
		t.Fatalf("PATCH must test uid then resourceVersion: %+v", ops)
	}
	if getCount == 0 {
		t.Fatal("successful PATCH must still GET strict final object")
	}
	rec, found, err := authority.repo.GetAuth(context.Background(), "hub-model-b")
	if err != nil || !found || rec.Pending == nil || rec.DeliveredRv != finalRV {
		t.Fatalf("delivered rv must bind current pending: found=%v rec=%+v err=%v", found, rec, err)
	}
}

func TestModelBDelivery_UncertainOutcomesRequireStrictGET(t *testing.T) {
	tests := []struct {
		name string
		mode fakeGSMode
	}{
		{name: "patch_timeout_but_applied", mode: fakeGSTimeoutApplied},
		{name: "409_but_exact_bundle_present", mode: fakeGSConflictApplied},
		{name: "bad_2xx_body_but_applied", mode: fakeGSBad2xxApplied},
		{name: "empty_2xx_body_but_applied", mode: fakeGSEmpty2xxApplied},
		{name: "missing_fields_2xx_body_but_applied", mode: fakeGSMissing2xxApplied},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake, authority, shards := runModelBDelivery(t, tc.mode, nil)
			if len(shards) != 1 || !shards[0].TokenReady {
				t.Fatalf("GET-confirmed exact bundle should succeed: %+v", shards)
			}
			fake.mu.Lock()
			gets := fake.getCount
			finalRV := fake.gs.Metadata.ResourceVersion
			fake.mu.Unlock()
			if gets == 0 {
				t.Fatal("uncertain PATCH outcome was not GET-confirmed")
			}
			rec, found, err := authority.repo.GetAuth(context.Background(), "hub-model-b")
			if err != nil || !found || rec.DeliveredRv != finalRV {
				t.Fatalf("confirmed rv not stored: found=%v rec=%+v err=%v", found, rec, err)
			}
		})
	}
}

func TestModelBDelivery_UnconfirmedOutcomesFailClosed(t *testing.T) {
	tests := []struct {
		name string
		mode fakeGSMode
	}{
		{name: "409_not_applied", mode: fakeGSConflictNotApplied},
		{name: "bad_2xx_not_applied", mode: fakeGSBad2xxNotApplied},
		{name: "get_bad_json", mode: fakeGSBadGet},
		{name: "get_missing_identity_fields", mode: fakeGSMissingGet},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, authority, shards := runModelBDelivery(t, tc.mode, nil)
			if len(shards) != 1 || shards[0].TokenReady {
				t.Fatalf("unconfirmed delivery must fail closed: %+v", shards)
			}
			rec, found, err := authority.repo.GetAuth(context.Background(), "hub-model-b")
			if err != nil || !found || rec.Pending == nil || rec.DeliveredRv != "" {
				t.Fatalf("unconfirmed delivery must leave pending undelivered: found=%v rec=%+v err=%v", found, rec, err)
			}
		})
	}
}

func TestModelBDelivery_SameNameUIDRebuildFailsClosed(t *testing.T) {
	_, authority, shards := runModelBDelivery(t, fakeGSUIDRebuilt, nil)
	if len(shards) != 1 || shards[0].TokenReady {
		t.Fatalf("same-name uid rebuild must reject old delivery: %+v", shards)
	}
	rec, found, err := authority.repo.GetAuth(context.Background(), "hub-model-b")
	if err != nil || !found || rec.InstanceUid != "uid-A" || rec.DeliveredRv != "" {
		t.Fatalf("old uid response must not be marked delivered: found=%v rec=%+v err=%v", found, rec, err)
	}
}

func TestModelBDelivery_RedisPendingRaceFailsClosed(t *testing.T) {
	var raceErr error
	_, authority, shards := runModelBDelivery(t, fakeGSNormal, func(fake *fakeGameServerAPI, authority *modelBTestAuthority) {
		fake.afterApply = func() {
			_, newer, err := authority.issueWithGen("hub-model-b", "uid-A", 1, 2, "jti-newer")
			if err != nil {
				raceErr = err
				return
			}
			_, raceErr = authority.repo.StagePending(context.Background(), "hub-model-b", newer, 2*time.Hour)
		}
	})
	if raceErr != nil {
		t.Fatalf("inject newer pending: %v", raceErr)
	}
	if len(shards) != 1 || shards[0].TokenReady {
		t.Fatalf("pending changed after PATCH must fail closed: %+v", shards)
	}
	rec, found, err := authority.repo.GetAuth(context.Background(), "hub-model-b")
	if err != nil || !found || rec.Pending == nil || rec.Pending.Gen != 2 || rec.DeliveredRv != "" {
		t.Fatalf("old response contaminated newer pending: found=%v rec=%+v err=%v", found, rec, err)
	}
}

func TestModelBDelivery_RedisFailureAfterPatchFailsClosed(t *testing.T) {
	_, _, shards := runModelBDelivery(t, fakeGSNormal, func(fake *fakeGameServerAPI, authority *modelBTestAuthority) {
		fake.afterApply = func() { authority.mr.Close() }
	})
	if len(shards) != 1 || shards[0].TokenReady {
		t.Fatalf("PATCH applied but Redis confirmation unavailable must fail closed: %+v", shards)
	}
}

func TestModelBDelivery_ConcurrentAllocatorsNeverMarkWrongPending(t *testing.T) {
	authority := newModelBTestAuthority(t)
	fake := newFakeGameServerAPI(t, fakeGSNormal)
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	p := newTestFleetProvider(t, srv.URL)
	authority.configure(p)

	start := make(chan struct{})
	results := make(chan []ShardCandidate, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			<-start
			shards, err := p.ListShards(context.Background(), "")
			if err != nil {
				t.Errorf("concurrent ListShards: %v", err)
				return
			}
			results <- shards
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	for shards := range results {
		if len(shards) != 1 {
			t.Fatalf("concurrent shard count: %+v", shards)
		}
	}
	rec, found, err := authority.repo.GetAuth(context.Background(), "hub-model-b")
	if err != nil || !found || rec.Pending == nil {
		t.Fatalf("current pending missing after concurrent publish: found=%v rec=%+v err=%v", found, rec, err)
	}
	if rec.DeliveredRv != "" {
		fake.mu.Lock()
		finalGS := cloneGameServer(fake.gs)
		fake.mu.Unlock()
		if err := p.credentialBundleMatches(&finalGS, rec.Pending, false); err != nil {
			t.Fatalf("delivered_rv points at non-current pending bundle: %v rec=%+v gs=%+v", err, rec, finalGS.Metadata)
		}
	}
}

func TestModelBDelivery_JWTClaimsMustMatchPendingAndAnnotation(t *testing.T) {
	authority := newModelBTestAuthority(t)
	p := &AgonesHubFleetProvider{hubCredVerifier: authority.verifyToken, dsTokenRenewBefore: 10 * time.Minute}
	wrongToken, wrongTokenCred, err := authority.issueWithGen("hub-model-b", "uid-WRONG", 1, 7, "jti-wrong")
	if err != nil {
		t.Fatalf("issue wrong token: %v", err)
	}
	// 人为构造“annotation gen/exp 与 pending 一样、hash 也指向该 token,但 JWT uid/jti 与
	// pending 不同”的损坏记录,证明不能只信 annotation 数字或 hash。
	expected := proto.Clone(wrongTokenCred).(*hubv1.HubDSCredential)
	expected.InstanceUid = "uid-A"
	expected.Jti = "jti-expected"
	gs := &gameServer{Metadata: gsMetadata{
		Name: "hub-model-b", UID: "uid-A", ResourceVersion: "9",
		Annotations: hubCredentialAnnotations(wrongToken, expected),
	}}
	if err := p.credentialBundleMatches(gs, expected, false); err == nil || !strings.Contains(err.Error(), "jwt tuple mismatch") {
		t.Fatalf("wrong uid/jti JWT must be rejected after signature verification, got %v", err)
	}

	// 外置 annotation 数字与 JWT/pending 任一不一致也必须拒绝。
	gs.Metadata.UID = wrongTokenCred.InstanceUid
	gs.Metadata.Annotations = hubCredentialAnnotations(wrongToken, wrongTokenCred)
	gs.Metadata.Annotations[dsTokenGenAnnotationKey] = "8"
	if err := p.credentialBundleMatches(gs, wrongTokenCred, false); err == nil {
		t.Fatal("annotation gen mismatch must be rejected")
	}
	gs.Metadata.Annotations[dsTokenGenAnnotationKey] = strconv.FormatUint(wrongTokenCred.Gen, 10)
	gs.Metadata.Annotations[dsTokenExpAnnotationKey] = strconv.FormatUint(wrongTokenCred.ExpMs+1000, 10)
	if err := p.credentialBundleMatches(gs, wrongTokenCred, false); err == nil {
		t.Fatal("annotation exp mismatch must be rejected")
	}
	for _, key := range []string{
		dsTokenJTIAnnotationKey, dsInstanceUIDAnnotationKey, dsInstanceEpochAnnotationKey,
		dsWriterEpochAnnotationKey, dsTokenKidAnnotationKey, dsTokenHashAnnotationKey,
	} {
		gs.Metadata.Annotations = hubCredentialAnnotations(wrongToken, wrongTokenCred)
		gs.Metadata.Annotations[key] = "wrong"
		if err := p.credentialBundleMatches(gs, wrongTokenCred, false); err == nil {
			t.Fatalf("annotation %s mismatch must be rejected", key)
		}
	}
}
