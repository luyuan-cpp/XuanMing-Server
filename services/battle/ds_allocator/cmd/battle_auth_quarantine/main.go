// battle_auth_quarantine 是仅供受控运维环境调用的 Battle 凭据紧急吊销工具。
// 默认只审计；显式 -apply 才执行 allocation_id + 完整 active tuple CAS。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/data"
)

func main() {
	addrs := flag.String("addrs", "", "Redis addresses, comma separated")
	passwordEnv := flag.String("password-env", "PANDORA_REDIS_PASSWORD", "Redis password environment variable")
	matchID := flag.Uint64("match-id", 0, "expected match id")
	allocationID := flag.String("allocation-id", "", "expected allocation id")
	pod := flag.String("pod", "", "expected Battle GameServer name")
	uid := flag.String("uid", "", "expected GameServer UID")
	epoch := flag.Uint("epoch", 0, "expected instance epoch")
	gen := flag.Uint64("gen", 0, "expected active credential generation")
	jti := flag.String("jti", "", "expected active credential jti")
	expMs := flag.Uint64("exp-ms", 0, "expected active credential expiration")
	kid := flag.String("kid", "", "expected active credential kid")
	tokenHash := flag.String("token-sha256", "", "expected active token sha256")
	writer := flag.Uint("writer-epoch", 0, "expected writer epoch")
	authTTL := flag.Duration("auth-ttl", 4*time.Hour, "quarantined auth retention")
	battleTTL := flag.Duration("battle-ttl", 2*time.Hour, "abandoned battle retention")
	apply := flag.Bool("apply", false, "perform quarantine CAS")
	flag.Parse()
	addresses := split(*addrs)
	if len(addresses) == 0 || *matchID == 0 || *allocationID == "" || *pod == "" || *uid == "" ||
		*epoch == 0 || *gen == 0 || *jti == "" || *expMs == 0 || *kid == "" ||
		*tokenHash == "" || *writer == 0 {
		fatal(fmt.Errorf("addrs, allocation id and complete expected credential tuple are required"))
	}
	client := redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs: addresses, Password: os.Getenv(*passwordEnv),
	})
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		fatal(err)
	}
	repo := data.NewRedisBattleAuthRepo(client)
	expected := data.BattleQuarantineExpected{
		AllocationID: *allocationID,
		Credential: data.BattleCredentialIdentity{
			PodName: *pod, InstanceUID: *uid, InstanceEpoch: uint32(*epoch),
			Gen: *gen, JTI: *jti, ExpMs: *expMs, Kid: *kid,
			TokenSHA256: *tokenHash, WriterEpoch: uint32(*writer),
		},
	}
	if !*apply {
		snapshot, err := repo.ReadAuthority(ctx, *matchID)
		if err != nil {
			fatal(err)
		}
		active := snapshot.Auth.GetActive()
		credential := expected.Credential
		if !snapshot.AuthFound || !snapshot.BattleFound || snapshot.Auth.GetAllocationId() != *allocationID ||
			snapshot.Auth.GetDsPodName() != credential.PodName || snapshot.Auth.GetInstanceUid() != credential.InstanceUID ||
			snapshot.Auth.GetInstanceEpoch() != credential.InstanceEpoch || active.GetGen() != credential.Gen ||
			active.GetJti() != credential.JTI || active.GetExpMs() != credential.ExpMs || active.GetKid() != credential.Kid ||
			active.GetTokenSha256() != credential.TokenSHA256 || active.GetWriterEpoch() != credential.WriterEpoch {
			fatal(fmt.Errorf("expected allocation/tuple does not equal current active authority"))
		}
		fmt.Println("[AUDIT ONLY] 当前 allocation 与 active 完整 tuple 精确匹配；未修改 Redis，显式加 -apply。")
		return
	}
	result, err := repo.QuarantineExpected(ctx, *matchID, expected, *authTTL, *battleTTL)
	if err != nil {
		fatal(err)
	}
	if !result.AuthQuarantined {
		fatal(fmt.Errorf("expected tuple no longer matches active authority; zero mutation"))
	}
	if !result.ProjectionAbandoned {
		fmt.Printf("Battle match=%d allocation=%s 授权已 QUARANTINED；battle 投影缺失/漂移，未改动投影，需独立恢复审计\n", *matchID, *allocationID)
		return
	}
	fmt.Printf("Battle match=%d allocation=%s 已进入 QUARANTINED/abandoned 补偿流\n", *matchID, *allocationID)
}

func split(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func fatal(err error) { fmt.Fprintln(os.Stderr, "ERROR:", err); os.Exit(1) }
