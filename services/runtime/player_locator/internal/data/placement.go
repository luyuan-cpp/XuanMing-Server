package data

import (
	"context"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
)

// PlacementRepo stores the durable player route. Unlike LocationRepo, this key
// has no Redis TTL: network presence expiry must never erase routing authority.
type PlacementRepo interface {
	GetPlacement(context.Context, uint64) (*locatorv1.PlayerPlacementStorageRecord, bool, error)
	UpdatePlacement(context.Context, uint64, int, func(*locatorv1.PlayerPlacementStorageRecord, bool) (*locatorv1.PlayerPlacementStorageRecord, error)) (*locatorv1.PlayerPlacementStorageRecord, error)
	// UpdatePlacementWithBattleTerminalFence watches the durable, version-free
	// per-player terminal tombstone in the same Redis transaction as placement.
	// Match-start Begin/Bind/Admission use it so terminal publication cannot race
	// a placement CAS. Both keys share {playerID}.
	UpdatePlacementWithBattleTerminalFence(context.Context, uint64, uint64, int, func(*locatorv1.PlayerPlacementStorageRecord, bool) (*locatorv1.PlayerPlacementStorageRecord, error)) (*locatorv1.PlayerPlacementStorageRecord, error)
}

type RedisPlacementRepo struct {
	rdb redis.UniversalClient
}

func NewRedisPlacementRepo(rdb redis.UniversalClient) *RedisPlacementRepo {
	return &RedisPlacementRepo{rdb: rdb}
}

// PlacementKey exports the key contract for cluster-slot tests and migrations.
func PlacementKey(playerID uint64) string {
	return fmt.Sprintf("pandora:placement:{%d}", playerID)
}

func (r *RedisPlacementRepo) GetPlacement(ctx context.Context, playerID uint64) (*locatorv1.PlayerPlacementStorageRecord, bool, error) {
	if playerID == 0 {
		return nil, false, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	b, err := r.rdb.Get(ctx, PlacementKey(playerID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errcode.NewCause(errcode.ErrUnavailable, err, "read placement authority failed")
	}
	rec := new(locatorv1.PlayerPlacementStorageRecord)
	if err := proto.Unmarshal(b, rec); err != nil {
		return nil, false, errcode.NewCause(errcode.ErrUnavailable, err, "decode placement authority failed")
	}
	if rec.GetPlayerId() != playerID || rec.GetVersion() == 0 {
		return nil, false, errcode.New(errcode.ErrUnavailable, "placement authority is malformed")
	}
	return rec, true, nil
}

func (r *RedisPlacementRepo) UpdatePlacement(
	ctx context.Context,
	playerID uint64,
	maxRetry int,
	mutate func(*locatorv1.PlayerPlacementStorageRecord, bool) (*locatorv1.PlayerPlacementStorageRecord, error),
) (*locatorv1.PlayerPlacementStorageRecord, error) {
	return r.updatePlacement(ctx, playerID, 0, maxRetry, mutate)
}

func (r *RedisPlacementRepo) UpdatePlacementWithBattleTerminalFence(
	ctx context.Context,
	playerID, matchID uint64,
	maxRetry int,
	mutate func(*locatorv1.PlayerPlacementStorageRecord, bool) (*locatorv1.PlayerPlacementStorageRecord, error),
) (*locatorv1.PlayerPlacementStorageRecord, error) {
	if matchID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "battle terminal fence match_id required")
	}
	return r.updatePlacement(ctx, playerID, matchID, maxRetry, mutate)
}

func (r *RedisPlacementRepo) updatePlacement(
	ctx context.Context,
	playerID, battleFenceMatchID uint64,
	maxRetry int,
	mutate func(*locatorv1.PlayerPlacementStorageRecord, bool) (*locatorv1.PlayerPlacementStorageRecord, error),
) (*locatorv1.PlayerPlacementStorageRecord, error) {
	if playerID == 0 || mutate == nil {
		return nil, errcode.New(errcode.ErrInvalidArg, "player_id and placement mutator required")
	}
	if maxRetry < 0 {
		maxRetry = 0
	}
	key := PlacementKey(playerID)
	watchKeys := []string{key}
	if battleFenceMatchID != 0 {
		watchKeys = append(watchKeys, placement.BattleTerminalFenceKey(playerID, battleFenceMatchID))
	}
	for attempt := 0; attempt <= maxRetry; attempt++ {
		var result *locatorv1.PlayerPlacementStorageRecord
		var businessErr error
		err := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			if battleFenceMatchID != 0 {
				exists, err := tx.Exists(ctx, placement.BattleTerminalFenceKey(playerID, battleFenceMatchID)).Result()
				if err != nil {
					return err
				}
				if exists != 0 {
					businessErr = errcode.New(errcode.ErrInvalidState,
						"battle %d has a durable terminal/leave fence", battleFenceMatchID)
					return businessErr
				}
			}
			cur, found, err := readPlacement(ctx, tx, key)
			if err != nil {
				return err
			}
			next, err := mutate(cur, found)
			if err != nil {
				businessErr = err
				return err
			}
			if next == nil || next.GetPlayerId() != playerID || next.GetVersion() == 0 {
				businessErr = errcode.New(errcode.ErrInvalidState, "placement mutator produced invalid record")
				return businessErr
			}
			encoded, err := proto.Marshal(next)
			if err != nil {
				return err
			}
			if _, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				// Deliberately no EXPIRE: placement is durable authority, not presence.
				pipe.Set(ctx, key, encoded, 0)
				return nil
			}); err != nil {
				return err
			}
			result = proto.Clone(next).(*locatorv1.PlayerPlacementStorageRecord)
			return nil
		}, watchKeys...)
		if err == nil {
			return result, nil
		}
		if businessErr != nil && err == businessErr {
			return nil, businessErr
		}
		if errors.Is(err, redis.TxFailedErr) {
			continue
		}
		return nil, errcode.NewCause(errcode.ErrUnavailable, err, "update placement authority failed")
	}
	return nil, errcode.New(errcode.ErrLocatorConflict, "placement CAS retry exhausted")
}

func readPlacement(ctx context.Context, c redis.Cmdable, key string) (*locatorv1.PlayerPlacementStorageRecord, bool, error) {
	b, err := c.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	rec := new(locatorv1.PlayerPlacementStorageRecord)
	if err := proto.Unmarshal(b, rec); err != nil {
		return nil, false, err
	}
	return rec, true, nil
}
