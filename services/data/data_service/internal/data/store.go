// Package data 是 data_service 的数据层(MySQL 版本化 blob + Redis 缓存,2026-06-16)。
//
// 库表(deploy/mysql-init/07-data-tables.sql,pandora_player 库):
//
//	player_data  玩家数据 blob(player_id PK;version 乐观锁;data 为序列化 PlayerProfile bytes)
//
// 表是结构化列直接映射(CLAUDE.md §5.9 关系型表不强制 proto bytes blob);
// data 列本身是上层业务序列化好的不透明 bytes,data_service 不解释其内容。
//
// 持久层基于 proto2mysql(v0.0.22):PlayerData proto 直接映射 player_data 表
// (WithTableName),不再手写 SQL。注意该库对 bytes 字段按 base64 文本存储
// (写入 base64 编码/读取解码,库内对称可逆):切换前用旧实现写入的 raw 二进制
// 存量行读取会解码失败,dev 环境需清空 player_data 表。schema 仍归 mysql-init
// 管理,不使用库的自动 DDL。
package data

import (
	"context"
	"database/sql"
	"errors"

	pbmysql "github.com/luyuancpp/proto2mysql"

	"github.com/luyuancpp/pandora/pkg/errcode"
	datav1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/data_service/v1"
)

// PlayerStore 是玩家数据的持久层抽象。biz 只依赖此接口,不依赖 *sql.DB。
type PlayerStore interface {
	// Read 读玩家数据。not found → (nil, false, nil)。
	Read(ctx context.Context, playerID uint64) (*datav1.PlayerData, bool, error)

	// Write 乐观锁写:
	//   expectVersion == 0 → 视为新建,INSERT(冲突即已存在 → ErrDataVersionMismatch);
	//   expectVersion  > 0 → UPDATE ... WHERE player_id=? AND version=expectVersion,
	//                        受影响行 0(版本不匹配 / 不存在)→ ErrDataVersionMismatch。
	// 成功返回写入后的新版本号(= expectVersion + 1)。
	Write(ctx context.Context, playerID uint64, expectVersion int32, data []byte) (int32, error)
}

// MySQLPlayerStore 是基于 proto2mysql 的 PlayerStore 实现。
type MySQLPlayerStore struct {
	pdb *pbmysql.PbMysqlDB
}

// NewMySQLPlayerStore 构造。db 由 pkg/mysqlx.MustNewClient 提供(连 pandora_player 库)。
func NewMySQLPlayerStore(db *sql.DB) *MySQLPlayerStore {
	pdb := pbmysql.NewPbMysqlDB()
	// DSN 已选定 pandora_player 库,直接复用连接;不走 OpenDB 的 USE,
	// 也不用自动 DDL(schema 归 mysql-init),故 DBName 留空。
	pdb.DB = db
	pdb.RegisterTable(&datav1.PlayerData{},
		pbmysql.WithTableName("player_data"),
		pbmysql.WithPrimaryKey("player_id"),
	)
	return &MySQLPlayerStore{pdb: pdb}
}

func (s *MySQLPlayerStore) Read(ctx context.Context, playerID uint64) (*datav1.PlayerData, bool, error) {
	row := &datav1.PlayerData{PlayerId: playerID}
	if err := s.pdb.WithContext(ctx).FindOneByPK(row); err != nil {
		if errors.Is(err, pbmysql.ErrNoRowsFound) {
			return nil, false, nil
		}
		return nil, false, errcode.New(errcode.ErrInternal, "read player_data %d: %v", playerID, err)
	}
	return row, true, nil
}

func (s *MySQLPlayerStore) Write(ctx context.Context, playerID uint64, expectVersion int32, data []byte) (int32, error) {
	pdb := s.pdb.WithContext(ctx)

	if expectVersion == 0 {
		// 新建:INSERT 起始版本 1。主键冲突说明已存在(并发或客户端版本陈旧)→ 版本不匹配。
		row := &datav1.PlayerData{PlayerId: playerID, Version: 1, Data: data}
		if err := pdb.Insert(row); err != nil {
			if errors.Is(err, pbmysql.ErrDuplicateKey) {
				return 0, errcode.New(errcode.ErrDataVersionMismatch,
					"player_data %d already exists (expect new)", playerID)
			}
			return 0, errcode.New(errcode.ErrInternal, "insert player_data %d: %v", playerID, err)
		}
		return 1, nil
	}

	// 更新:乐观锁 CAS(WHERE version 匹配,版本 +1)。用显式字段版接口,
	// 避免 proto3 零值字段(如空 data)被 Has() 跳过。
	row := &datav1.PlayerData{PlayerId: playerID, Version: expectVersion, Data: data}
	ok, err := pdb.UpdateFieldsIfVersion(row, "version", "data")
	if err != nil {
		return 0, errcode.New(errcode.ErrInternal, "update player_data %d: %v", playerID, err)
	}
	if !ok {
		return 0, errcode.New(errcode.ErrDataVersionMismatch,
			"player_data %d version mismatch (expect %d)", playerID, expectVersion)
	}
	return expectVersion + 1, nil
}
