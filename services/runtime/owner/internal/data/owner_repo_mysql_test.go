// owner_repo_mysql_test.go — owner 权威状态机真实 MySQL 集成测试(owner-authority.md §5)。
//
// PANDORA_TEST_MYSQL_DSN 必须是不带 database 的专用测试实例 DSN(同 inventory/bag 模式)。
// 测试只创建/删除名称严格匹配 pandora_owner_it_* 的随机临时库;未设置时明确 Skip。
//
// 覆盖(§16 风险对应验证):
//   - 首迁移无屏障 + Begin/Admit 幂等重放(响应丢失安全);
//   - epoch CAS 冲突(并发双迁移一胜一败);
//   - admit_not_before = CAS 时点旧租约观察值 + 余量;早到 Admit 拒;Begin 后旧实例续租
//     不回写已算屏障;
//   - Admit exact 身份全等(旧 epoch / 换实例 UID 拒);
//   - Release 幂等(迟到旧 operation no-op);
//   - 实例租约只前进 + 实例纪元不符拒。
package data

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

const ownerMySQLTestTimeout = 15 * time.Second

var ownerMySQLTestDBPattern = regexp.MustCompile(`^pandora_owner_it_[0-9]+_[0-9a-f]{12}$`)

type ownerMySQLFixture struct {
	admin   *sql.DB
	db      *sql.DB
	dbName  string
	created bool
}

func openOwnerMySQLFixture(t *testing.T) *ownerMySQLFixture {
	t.Helper()
	serverDSN := strings.TrimSpace(os.Getenv("PANDORA_TEST_MYSQL_DSN"))
	if serverDSN == "" {
		t.Skip("未设置 PANDORA_TEST_MYSQL_DSN,跳过 owner 权威真 MySQL 集成测试")
	}
	cfg, err := mysql.ParseDSN(serverDSN)
	if err != nil {
		t.Fatalf("解析 PANDORA_TEST_MYSQL_DSN: %v", err)
	}
	if cfg.DBName != "" {
		t.Fatalf("PANDORA_TEST_MYSQL_DSN 禁止带 database,got=%q", cfg.DBName)
	}
	cfg.MultiStatements = true
	cfg.ParseTime = true
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = ownerMySQLTestTimeout
	cfg.WriteTimeout = ownerMySQLTestTimeout

	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatalf("打开 MySQL 管理连接: %v", err)
	}
	f := &ownerMySQLFixture{admin: admin}
	t.Cleanup(func() { f.cleanup(t) })
	ctx, cancel := context.WithTimeout(context.Background(), ownerMySQLTestTimeout)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("已设置测试 DSN 但 MySQL 不可达: %v", err)
	}

	var randBytes [6]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		t.Fatalf("生成随机库名: %v", err)
	}
	f.dbName = fmt.Sprintf("pandora_owner_it_%d_%s", time.Now().UnixNano(), hex.EncodeToString(randBytes[:]))
	if !ownerMySQLTestDBPattern.MatchString(f.dbName) {
		t.Fatalf("随机测试库名未通过安全校验: %q", f.dbName)
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+f.dbName+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatalf("创建随机测试库 %s: %v", f.dbName, err)
	}
	f.created = true

	if _, err := admin.ExecContext(ctx, readOwnerMySQLSchema(t, f.dbName)); err != nil {
		t.Fatalf("初始化 owner schema: %v", err)
	}
	testCfg := cfg.Clone()
	testCfg.DBName = f.dbName
	db, err := sql.Open("mysql", testCfg.FormatDSN())
	if err != nil {
		t.Fatalf("打开测试库连接: %v", err)
	}
	f.db = db
	return f
}

func (f *ownerMySQLFixture) cleanup(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), ownerMySQLTestTimeout)
	defer cancel()
	if f.db != nil {
		_ = f.db.Close()
	}
	if f.created && ownerMySQLTestDBPattern.MatchString(f.dbName) {
		if _, err := f.admin.ExecContext(ctx, "DROP DATABASE IF EXISTS `"+f.dbName+"`"); err != nil {
			t.Errorf("删除随机测试库 %s: %v", f.dbName, err)
		}
	}
	_ = f.admin.Close()
}

func readOwnerMySQLSchema(t *testing.T, dbName string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("定位测试文件路径失败")
	}
	path := filepath.Clean(filepath.Join(filepath.Dir(thisFile),
		"..", "..", "..", "..", "..", "deploy", "mysql-init", "15-owner-tables.sql"))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 owner schema %s: %v", path, err)
	}
	schema := string(b)
	const needle = "USE `pandora_owner`;"
	if strings.Count(schema, needle) != 1 {
		t.Fatalf("owner schema USE 锚点数量异常: %d", strings.Count(schema, needle))
	}
	return strings.Replace(schema, needle, "USE `"+dbName+"`;", 1)
}

func testTarget(uid string) OwnerTarget {
	return OwnerTarget{
		PodName: "hub-" + uid, InstanceUID: uid, InstanceEpoch: 1,
		AssignmentOrAllocationID: "assign-" + uid, ReleaseTrack: "stable",
	}
}

const testOpA = "6f9619ff-8b86-4d01-b42d-00cf4fc964ff"
const testOpB = "7f9619ff-8b86-4d01-b42d-00cf4fc964aa"
const testOpC = "8f9619ff-8b86-4d01-b42d-00cf4fc964bb"

func TestOwnerRepoMySQL(t *testing.T) {
	f := openOwnerMySQLFixture(t)
	repo := NewMySQLOwnerRepo(f.db)
	ctx := context.Background()

	t.Run("FirstTransitionNoBarrierAndIdempotentReplay", func(t *testing.T) {
		const player = 301
		target := testTarget("uid-a")
		rec, err := repo.BeginTransition(ctx, player, 0, testOpA, OwnerTypeHub, target, 5*time.Second)
		if err != nil || rec.OwnerEpoch != 1 || rec.Phase != OwnerPhasePending {
			t.Fatalf("首迁移: %+v err=%v", rec, err)
		}
		if rec.AdmitNotBeforeMs > time.Now().UnixMilli() {
			t.Fatalf("首迁移(无旧 owner)不应有屏障: admit=%d", rec.AdmitNotBeforeMs)
		}
		// Begin 幂等重放:同 operation 原样返回,epoch 不再推进。
		replay, err := repo.BeginTransition(ctx, player, 0, testOpA, OwnerTypeHub, target, 5*time.Second)
		if err != nil || replay.OwnerEpoch != 1 {
			t.Fatalf("Begin 重放: %+v err=%v", replay, err)
		}
		// Admit 成功 + 幂等重放。
		admitted, retry, err := repo.Admit(ctx, player, 1, testOpA, target)
		if err != nil || retry != 0 || admitted.Phase != OwnerPhaseAdmitted {
			t.Fatalf("Admit: %+v retry=%d err=%v", admitted, retry, err)
		}
		again, _, err := repo.Admit(ctx, player, 1, testOpA, target)
		if err != nil || again.Phase != OwnerPhaseAdmitted {
			t.Fatalf("Admit 重放: %+v err=%v", again, err)
		}
	})

	t.Run("EpochConflictAndConcurrentBegin", func(t *testing.T) {
		const player = 302
		target := testTarget("uid-b")
		if _, err := repo.BeginTransition(ctx, player, 0, testOpA, OwnerTypeHub, target, 0); err != nil {
			t.Fatalf("首迁移: %v", err)
		}
		// 过期期望 → 冲突并附当前记录。
		cur, err := repo.BeginTransition(ctx, player, 0, testOpB, OwnerTypeHub, testTarget("uid-b2"), 0)
		if errcode.As(err) != errcode.ErrOwnerEpochConflict || cur.OwnerEpoch != 1 {
			t.Fatalf("epoch 冲突: %+v err=%v", cur, err)
		}
		// 并发双迁移(同 expect):恰好一胜一冲突。
		var wg sync.WaitGroup
		errs := make([]error, 2)
		ops := []string{testOpB, testOpC}
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				_, errs[idx] = repo.BeginTransition(ctx, player, 1, ops[idx], OwnerTypeBattle,
					testTarget(fmt.Sprintf("uid-b-race-%d", idx)), 0)
			}(i)
		}
		wg.Wait()
		wins, conflicts := 0, 0
		for _, e := range errs {
			switch {
			case e == nil:
				wins++
			case errcode.As(e) == errcode.ErrOwnerEpochConflict:
				conflicts++
			default:
				t.Fatalf("并发 Begin 意外错误: %v", e)
			}
		}
		if wins != 1 || conflicts != 1 {
			t.Fatalf("并发双迁移应恰好一胜一冲突: wins=%d conflicts=%d", wins, conflicts)
		}
	})

	t.Run("BarrierFromOldLeaseObservedAtCAS", func(t *testing.T) {
		const player = 303
		oldTarget := testTarget("uid-c-old")
		newTarget := testTarget("uid-c-new")
		if _, err := repo.BeginTransition(ctx, player, 0, testOpA, OwnerTypeHub, oldTarget, 0); err != nil {
			t.Fatalf("旧 owner 建立: %v", err)
		}
		if _, _, err := repo.Admit(ctx, player, 1, testOpA, oldTarget); err != nil {
			t.Fatalf("旧 owner admit: %v", err)
		}
		// 旧实例续租 10s → 屏障应 ≥ 旧租约截止 + 余量。
		oldDeadline, err := repo.RenewInstanceLease(ctx, oldTarget, 10*time.Second)
		if err != nil {
			t.Fatalf("旧实例续租: %v", err)
		}
		const margin = 5 * time.Second
		rec, err := repo.BeginTransition(ctx, player, 1, testOpB, OwnerTypeBattle, newTarget, margin)
		if err != nil {
			t.Fatalf("迁移到新 owner: %v", err)
		}
		wantMin := oldDeadline + margin.Milliseconds()
		if rec.AdmitNotBeforeMs < wantMin {
			t.Fatalf("屏障应 ≥ 旧租约+余量: admit=%d want>=%d", rec.AdmitNotBeforeMs, wantMin)
		}
		// 早到 Admit 拒(带 retry_after)。
		_, retry, err := repo.Admit(ctx, player, 2, testOpB, newTarget)
		if errcode.As(err) != errcode.ErrOwnerBarrierNotOpen || retry <= 0 {
			t.Fatalf("早到 Admit 应拒: retry=%d err=%v", retry, err)
		}
		// Begin 后旧实例继续续租,不回写已算屏障(取 CAS 时点观察值)。
		if _, err := repo.RenewInstanceLease(ctx, oldTarget, 20*time.Second); err != nil {
			t.Fatalf("Begin 后旧实例续租: %v", err)
		}
		after, err := repo.Query(ctx, player)
		if err != nil || after.AdmitNotBeforeMs != rec.AdmitNotBeforeMs {
			t.Fatalf("屏障被后续续租改写: before=%d after=%d err=%v", rec.AdmitNotBeforeMs, after.AdmitNotBeforeMs, err)
		}
		// 身份不匹配拒:旧 epoch / 换实例 UID。
		if _, _, err := repo.Admit(ctx, player, 1, testOpA, oldTarget); errcode.As(err) != errcode.ErrOwnerIdentityMismatch {
			t.Fatalf("旧 epoch Admit 应拒: %v", err)
		}
		forged := newTarget
		forged.InstanceUID = "uid-c-forged"
		if _, _, err := repo.Admit(ctx, player, 2, testOpB, forged); errcode.As(err) != errcode.ErrOwnerIdentityMismatch {
			t.Fatalf("换实例 Admit 应拒: %v", err)
		}
	})

	t.Run("ReleaseIdempotentLateNoop", func(t *testing.T) {
		const player = 304
		target := testTarget("uid-d")
		if _, err := repo.BeginTransition(ctx, player, 0, testOpA, OwnerTypeHub, target, 0); err != nil {
			t.Fatalf("建立: %v", err)
		}
		if _, _, err := repo.Admit(ctx, player, 1, testOpA, target); err != nil {
			t.Fatalf("admit: %v", err)
		}
		rec, err := repo.Release(ctx, player, 1, testOpA)
		if err != nil || rec.OwnerType != OwnerTypeNone || rec.OwnerEpoch != 1 {
			t.Fatalf("释放: %+v err=%v", rec, err)
		}
		// 迟到 Release(旧 operation)幂等 no-op。
		late, err := repo.Release(ctx, player, 1, testOpB)
		if err != nil || late.OwnerType != OwnerTypeNone {
			t.Fatalf("迟到释放应 no-op: %+v err=%v", late, err)
		}
	})

	t.Run("LeaseMonotonicAndEpochGuard", func(t *testing.T) {
		target := testTarget("uid-e")
		first, err := repo.RenewInstanceLease(ctx, target, 10*time.Second)
		if err != nil {
			t.Fatalf("首续: %v", err)
		}
		shorter, err := repo.RenewInstanceLease(ctx, target, 1*time.Second)
		if err != nil || shorter < first {
			t.Fatalf("deadline 不得回退: first=%d shorter=%d err=%v", first, shorter, err)
		}
		replaced := target
		replaced.InstanceEpoch = 2
		if _, err := repo.RenewInstanceLease(ctx, replaced, 5*time.Second); errcode.As(err) != errcode.ErrOwnerLeaseRegressed {
			t.Fatalf("实例纪元不符续租应拒: %v", err)
		}
	})
}
