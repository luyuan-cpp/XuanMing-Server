// Package data 是 data_service 的数据层(MySQL 强 schema 列 + Redis pb 二进制缓存,2026-06-20)。
//
// 库表(pandora_player 库 player_data 表):schema 唯一来源是 PlayerData proto,
// 启动时经 proto2mysql CreateOrUpdateTable 按 pb 字段建表/同步表结构
// (表不存在则 CREATE;存在则补缺列/对齐类型,不删多余列),不再手写 SQL。
// 每个标量字段即一列,MySQL 直接可见、可查询。
//
// ⚠️ dev 迁移:旧表含 `data BLOB NOT NULL` 列(已从 proto 删除),该列不会被自动
// 删除且无默认值,会导致 INSERT 失败——dev 环境切换前需 DROP TABLE player_data,
// 由服务启动时按新 schema 重建。
package data

import (
	"context"
	"database/sql"
	"errors"

	pbmysql "github.com/luyuancpp/proto2mysql"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	datav1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/data_service/v1"
)

// playerDataFields 是乐观锁 CAS 更新时 SET 的业务列(除主键与 version 外的全部 proto 字段)。
// 新增 proto 字段后必须同步追加,否则该字段写不进 MySQL。
var playerDataFields = []string{
	"nickname", "level", "mmr", "avatar",
	"created_at_ms", "last_seen_ms", "total_battles", "total_wins",
}

// PlayerStore 是玩家数据的持久层抽象。biz 只依赖此接口,不依赖 *sql.DB。
type PlayerStore interface {
	// Read 读玩家数据。not found → (nil, false, nil)。
	Read(ctx context.Context, playerID uint64) (*datav1.PlayerData, bool, error)

	// Write 乐观锁整条写(pd.Version 为期望版本):
	//   version == 0 → 视为新建,INSERT 起始版本 1(冲突即已存在 → ErrDataVersionMismatch);
	//   version  > 0 → UPDATE ... WHERE player_id=? AND version=?,
	//                  受影响行 0(版本不匹配 / 不存在)→ ErrDataVersionMismatch。
	// 成功返回写入后的新版本号(= 期望版本 + 1)。不修改入参 pd。
	Write(ctx context.Context, pd *datav1.PlayerData) (uint32, error)
}

// MySQLPlayerStore 是基于 proto2mysql 的 PlayerStore 实现。
type MySQLPlayerStore struct {
	pdb *pbmysql.PbMysqlDB
}

// NewMySQLPlayerStore 构造。db 由 pkg/mysqlx.MustNewClient 提供(连 pandora_player 库)。
// 启动时按 pb 定义 CreateOrUpdateTable:表不存在则建,存在则补缺列/对齐类型
// (pb 是 schema 唯一来源;失败返回错误,由 main 直接退出)。
func NewMySQLPlayerStore(db *sql.DB) (*MySQLPlayerStore, error) {
	pdb := pbmysql.NewPbMysqlDB()
	// DSN 已选定 pandora_player 库,直接复用连接;DBName 从连接取
	// (CreateOrUpdateTable 查 information_schema 需要库名)。
	pdb.DB = db
	if err := db.QueryRow("SELECT DATABASE()").Scan(&pdb.DBName); err != nil {
		return nil, errcode.New(errcode.ErrInternal, "resolve current database: %v", err)
	}
	if pdb.DBName == "" {
		return nil, errcode.New(errcode.ErrInternal, "mysql dsn selects no database (need pandora_player)")
	}
	pdb.RegisterTable(&datav1.PlayerData{},
		pbmysql.WithTableName("player_data"),
		pbmysql.WithPrimaryKey("player_id"),
	)
	if err := pdb.CreateOrUpdateTable(&datav1.PlayerData{}); err != nil {
		return nil, errcode.New(errcode.ErrInternal, "create/update table player_data: %v", err)
	}
	return &MySQLPlayerStore{pdb: pdb}, nil
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

func (s *MySQLPlayerStore) Write(ctx context.Context, pd *datav1.PlayerData) (uint32, error) {
	pdb := s.pdb.WithContext(ctx)
	expectVersion := pd.GetVersion()

	// 不改入参:克隆一份作写入行(CLAUDE.md §5.10 proto 禁止值拷贝,用 proto.Clone)。
	row := proto.Clone(pd).(*datav1.PlayerData)

	if expectVersion == 0 {
		// 新建:INSERT 起始版本 1。主键冲突说明已存在(并发或客户端版本陈旧)→ 版本不匹配。
		row.Version = 1
		if err := pdb.Insert(row); err != nil {
			if errors.Is(err, pbmysql.ErrDuplicateKey) {
				return 0, errcode.New(errcode.ErrDataVersionMismatch,
					"player_data %d already exists (expect new)", pd.GetPlayerId())
			}
			return 0, errcode.New(errcode.ErrInternal, "insert player_data %d: %v", pd.GetPlayerId(), err)
		}
		return 1, nil
	}

	// 更新:乐观锁 CAS(WHERE version 匹配,版本 +1),SET 全部业务列。
	// 用显式字段版接口,避免 proto3 零值字段(如 level=0)被 Has() 跳过。
	ok, err := pdb.UpdateFieldsIfVersion(row, "version", playerDataFields...)
	if err != nil {
		return 0, errcode.New(errcode.ErrInternal, "update player_data %d: %v", pd.GetPlayerId(), err)
	}
	if !ok {
		return 0, errcode.New(errcode.ErrDataVersionMismatch,
			"player_data %d version mismatch (expect %d)", pd.GetPlayerId(), expectVersion)
	}
	return expectVersion + 1, nil
}
