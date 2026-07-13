package auth

import (
	"fmt"
	"time"
)

const (
	// DSLocalProfileEnv 是 allocator→UE 的本机运行契约标记。它不是授权凭据；UE 还会
	// 机械校验 Windows、非 Agones、本地 pod 前缀以及完整 Model-B JWT scope。
	DSLocalProfileEnv = "PANDORA_DS_LOCAL_PROFILE"
	// DSLocalProfileOffV1 只用于 mode=local + ds_auth.mode=off + authority=legacy。
	DSLocalProfileOffV1 = "local-off-v1"
	// DSLocalHubMinTokenTTL 是一次性 env 凭据必须覆盖的最短本地 Hub 调试会话。
	// local-off-v1 没有 annotation 轮换，UE 到 exp 会主动清空 active，故不能按 Guard=off 跳过。
	DSLocalHubMinTokenTTL = 12 * time.Hour
)

// ValidateDSLocalProfileOffV1 阻止本机 allocator 把生产/灰度姿态误标成离线 profile。
// guardMode 应传已经过 middleware 解析归一化的值，故只接受精确的 "off"。
func ValidateDSLocalProfileOffV1(guardMode, authorityMode string, signerReady bool) error {
	if guardMode != "off" || authorityMode != "legacy" || !signerReady {
		return fmt.Errorf("%s requires guard=off authority=legacy signer_ready=true (got guard=%q authority=%q signer_ready=%t)",
			DSLocalProfileOffV1, guardMode, authorityMode, signerReady)
	}
	return nil
}

// ValidateDSLocalHubProfileOffV1 在通用 profile 门上再校验一次性 Hub 凭据寿命。
func ValidateDSLocalHubProfileOffV1(guardMode, authorityMode string, signerReady bool, tokenTTL time.Duration) error {
	if err := ValidateDSLocalProfileOffV1(guardMode, authorityMode, signerReady); err != nil {
		return err
	}
	if tokenTTL < DSLocalHubMinTokenTTL {
		return fmt.Errorf("%s hub token ttl=%s is below local session minimum %s",
			DSLocalProfileOffV1, tokenTTL, DSLocalHubMinTokenTTL)
	}
	return nil
}
