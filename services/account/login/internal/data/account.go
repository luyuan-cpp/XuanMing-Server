// Package data 是 login 服务的"数据层"(repository)。
//
// W2 mock 阶段:不接 MySQL / 不接 Redis,只在内存里返回固定账号。
//
// W3 真实化:
//   - MySQL: pandora_account.account 表(account / password_hash / status / created_at)
//   - Redis: session token 缓存(K=token, V=player_id, TTL=24h)
//   - Redis: device 频控(K=device_id, V=ts, TTL=1h)
package data

import (
	"context"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// AccountRepo 是账号数据访问接口。biz 层依赖本接口,而不是具体实现,
// 方便 W3 替换为 mysql/redis 版本时不动 biz/service。
type AccountRepo interface {
	// FindByAccount 根据账号名查 player_id + password_hash。
	// 找不到返回 ErrLoginAccountNotFound。
	FindByAccount(ctx context.Context, account string) (playerID int64, passwordHash string, err error)

	// CheckBanned 检查账号 / 设备是否封禁。
	// W2 mock 永远返回 false。
	CheckBanned(ctx context.Context, playerID int64, deviceID string) (banned bool, err error)
}

// MockAccountRepo W2 mock 实现:固定账号 + 固定密码哈希。
type MockAccountRepo struct {
	Account      string
	PasswordHash string
	PlayerID     int64
}

// NewMockAccountRepo 构造 mock repo。
//
// account / passwordHash 来自 conf.LoginConf.Mock*,playerID 由 biz 层用 snowflake 填。
func NewMockAccountRepo(account, passwordHash string, playerID int64) *MockAccountRepo {
	return &MockAccountRepo{
		Account:      account,
		PasswordHash: passwordHash,
		PlayerID:     playerID,
	}
}

func (m *MockAccountRepo) FindByAccount(ctx context.Context, account string) (int64, string, error) {
	if account != m.Account {
		return 0, "", errcode.New(errcode.ErrLoginAccountNotFound, "account=%s not found", account)
	}
	return m.PlayerID, m.PasswordHash, nil
}

func (m *MockAccountRepo) CheckBanned(ctx context.Context, playerID int64, deviceID string) (bool, error) {
	return false, nil
}
