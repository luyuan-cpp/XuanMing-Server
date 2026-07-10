// Package data 是 data_service 的数据层(MySQL 强 schema 列 + Redis pb 二进制缓存,2026-06-20)。
//
// 库表(pandora_player 库 player_data 表):schema 唯一来源是 PlayerData proto,
// 表名/主键写在 proto option 里(proto2mysql.table_name/primary_key),启动时经
// proto2mysql v0.0.28 的 RegisterAllTables 自动扫描注册 + SyncAllTables 按 pb
// 建表/同步表结构(表不存在则 CREATE;存在则补缺列/对齐类型,不删多余列),不再手写 SQL,
// 也不再手写 RegisterTable。每个标量字段即一列,MySQL 直接可见、可查询。
//
// ⚠️ dev 迁移(data_service 从未部署上线、无有效历史数据,故按开发期重置处理):
// 旧表若残留 `data BLOB NOT NULL` 列(已从 proto 删除),该列不会被自动删除且无默认值,
// 会导致新 schema 的 INSERT 失败;旧 Redis 缓存也是旧 pb 结构。切换前直接清空即可:
//
//	MySQL:  DROP TABLE IF EXISTS pandora_player.player_data;(服务启动时按新 pb 重建)
//	Redis:  DEL pandora:data:player:*(旧结构缓存作废)
//
// 因该服务从未产生有效数据,PlayerData 复用字段编号 3-10 属 CLAUDE.md §5.4 允许的开发期例外。
package data

import (
	"context"
	"database/sql"
	"errors"

	pbmysql "github.com/luyuancpp/proto2mysql"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	datav1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/data_service/v1"

	// blank import:触发 proto2mysql option 扩展注册(proto2mysql.db/table_name/primary_key 等),
	// 否则运行时无法把 data_service 描述符上的 option 解析为已知扩展,
	// RegisterAllTables 扫不到 (proto2mysql.table_name)。
	_ "github.com/luyuancpp/pandora/proto/gen/go/proto2mysql"
)

// playerDataUpdateFields 是乐观锁 CAS 更新时 SET 的业务列:PlayerData 的全部字段
// 去掉主键(player_id)与乐观锁列(version)。从 PlayerData 的 proto 描述符动态推导,
// 新增 proto 字段自动纳入,无需手工维护——避免漏列导致新字段永远写不进 MySQL。
var playerDataUpdateFields = buildPlayerDataUpdateFields()

// playerDataUpdatableSet 是 playerDataUpdateFields 的集合形态,供 update_mask 路径合法性校验
// (O(1) 命中判断),同样从 proto 描述符动态推导。
var playerDataUpdatableSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(playerDataUpdateFields))
	for _, f := range playerDataUpdateFields {
		m[f] = struct{}{}
	}
	return m
}()

// playerDataPKField / playerDataVersionField 与 proto option / message 保持一致:
// player_id 是主键(不参与 SET),version 是乐观锁列(由 CAS 单独 +1,不进 SET)。
const (
	playerDataPKField      = "player_id"
	playerDataVersionField = "version"
)

// IsPlayerDataUpdatableField 判断给定字段名(PlayerData 的 proto 字段名 snake_case)是否是
// 可经 update_mask 更新的业务列(即非主键、非 version 的标量列)。供 biz 校验 update_mask 路径。
func IsPlayerDataUpdatableField(name string) bool {
	_, ok := playerDataUpdatableSet[name]
	return ok
}

// buildPlayerDataUpdateFields 遍历 PlayerData 描述符,返回除主键与 version 外的全部字段名
// (proto 字段名 = snake_case,与 proto2mysql 建表的列名一致)。
func buildPlayerDataUpdateFields() []string {
	fields := (&datav1.PlayerData{}).ProtoReflect().Descriptor().Fields()
	names := make([]string, 0, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		name := string(fields.Get(i).Name())
		if name == playerDataPKField || name == playerDataVersionField {
			continue
		}
		names = append(names, name)
	}
	return names
}

// PlayerStore 是玩家数据的持久层抽象。biz 只依赖此接口,不依赖 *sql.DB。
type PlayerStore interface {
	// Read 读玩家数据。not found → (nil, false, nil)。
	Read(ctx context.Context, playerID uint64) (*datav1.PlayerData, bool, error)

	// Write 乐观锁整条写(pd.Version 为期望版本):
	//   version == 0 → 视为新建,INSERT 起始版本 1(冲突即已存在 → ErrDataVersionMismatch);
	//   version  > 0 → UPDATE ... WHERE player_id=? AND version=?,
	//                  受影响行 0(版本不匹配 / 不存在)→ ErrDataVersionMismatch。
	// updateFields 指定 UPDATE 要 SET 的业务列(snake_case proto 字段名):更新(version>0)时
	// **必须非空**——空掩码会全量覆盖并把调用方不认得的新列清零,破坏零停机滚动升级
	// (CLAUDE.md §9 不变量 17),故空掩码更新直接返回 ErrInvalidArg;非空 → 只 SET 掩码内的列。
	// 新建(version==0)始终整条 INSERT,忽略 updateFields。调用方须保证 updateFields
	// 已校验合法(不含 player_id/version/未知字段)。成功返回写入后的新版本号(= 期望版本 + 1)。不修改入参 pd。
	Write(ctx context.Context, pd *datav1.PlayerData, updateFields []string) (uint32, error)
}

// MySQLPlayerStore 是基于 proto2mysql 的 PlayerStore 实现。
type MySQLPlayerStore struct {
	pdb *pbmysql.DB
}

// NewMySQLPlayerStore 构造。db 由 pkg/mysqlx.MustNewClient 提供(连 pandora_player 库)。
// 启动时用 proto2mysql v0.0.28 的 RegisterAllTables 自动扫描已链接描述符,
// 注册所有声明了 (proto2mysql.db) + (proto2mysql.table_name) 的表(即 PlayerData),
// 再 SyncAllTables 按 pb 建表/同步表结构(表不存在则建,存在则补缺列/对齐类型)。
// pb 是 schema 唯一来源;失败返回错误,由 main 直接退出。
func NewMySQLPlayerStore(db *sql.DB) (*MySQLPlayerStore, error) {
	pdb := pbmysql.NewDB()
	// DSN 已选定 pandora_player 库,直接复用连接;DBName 从连接取
	// (SyncAllTables 查 information_schema 需要库名)。
	pdb.DB = db
	if err := db.QueryRow("SELECT DATABASE()").Scan(&pdb.DBName); err != nil {
		return nil, errcode.New(errcode.ErrInternal, "resolve current database: %v", err)
	}
	if pdb.DBName == "" {
		return nil, errcode.New(errcode.ErrInternal, "mysql dsn selects no database (need pandora_player)")
	}
	// 自动注册所有 proto option 声明的表(不再手写 RegisterTable);
	// PlayerData 必须在注册集里,否则后续 CRUD 找不到表。
	registered := pdb.RegisterAllTables()
	if !containsTable(registered, playerDataFullName) {
		return nil, errcode.New(errcode.ErrInternal,
			"PlayerData not auto-registered by RegisterAllTables (registered=%v); "+
				"check (proto2mysql.db)/(proto2mysql.table_name) options and blank import", registered)
	}
	if err := pdb.SyncAllTables(); err != nil {
		return nil, errcode.New(errcode.ErrInternal, "sync tables: %v", err)
	}
	return &MySQLPlayerStore{pdb: pdb}, nil
}

// playerDataFullName 是 PlayerData 的 proto full name,RegisterAllTables 用它作注册键。
const playerDataFullName = "pandora.data_service.v1.PlayerData"

// containsTable 判断 RegisterAllTables 返回的 proto full name 列表里是否含指定表。
func containsTable(registered []string, fullName string) bool {
	for _, name := range registered {
		if name == fullName {
			return true
		}
	}
	return false
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

func (s *MySQLPlayerStore) Write(ctx context.Context, pd *datav1.PlayerData, updateFields []string) (uint32, error) {
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

	// 更新:乐观锁 CAS(WHERE version 匹配,版本 +1)。必须带非空 updateFields —— 只 SET 掩码内
	// 的列(滚动升级时旧副本不清零它不认得的新列,CLAUDE.md §9 不变量 17)。空掩码会全量覆盖
	// 清零未知新列,这里兜底拒绝(biz 层已先校验,双保险防止绕过)。用显式字段版接口,避免 proto3
	// 零值字段(如 level=0)被 Has() 跳过。
	if len(updateFields) == 0 {
		return 0, errcode.New(errcode.ErrInvalidArg,
			"update player_data %d requires non-empty update_mask (empty mask would overwrite unknown new columns)", pd.GetPlayerId())
	}
	ok, err := pdb.UpdateFieldsIfVersion(row, playerDataVersionField, updateFields...)
	if err != nil {
		return 0, errcode.New(errcode.ErrInternal, "update player_data %d: %v", pd.GetPlayerId(), err)
	}
	if !ok {
		return 0, errcode.New(errcode.ErrDataVersionMismatch,
			"player_data %d version mismatch (expect %d)", pd.GetPlayerId(), expectVersion)
	}
	return expectVersion + 1, nil
}
