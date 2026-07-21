// Package conf 是 player 服务的私有配置结构(W4 ④,2026-06-06)。
package conf

import (
	"fmt"
	"time"

	"github.com/luyuancpp/pandora/pkg/config"
	"github.com/luyuancpp/pandora/pkg/kafkax"
)

// Config 是 player 服务的完整配置。
type Config struct {
	config.Base `yaml:",inline" mapstructure:",squash"`

	Player PlayerConf `yaml:"player" json:"player"`
}

// PlayerConf 是 player 服务私有配置。
type PlayerConf struct {
	// BaseMMR 新玩家缺省 MMR(EnsureProfile / GetMMR 未建档兜底,默认 1500,与 battle_result 对齐)。
	BaseMMR int `yaml:"base_mmr,omitempty" json:"base_mmr,omitempty"`

	// MMRFloor MMR 下限(UpdateMMR 后 clamp,默认 0)。
	MMRFloor int `yaml:"mmr_floor,omitempty" json:"mmr_floor,omitempty"`

	// DefaultNicknamePrefix 默认昵称前缀(EnsureProfile 建档时 nickname=prefix+player_id,保证 uk 唯一,默认 "Player_")。
	DefaultNicknamePrefix string `yaml:"default_nickname_prefix,omitempty" json:"default_nickname_prefix,omitempty"`

	// MaxNicknameLen 昵称最大长度(UpdateNickname 校验,默认 32)。
	MaxNicknameLen int `yaml:"max_nickname_len,omitempty" json:"max_nickname_len,omitempty"`

	// HeroSelectionEnabled 出战英雄选择功能开关(默认 false,demo 阶段跳过选英雄,
	// 与 login demo-skip 风格一致;关闭时 SelectHero 返回 ERR_PLAYER_FEATURE_DISABLED)。
	HeroSelectionEnabled bool `yaml:"hero_selection_enabled,omitempty" json:"hero_selection_enabled,omitempty"`

	// LoadoutCustomizeEnabled 出战装备预设 / 天赋树自定义功能开关(默认 false,demo 阶段跳过;
	// 关闭时 SetEquipment / SetTalents / ResetTalents 返回 ERR_PLAYER_FEATURE_DISABLED;
	// 授予类 GrantTalentPoints 由系统驱动不受此开关影响)。
	//
	// ⚠️ 安全(2026-06-17 审查):SetEquipment 目前**只校验槽位不重复 + item_config_id 非 0**,
	// 未校验玩家是否拥有该装备 / item 是否为装备 / 槽位是否匹配。因 GetLoadout 会把装备转成
	// Battle DS 初始 GameplayEffect,启用后等于客户端可给自己配任意装备。
	// **在接 inventory/配置表做拥有权 + 类型 + 槽位校验前,严禁对客户端开放**:
	// (1) 生产保持 false;(2) 不在 Envoy 暴露 player.v1.PlayerService 路由(当前未暴露)。
	LoadoutCustomizeEnabled bool `yaml:"loadout_customize_enabled,omitempty" json:"loadout_customize_enabled,omitempty"`

	// ConsumeTopics 本服订阅的 kafka topic(默认 [player.update])。
	ConsumeTopics []string `yaml:"consume_topics,omitempty" json:"consume_topics,omitempty"`

	// ── 玩家等级经验(实时成长,docs/design/realtime-progression.md)──

	// ExpCurve 等级经验曲线:第 i 项(0 基)= 从 Lv(i+1) 升到 Lv(i+2) 所需级内经验(须 >0)。
	// 最高等级 = len(ExpCurve)+1(需求 Lv15 → 配 14 项)。
	// **空 = 经验功能关闭**(AddExperience 返回 ERR_PLAYER_FEATURE_DISABLED,GetProfile 不标满级),
	// 默认关闭保持现有行为不变(§14.2)。数值必须与客户端 j_玩家等级经验.xlsx / CfgPlayerLevelExp
	// 同源(客户端用同表补 RequiredExp 显示;漂移只影响显示,权威在本服务)。
	ExpCurve []uint64 `yaml:"exp_curve,omitempty" json:"exp_curve,omitempty"`

	// MaxExpPerGrant 单次 AddExperience 入账上限(默认 1000000)。
	// 防异常 / 越权调用方一次灌满等级(DS 不可信纵深:battle_result 已按怪物表换算,
	// 这里是 player 侧最后一道兜底)。
	MaxExpPerGrant uint64 `yaml:"max_exp_per_grant,omitempty" json:"max_exp_per_grant,omitempty"`

	// PushOutboxInterval 经验推送出箱发布轮询间隔(默认 1s;经验条刷新体感由它决定上界)。
	PushOutboxInterval config.Duration `yaml:"push_outbox_interval,omitempty" json:"push_outbox_interval,omitempty"`

	// PushOutboxBatch 每轮发布取多少条推送出箱记录(默认 128)。
	PushOutboxBatch int `yaml:"push_outbox_batch,omitempty" json:"push_outbox_batch,omitempty"`

	// ExpHistoryRetention 经验幂等收据(exp_history)留存期(默认 7 天,下限 7 天:
	// 必须覆盖 battle_result progress 出箱最长重试窗,收据被提前清掉会破坏幂等,§9.2)。
	ExpHistoryRetention config.Duration `yaml:"exp_history_retention,omitempty" json:"exp_history_retention,omitempty"`
}

// Defaults 填默认值。
func (c *Config) Defaults() {
	if c.Player.BaseMMR <= 0 {
		c.Player.BaseMMR = 1500
	}
	if c.Player.MMRFloor < 0 {
		c.Player.MMRFloor = 0
	}
	if c.Player.DefaultNicknamePrefix == "" {
		c.Player.DefaultNicknamePrefix = "Player_"
	}
	if c.Player.MaxNicknameLen <= 0 {
		c.Player.MaxNicknameLen = 32
	}
	if len(c.Player.ConsumeTopics) == 0 {
		c.Player.ConsumeTopics = []string{kafkax.TopicPlayerUpdate}
	}
	if c.Server.Grpc.Addr == "" {
		c.Server.Grpc.Addr = ":50002"
	}
	if c.Server.Http.Addr == "" {
		c.Server.Http.Addr = ":51002"
	}
}

// ValidateExpCurve 校验等级经验曲线(main 启动时调,失败即退出):
// 空 = 功能关闭合法;非空时每项须 >0(0 会让该级永不可升,属配置错误),
// 且长度有 sanity 上限(防手滑配出千级曲线)。
func (p *PlayerConf) ValidateExpCurve() error {
	const maxCurveLen = 200
	if len(p.ExpCurve) > maxCurveLen {
		return fmt.Errorf("player.exp_curve too long: %d > %d", len(p.ExpCurve), maxCurveLen)
	}
	for i, need := range p.ExpCurve {
		if need == 0 {
			return fmt.Errorf("player.exp_curve[%d] must be positive (Lv%d→Lv%d)", i, i+1, i+2)
		}
	}
	return nil
}

// MaxExpPerGrantOrDefault 返回生效的单次入账上限(未配置 → 1000000)。
func (p *PlayerConf) MaxExpPerGrantOrDefault() uint64 {
	if p.MaxExpPerGrant > 0 {
		return p.MaxExpPerGrant
	}
	return 1_000_000
}

// PushOutboxIntervalOrDefault 返回生效的推送出箱轮询间隔(未配置 → 1s)。
func (p *PlayerConf) PushOutboxIntervalOrDefault() time.Duration {
	if d := p.PushOutboxInterval.Std(); d > 0 {
		return d
	}
	return time.Second
}

// PushOutboxBatchOrDefault 返回生效的推送出箱批大小(未配置 → 128)。
func (p *PlayerConf) PushOutboxBatchOrDefault() int {
	if p.PushOutboxBatch > 0 {
		return p.PushOutboxBatch
	}
	return 128
}

// ExpHistoryRetentionOrDefault 返回生效的 exp_history 留存期(未配置 → 7 天;
// 配置低于 7 天按 7 天,防手滑把幂等窗清穿)。
func (p *PlayerConf) ExpHistoryRetentionOrDefault() time.Duration {
	const min = 7 * 24 * time.Hour
	if d := p.ExpHistoryRetention.Std(); d > min {
		return d
	}
	return min
}
