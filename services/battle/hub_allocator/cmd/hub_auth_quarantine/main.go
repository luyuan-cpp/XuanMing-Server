// hub_auth_quarantine 是仅供受控运维环境调用的 Hub 凭据紧急吊销工具。
// 默认只审计；显式 -apply 才执行完整 tuple CAS。Redis 密码只从环境读取。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/data"
)

func main() {
	addrs := flag.String("addrs", "", "Redis addresses, comma separated")
	passwordEnv := flag.String("password-env", "PANDORA_REDIS_PASSWORD", "Redis password environment variable")
	pod := flag.String("pod", "", "expected Hub GameServer name")
	uid := flag.String("uid", "", "expected GameServer UID")
	epoch := flag.Uint("epoch", 0, "expected protocol epoch")
	gen := flag.Uint64("gen", 0, "expected active credential generation")
	jti := flag.String("jti", "", "expected active credential jti")
	kid := flag.String("kid", "", "expected active credential kid")
	tokenHash := flag.String("token-sha256", "", "expected active token sha256")
	writer := flag.Uint("writer-epoch", 0, "expected writer epoch")
	authTTL := flag.Duration("auth-ttl", 48*time.Hour, "quarantined auth retention")
	shardTTL := flag.Duration("shard-ttl", 30*time.Minute, "draining shard retention")
	apply := flag.Bool("apply", false, "perform quarantine CAS")
	flag.Parse()
	addresses := split(*addrs)
	if len(addresses) == 0 || *pod == "" || *uid == "" || *epoch == 0 || *gen == 0 ||
		*jti == "" || *kid == "" || *tokenHash == "" || *writer == 0 {
		fatal(fmt.Errorf("addrs and complete expected credential tuple are required"))
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
	repo := data.NewRedisHubAuthRepo(client)
	expected := data.CredentialIdentity{
		Gen: *gen, JTI: *jti, InstanceUID: *uid, ProtocolEpoch: uint32(*epoch),
		TokenSHA256: *tokenHash, Kid: *kid, WriterEpoch: uint32(*writer),
	}
	if !*apply {
		record, found, err := repo.GetAuth(ctx, *pod)
		if err != nil {
			fatal(err)
		}
		active := record.GetActive()
		if !found || active.GetGen() != expected.Gen || active.GetJti() != expected.JTI ||
			record.GetInstanceUid() != expected.InstanceUID || record.GetProtocolEpoch() != expected.ProtocolEpoch ||
			active.GetKid() != expected.Kid || active.GetTokenSha256() != expected.TokenSHA256 ||
			active.GetWriterEpoch() != expected.WriterEpoch {
			fatal(fmt.Errorf("expected tuple does not equal current active authority"))
		}
		fmt.Println("[AUDIT ONLY] 当前 active 完整 tuple 精确匹配；未修改 Redis，显式加 -apply 才隔离。")
		return
	}
	result, err := repo.QuarantineExpected(ctx, *pod, expected, *authTTL, *shardTTL)
	if err != nil {
		fatal(err)
	}
	if !result.AuthQuarantined {
		fatal(fmt.Errorf("expected tuple no longer matches active authority; zero mutation"))
	}
	if !result.ProjectionDrained {
		fmt.Printf("Hub %s 授权已进入 QUARANTINED；shard 投影缺失/漂移，未改动投影，需独立恢复审计\n", *pod)
		return
	}
	fmt.Printf("Hub %s 已按 UID/epoch/gen/jti/writer fencing 进入 QUARANTINED/draining\n", *pod)
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
