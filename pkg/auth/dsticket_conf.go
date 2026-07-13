package auth

// DSTicket v2 从配置文件构造签发器/校验器(方案 B)。
//
// 集中放这里的原因:login / hub_allocator / matchmaker 三个签发点必须用完全一致的
// 加载与校验逻辑(读私钥文件、kid 自检、TTL 上限、JWKS 严格解析、revision 比对),
// 避免每个服务各写一份出现行为漂移。

import (
	"fmt"
	"os"
	"strconv"

	"github.com/luyuancpp/pandora/pkg/config"
)

// NewDSTicketSignerFromConf 按共享配置构造 v2 签发器。
// 调用方必须先用 conf.SignerEnabled() 判断是否启用;未启用时调用本函数返回错误。
func NewDSTicketSignerFromConf(c config.DSTicketConf) (*DSTicketSigner, error) {
	if !c.SignerEnabled() {
		return nil, fmt.Errorf("ds_ticket: private_key_file 未配置,v2 签发未启用")
	}
	pem, err := os.ReadFile(c.PrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("ds_ticket: 读私钥文件失败: %w", err)
	}
	return NewDSTicketSigner(DSTicketSignerConfig{
		PrivateKeyPEM: pem,
		ActiveKid:     c.ActiveKid,
		TTL:           c.TTL.Std(),
	})
}

// NewDSTicketVerifierFromConf 按共享配置构造 v2 校验器(服务侧;DS 侧走 env + UE 实现)。
// JWKS 按严格规则解析(拒私钥成员/oct/未知字段);配置了 KeysetRevision 时与文件内
// revision 不一致直接失败,防止「换了键没换文件」这类半完成发布。
func NewDSTicketVerifierFromConf(c config.DSTicketConf) (*DSTicketVerifier, error) {
	if !c.VerifierEnabled() {
		return nil, fmt.Errorf("ds_ticket: jwks_file 未配置,v2 校验未启用")
	}
	if c.KeysetRevision == "" || c.ActiveKid == "" {
		return nil, fmt.Errorf("ds_ticket: verifier requires explicit keyset_revision and active_kid")
	}
	data, err := os.ReadFile(c.JWKSFile)
	if err != nil {
		return nil, fmt.Errorf("ds_ticket: 读 JWKS 文件失败: %w", err)
	}
	revision, activeKid, err := DSTicketJWKSMetadata(data)
	if err != nil {
		return nil, fmt.Errorf("ds_ticket: JWKS 不合规: %w", err)
	}
	want, err := strconv.Atoi(c.KeysetRevision)
	if err != nil || want < 1 {
		return nil, fmt.Errorf("ds_ticket: keyset_revision 必须是正整数")
	}
	if revision != want {
		return nil, fmt.Errorf("ds_ticket: keyset revision 不匹配: 期望 %d, JWKS 内为 %d",
			want, revision)
	}
	if activeKid != c.ActiveKid {
		return nil, fmt.Errorf("ds_ticket: active_kid 不匹配: 配置为 %q, JWKS 内为 %q", c.ActiveKid, activeKid)
	}
	// NewDSTicketVerifier 内部会做严格 JWKS 解析(拒私钥成员/oct/未知字段),此处无需重复。
	return NewDSTicketVerifier(DSTicketVerifierConfig{JWKS: data})
}
