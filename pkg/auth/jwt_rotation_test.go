// jwt_rotation_test.go — DS 回调令牌不停服密钥轮换(AdditionalSecrets + kid)单测(审核 P1 #3)。
package auth

import (
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
)

const (
	rotOldSecret = "pandora-ds-old-shared-secret-32b!!"
	rotNewSecret = "pandora-ds-new-shared-secret-32b!!"
)

// newRotSigner 用指定主密钥构造 DS 回调 Signer。
func newRotSigner(t *testing.T, secret string, now time.Time) *Signer {
	t.Helper()
	s, err := NewSigner(Config{
		Issuer:   DSCallbackIssuer,
		Audience: DSCallbackAudience,
		Secret:   []byte(secret),
		NowFn:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewSigner(%q): %v", secret, err)
	}
	return s
}

// newRotVerifier 用主密钥 + 额外校验密钥构造 Verifier。
func newRotVerifier(t *testing.T, secret string, additional []string, now time.Time) *Verifier {
	t.Helper()
	extra := make([][]byte, 0, len(additional))
	for _, a := range additional {
		extra = append(extra, []byte(a))
	}
	v, err := NewVerifier(Config{
		Issuer:            DSCallbackIssuer,
		Audience:          DSCallbackAudience,
		Secret:            []byte(secret),
		AdditionalSecrets: extra,
		NowFn:             func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

// 单密钥场景:行为与历史一致(主密钥签、主密钥校验通过)。
func TestRotationSingleKeyUnchanged(t *testing.T) {
	now := time.Unix(1_780_000_000, 0).UTC()
	s := newRotSigner(t, rotOldSecret, now)
	v := newRotVerifier(t, rotOldSecret, nil, now)

	tok, _, err := s.SignDSCallback(DSTypeBattle, "", 9001, time.Hour)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := v.VerifyDSCallback(tok); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// 阶段①:各服务先把新密钥加进 additional。旧主密钥签的 token,校验侧(主=旧 + additional=新)通过。
func TestRotationPhase1OldTokenAcceptedByExtendedVerifier(t *testing.T) {
	now := time.Unix(1_780_000_000, 0).UTC()
	s := newRotSigner(t, rotOldSecret, now)
	v := newRotVerifier(t, rotOldSecret, []string{rotNewSecret}, now)

	tok, _, err := s.SignDSCallback(DSTypeHub, "pandora-hub-abc12", 0, time.Hour)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := v.VerifyDSCallback(tok); err != nil {
		t.Fatalf("verify old token: %v", err)
	}
}

// 阶段②:主密钥翻新为新密钥,旧密钥进 additional。
// 新主密钥签的 token,被「主=新 + additional=旧」的校验侧接受;
// 且此时仍在线的旧副本(主=旧 + additional=新)也能接受新 token —— 双向共存无 401。
func TestRotationPhase2Coexistence(t *testing.T) {
	now := time.Unix(1_780_000_000, 0).UTC()

	newSigner := newRotSigner(t, rotNewSecret, now)
	oldSigner := newRotSigner(t, rotOldSecret, now)

	// 已翻新副本:主=新,additional=旧
	rolledVerifier := newRotVerifier(t, rotNewSecret, []string{rotOldSecret}, now)
	// 尚未翻新副本:主=旧,additional=新
	pendingVerifier := newRotVerifier(t, rotOldSecret, []string{rotNewSecret}, now)

	newTok, _, err := newSigner.SignDSCallback(DSTypeBattle, "", 42, time.Hour)
	if err != nil {
		t.Fatalf("sign new: %v", err)
	}
	oldTok, _, err := oldSigner.SignDSCallback(DSTypeBattle, "", 43, time.Hour)
	if err != nil {
		t.Fatalf("sign old: %v", err)
	}

	for name, v := range map[string]*Verifier{"rolled": rolledVerifier, "pending": pendingVerifier} {
		if _, err := v.VerifyDSCallback(newTok); err != nil {
			t.Fatalf("%s verify new token: %v", name, err)
		}
		if _, err := v.VerifyDSCallback(oldTok); err != nil {
			t.Fatalf("%s verify old token: %v", name, err)
		}
	}
}

// 阶段③:清空 additional,只剩新主密钥。旧密钥签的 token 被拒(轮换完成,旧密钥彻底失效)。
func TestRotationPhase3OldKeyRejectedAfterCleanup(t *testing.T) {
	now := time.Unix(1_780_000_000, 0).UTC()
	oldSigner := newRotSigner(t, rotOldSecret, now)
	v := newRotVerifier(t, rotNewSecret, nil, now)

	oldTok, _, err := oldSigner.SignDSCallback(DSTypeBattle, "", 9001, time.Hour)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := v.VerifyDSCallback(oldTok); err == nil {
		t.Fatal("expected old-key token to be rejected after cleanup")
	}
}

// 完全无关的密钥签的 token 一律被拒。
func TestRotationUnknownKeyRejected(t *testing.T) {
	now := time.Unix(1_780_000_000, 0).UTC()
	evilSigner := newRotSigner(t, "pandora-ds-evil-shared-secret-32!!", now)
	v := newRotVerifier(t, rotNewSecret, []string{rotOldSecret}, now)

	tok, _, err := evilSigner.SignDSCallback(DSTypeBattle, "", 9001, time.Hour)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := v.VerifyDSCallback(tok); err == nil {
		t.Fatal("expected unknown-key token to be rejected")
	}
}

// DS 回调令牌带 kid 头 = 主密钥指纹;轮换期校验侧据此路由到正确密钥。
func TestRotationKidHeaderPresent(t *testing.T) {
	now := time.Unix(1_780_000_000, 0).UTC()
	s := newRotSigner(t, rotNewSecret, now)
	tok, _, err := s.SignDSCallback(DSTypeBattle, "", 9001, time.Hour)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	parsed, _, err := jwt.NewParser().ParseUnverified(tok, jwt.MapClaims{})
	if err != nil {
		t.Fatalf("parse unverified: %v", err)
	}
	kid, ok := parsed.Header["kid"].(string)
	if !ok || kid == "" {
		t.Fatalf("expected non-empty kid header, got %v", parsed.Header["kid"])
	}
	if kid != keyFingerprint([]byte(rotNewSecret)) {
		t.Fatalf("kid mismatch: got %q", kid)
	}
}
