// Pandora 数据库迁移器。
//
// 迁移目标与凭据由外部 Secret 文件显式提供；二进制不会从业务配置猜测数据库，也不会
// 把 DSN 放进参数或日志。每个物理库独立执行并受硬超时约束，任一目标失败都会让整个
// 进程以非零状态退出，从而阻断后续业务发布。
package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	migratemysql "github.com/golang-migrate/migrate/v4/database/mysql"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

const (
	defaultTargetTimeoutSeconds   = 900
	defaultLockWaitTimeoutSeconds = 15
	minimumTargetTimeoutSeconds   = 30
	maximumTargetTimeoutSeconds   = 3600
	maximumLockWaitTimeoutSeconds = 60
	connectionTimeout             = 10 * time.Second
	workerShutdownGracePeriod     = 5 * time.Second
	mysqlAdvisoryLockWait         = 10 * time.Second
	advisoryLockWrapperGrace      = time.Second
	maximumSecretFileBytes        = 64 << 10
	schemaMigrationsTable         = "schema_migrations"
)

var (
	targetNamePattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	databaseNamePattern  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	migrationFilePattern = regexp.MustCompile(
		`^([0-9]+)_[a-z0-9_]+\.up\.sql$`,
	)
	mysqlAccountPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)
)

// migrationsFS 烘焙进 scratch 镜像，无需运行期 ConfigMap 或可写卷。
//
//go:embed all:migrations
var migrationsFS embed.FS

type targetManifest struct {
	Targets []migrationTarget `json:"targets"`
}

// migrationTarget 描述一个物理数据库。多个拍卖分片可以复用同一个 MigrationSet，
// 但必须分别给出唯一 Name、Database 与 DSNFile。
type migrationTarget struct {
	Name                     string `json:"name"`
	MigrationSet             string `json:"migration_set"`
	Database                 string `json:"database"`
	DSNFile                  string `json:"dsn_file"`
	BootstrapAdminDSNFile    string `json:"bootstrap_admin_dsn_file,omitempty"`
	TimeoutSeconds           int    `json:"timeout_seconds,omitempty"`
	LockWaitTimeoutSeconds   int    `json:"lock_wait_timeout_seconds,omitempty"`
	expectedMigrationVersion uint
}

type targetDescriptor struct {
	Name         string
	MigrationSet string
	Database     string
}

type commandConfig struct {
	TargetsFile     string
	ExpectedTargets string
	Environment     string
	Bootstrap       bool
	WorkerTarget    string
}

type migrationVersionReader interface {
	Version() (version uint, dirty bool, err error)
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.LUTC)
	if err := run(os.Args[1:]); err != nil {
		log.Printf("[migrate] 失败: %v", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := parseCommandConfig(args)
	if err != nil {
		return err
	}
	manifest, err := loadTargetManifest(cfg.TargetsFile)
	if err != nil {
		return err
	}
	expectedTargets, err := parseExpectedTargetInventory(cfg.ExpectedTargets)
	if err != nil {
		return err
	}
	if err := validateExpectedTargets(manifest.Targets, expectedTargets); err != nil {
		return err
	}
	requireTLS := !isDevelopmentEnvironment(cfg.Environment)
	if err := preflightTargets(manifest.Targets, cfg.Bootstrap, requireTLS); err != nil {
		return fmt.Errorf("目标预检失败: %w", err)
	}

	// worker 只迁移父进程指定的单目标。父进程用操作系统级进程超时兜底，避免某条
	// DDL 或驱动调用永久卡住。
	if cfg.WorkerTarget != "" {
		target, ok := findTarget(manifest.Targets, cfg.WorkerTarget)
		if !ok {
			return fmt.Errorf("worker 目标 %q 不在清单中", cfg.WorkerTarget)
		}
		return migrateTarget(target, requireTLS)
	}

	if cfg.Bootstrap {
		if !isDevelopmentEnvironment(cfg.Environment) {
			return fmt.Errorf("bootstrap 仅允许 local/dev/development，当前环境为 %q", cfg.Environment)
		}
		if err := bootstrapTargets(manifest.Targets); err != nil {
			return fmt.Errorf("开发环境 bootstrap 失败: %w", err)
		}
	}

	for _, target := range manifest.Targets {
		if err := runTargetWithDeadline(cfg.TargetsFile, cfg.Environment, expectedTargets, target); err != nil {
			return err
		}
	}
	log.Printf("[migrate] 全部 %d 个物理目标迁移并验收完成", len(manifest.Targets))
	return nil
}

func parseCommandConfig(args []string) (commandConfig, error) {
	bootstrapDefault, err := strictEnvBool("MIGRATE_BOOTSTRAP_DB", false)
	if err != nil {
		return commandConfig{}, err
	}

	set := flag.NewFlagSet("pandora-migrate", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	cfg := commandConfig{}
	set.StringVar(&cfg.TargetsFile, "targets-file", os.Getenv("MIGRATE_TARGETS_FILE"),
		"迁移目标清单文件（必须由外部 Secret 提供）")
	set.StringVar(&cfg.ExpectedTargets, "expected-targets", os.Getenv("MIGRATE_EXPECTED_TARGETS"),
		"发布侧独立审核的 name:migration_set:database 精确目标集合（逗号分隔）")
	set.StringVar(&cfg.Environment, "environment", envOr("PANDORA_ENV", "production"),
		"环境名；bootstrap 只允许 local/dev/development")
	set.BoolVar(&cfg.Bootstrap, "bootstrap", bootstrapDefault,
		"仅开发环境显式建库并给迁移账号授权，默认 false")
	set.StringVar(&cfg.WorkerTarget, "worker-target", "", "内部参数：只执行一个迁移目标")
	if err := set.Parse(args); err != nil {
		return commandConfig{}, fmt.Errorf("解析参数: %w", err)
	}
	if set.NArg() != 0 {
		return commandConfig{}, fmt.Errorf("存在未识别参数: %s", strings.Join(set.Args(), " "))
	}
	cfg.TargetsFile = strings.TrimSpace(cfg.TargetsFile)
	if cfg.TargetsFile == "" {
		return commandConfig{}, errors.New("必须通过 -targets-file 或 MIGRATE_TARGETS_FILE 显式指定目标清单")
	}
	abs, err := filepath.Abs(cfg.TargetsFile)
	if err != nil {
		return commandConfig{}, fmt.Errorf("解析目标清单绝对路径: %w", err)
	}
	cfg.TargetsFile = filepath.Clean(abs)
	cfg.ExpectedTargets = strings.TrimSpace(cfg.ExpectedTargets)
	if cfg.ExpectedTargets == "" {
		return commandConfig{}, errors.New("必须通过 -expected-targets 或 MIGRATE_EXPECTED_TARGETS 提供独立目标清单")
	}
	cfg.Environment = strings.ToLower(strings.TrimSpace(cfg.Environment))
	if cfg.Environment == "" {
		cfg.Environment = "production"
	}
	return cfg, nil
}

func loadTargetManifest(path string) (targetManifest, error) {
	data, err := readBoundedFile(path)
	if err != nil {
		return targetManifest{}, fmt.Errorf("读取目标清单: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var manifest targetManifest
	if err := decoder.Decode(&manifest); err != nil {
		return targetManifest{}, fmt.Errorf("解析目标清单 JSON: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return targetManifest{}, err
	}
	if len(manifest.Targets) == 0 {
		return targetManifest{}, errors.New("目标清单 targets 不能为空")
	}

	manifestDir := filepath.Dir(path)
	seenNames := make(map[string]struct{}, len(manifest.Targets))
	for i := range manifest.Targets {
		target := &manifest.Targets[i]
		target.Name = strings.TrimSpace(target.Name)
		target.MigrationSet = strings.TrimSpace(target.MigrationSet)
		target.Database = strings.TrimSpace(target.Database)
		if !targetNamePattern.MatchString(target.Name) {
			return targetManifest{}, fmt.Errorf("targets[%d].name=%q 非法", i, target.Name)
		}
		if _, exists := seenNames[target.Name]; exists {
			return targetManifest{}, fmt.Errorf("目标名 %q 重复", target.Name)
		}
		seenNames[target.Name] = struct{}{}
		if !databaseNamePattern.MatchString(target.MigrationSet) {
			return targetManifest{}, fmt.Errorf("目标 %s 的 migration_set=%q 非法", target.Name, target.MigrationSet)
		}
		if !databaseNamePattern.MatchString(target.Database) {
			return targetManifest{}, fmt.Errorf("目标 %s 的 database=%q 非法", target.Name, target.Database)
		}
		if !validMigrationDatabaseMapping(target.MigrationSet, target.Database) {
			return targetManifest{}, fmt.Errorf(
				"目标 %s 的 database=%q 必须等于 migration_set=%q 或使用其下划线分片前缀",
				target.Name, target.Database, target.MigrationSet,
			)
		}
		if strings.TrimSpace(target.DSNFile) == "" {
			return targetManifest{}, fmt.Errorf("目标 %s 缺少 dsn_file", target.Name)
		}
		resolvedDSN, err := resolveSecretPath(manifestDir, target.DSNFile)
		if err != nil {
			return targetManifest{}, fmt.Errorf("目标 %s 的 dsn_file: %w", target.Name, err)
		}
		target.DSNFile = resolvedDSN
		if target.BootstrapAdminDSNFile != "" {
			resolvedAdminDSN, err := resolveSecretPath(manifestDir, target.BootstrapAdminDSNFile)
			if err != nil {
				return targetManifest{}, fmt.Errorf("目标 %s 的 bootstrap_admin_dsn_file: %w", target.Name, err)
			}
			target.BootstrapAdminDSNFile = resolvedAdminDSN
		}
		if target.TimeoutSeconds == 0 {
			target.TimeoutSeconds = defaultTargetTimeoutSeconds
		}
		if target.TimeoutSeconds < minimumTargetTimeoutSeconds || target.TimeoutSeconds > maximumTargetTimeoutSeconds {
			return targetManifest{}, fmt.Errorf("目标 %s 的 timeout_seconds 必须在 %d..%d", target.Name, minimumTargetTimeoutSeconds, maximumTargetTimeoutSeconds)
		}
		if target.LockWaitTimeoutSeconds == 0 {
			target.LockWaitTimeoutSeconds = defaultLockWaitTimeoutSeconds
		}
		if target.LockWaitTimeoutSeconds < 1 || target.LockWaitTimeoutSeconds > maximumLockWaitTimeoutSeconds {
			return targetManifest{}, fmt.Errorf("目标 %s 的 lock_wait_timeout_seconds 必须在 1..%d", target.Name, maximumLockWaitTimeoutSeconds)
		}
		if target.LockWaitTimeoutSeconds >= target.TimeoutSeconds {
			return targetManifest{}, fmt.Errorf("目标 %s 的锁等待超时必须小于目标总超时", target.Name)
		}
		latest, err := latestMigrationVersion(target.MigrationSet)
		if err != nil {
			return targetManifest{}, fmt.Errorf("目标 %s: %w", target.Name, err)
		}
		target.expectedMigrationVersion = latest
	}
	return manifest, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("目标清单只能包含一个 JSON 对象")
		}
		return fmt.Errorf("目标清单尾部存在非法内容: %w", err)
	}
	return nil
}

func parseExpectedTargetInventory(raw string) ([]targetDescriptor, error) {
	parts := strings.Split(raw, ",")
	seenNames := make(map[string]struct{}, len(parts))
	descriptors := make([]targetDescriptor, 0, len(parts))
	for _, part := range parts {
		fields := strings.Split(strings.TrimSpace(part), ":")
		if len(fields) != 3 {
			return nil, fmt.Errorf("expected-targets 项 %q 必须是 name:migration_set:database", strings.TrimSpace(part))
		}
		descriptor := targetDescriptor{
			Name:         strings.TrimSpace(fields[0]),
			MigrationSet: strings.TrimSpace(fields[1]),
			Database:     strings.TrimSpace(fields[2]),
		}
		if !targetNamePattern.MatchString(descriptor.Name) {
			return nil, fmt.Errorf("expected-targets 中的目标名 %q 非法", descriptor.Name)
		}
		if !databaseNamePattern.MatchString(descriptor.MigrationSet) || !databaseNamePattern.MatchString(descriptor.Database) {
			return nil, fmt.Errorf("expected-targets 中目标 %s 的 migration_set/database 非法", descriptor.Name)
		}
		if !validMigrationDatabaseMapping(descriptor.MigrationSet, descriptor.Database) {
			return nil, fmt.Errorf("expected-targets 中目标 %s 的 database 与 migration_set 映射非法", descriptor.Name)
		}
		if _, exists := seenNames[descriptor.Name]; exists {
			return nil, fmt.Errorf("expected-targets 中的目标名 %q 重复", descriptor.Name)
		}
		seenNames[descriptor.Name] = struct{}{}
		descriptors = append(descriptors, descriptor)
	}
	if len(descriptors) == 0 {
		return nil, errors.New("expected-targets 不能为空")
	}
	sort.Slice(descriptors, func(i, j int) bool {
		return targetDescriptorKey(descriptors[i]) < targetDescriptorKey(descriptors[j])
	})
	return descriptors, nil
}

func validateExpectedTargets(targets []migrationTarget, expected []targetDescriptor) error {
	actualSet := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		actualSet[targetDescriptorKey(targetDescriptor{
			Name: target.Name, MigrationSet: target.MigrationSet, Database: target.Database,
		})] = struct{}{}
	}
	expectedSet := make(map[string]struct{}, len(expected))
	for _, descriptor := range expected {
		expectedSet[targetDescriptorKey(descriptor)] = struct{}{}
	}
	missing := make([]string, 0)
	for _, descriptor := range expected {
		key := targetDescriptorKey(descriptor)
		if _, exists := actualSet[key]; !exists {
			missing = append(missing, key)
		}
	}
	unexpected := make([]string, 0)
	for name := range actualSet {
		if _, exists := expectedSet[name]; !exists {
			unexpected = append(unexpected, name)
		}
	}
	sort.Strings(unexpected)
	if len(missing) != 0 || len(unexpected) != 0 {
		return fmt.Errorf("目标清单与发布 inventory 不一致: missing=%v unexpected=%v", missing, unexpected)
	}
	return nil
}

func targetDescriptorKey(descriptor targetDescriptor) string {
	return descriptor.Name + ":" + descriptor.MigrationSet + ":" + descriptor.Database
}

func formatExpectedTargetInventory(descriptors []targetDescriptor) string {
	parts := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		parts = append(parts, targetDescriptorKey(descriptor))
	}
	return strings.Join(parts, ",")
}

func validMigrationDatabaseMapping(migrationSet, database string) bool {
	return database == migrationSet || strings.HasPrefix(database, migrationSet+"_")
}

func latestMigrationVersion(migrationSet string) (uint, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations/"+migrationSet)
	if err != nil {
		return 0, fmt.Errorf("迁移集 migrations/%s 不存在: %w", migrationSet, err)
	}
	seen := make(map[uint]string)
	var latest uint
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		match := migrationFilePattern.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		value, err := strconv.ParseUint(match[1], 10, 64)
		if err != nil || value == 0 {
			return 0, fmt.Errorf("迁移文件版本非法: %s", entry.Name())
		}
		version := uint(value)
		if previous, exists := seen[version]; exists {
			return 0, fmt.Errorf("迁移版本 %d 重复: %s / %s", version, previous, entry.Name())
		}
		seen[version] = entry.Name()
		if version > latest {
			latest = version
		}
	}
	if latest == 0 {
		return 0, fmt.Errorf("迁移集 migrations/%s 没有 .up.sql", migrationSet)
	}
	return latest, nil
}

func preflightTargets(targets []migrationTarget, bootstrap, requireTLS bool) error {
	physicalTargets := make(map[string]string, len(targets))
	for _, target := range targets {
		cfg, err := readAndHardenDSN(target.DSNFile, target, requireTLS)
		if err != nil {
			return fmt.Errorf("目标 %s: %w", target.Name, err)
		}
		identity := cfg.Net + "|" + cfg.Addr + "|" + cfg.DBName
		if previous, exists := physicalTargets[identity]; exists {
			return fmt.Errorf("目标 %s 与 %s 指向同一物理库；每库只能声明一次", target.Name, previous)
		}
		physicalTargets[identity] = target.Name
		if bootstrap && strings.TrimSpace(target.BootstrapAdminDSNFile) == "" {
			return fmt.Errorf("目标 %s 开启 bootstrap 时必须提供 bootstrap_admin_dsn_file", target.Name)
		}
	}
	return nil
}

func readAndHardenDSN(path string, target migrationTarget, requireTLS bool) (*mysql.Config, error) {
	data, err := readBoundedFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 dsn_file: %w", err)
	}
	dsn := strings.TrimSpace(string(data))
	if dsn == "" {
		return nil, errors.New("dsn_file 为空")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("DSN 格式错误: %w", err)
	}
	if cfg.User == "" {
		return nil, errors.New("DSN 必须包含独立迁移账号")
	}
	if cfg.DBName != target.Database {
		return nil, fmt.Errorf("DSN database=%q 与清单 database=%q 不一致", cfg.DBName, target.Database)
	}
	if requireTLS {
		if cfg.TLSConfig != "true" || cfg.TLS == nil || cfg.TLS.InsecureSkipVerify || cfg.AllowFallbackToPlaintext {
			return nil, errors.New("production DSN 必须显式 tls=true，拒绝明文、preferred、skip-verify 与 plaintext fallback")
		}
	}
	cfg.MultiStatements = true
	cfg.ParseTime = true
	cfg.Timeout = connectionTimeout
	cfg.ReadTimeout = statementTimeout(target)
	cfg.WriteTimeout = statementTimeout(target)
	if cfg.Params == nil {
		cfg.Params = make(map[string]string)
	}
	// 通过 DSN 系统变量让连接池创建的每条连接都带有限 metadata/行锁等待，
	// 不能只在某条随机 session 上 SET。
	cfg.Params["lock_wait_timeout"] = strconv.Itoa(target.LockWaitTimeoutSeconds)
	cfg.Params["innodb_lock_wait_timeout"] = strconv.Itoa(target.LockWaitTimeoutSeconds)
	return cfg, nil
}

func migrateTarget(target migrationTarget, requireTLS bool) error {
	log.Printf("[migrate] 目标=%s migration_set=%s database=%s expected_version=%d 开始",
		target.Name, target.MigrationSet, target.Database, target.expectedMigrationVersion)
	cfg, err := readAndHardenDSN(target.DSNFile, target, requireTLS)
	if err != nil {
		return err
	}
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return fmt.Errorf("打开数据库连接: %w", err)
	}
	// golang-migrate 使用单连接即可；限制连接数也避免 session 级安全参数漂移。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	pingCtx, cancel := context.WithTimeout(context.Background(), connectionTimeout)
	err = db.PingContext(pingCtx)
	cancel()
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("数据库连接预检: %w", err)
	}

	driver, err := migratemysql.WithInstance(db, &migratemysql.Config{
		MigrationsTable:  schemaMigrationsTable,
		DatabaseName:     target.Database,
		StatementTimeout: statementTimeout(target),
	})
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("构造 MySQL migration driver: %w", err)
	}
	source, err := iofs.New(migrationsFS, "migrations/"+target.MigrationSet)
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("加载迁移集: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", source, target.Name, driver)
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("构造迁移器: %w", err)
	}
	// golang-migrate 的 MySQL driver 内部 GET_LOCK 固定等待 10 秒。wrapper 必须比它长，
	// 否则 wrapper 先返回时底层 goroutine 仍占同一连接，后续 Version 查询会并发撞连接。
	m.LockTimeout = advisoryLockTimeout(target)

	if err := rejectDirtyOrNewer(m, target.expectedMigrationVersion); err != nil {
		closeMigration(m)
		return err
	}
	applyErr := m.Up()
	if errors.Is(applyErr, migrate.ErrLockTimeout) {
		// 此时底层 Lock goroutine 可能仍在 I/O；不要再对同一连接 Version/Close。
		// migrateTarget 只在专用 worker 进程中运行，返回后进程立即退出并由 OS 回收连接。
		return fmt.Errorf("等待 migration advisory lock 超时，worker 停止且发布阻断: %w", applyErr)
	}
	version, dirty, versionErr := m.Version()
	closeErr := closeMigration(m)
	if applyErr != nil && !errors.Is(applyErr, migrate.ErrNoChange) {
		if versionErr == nil {
			return fmt.Errorf("执行 up 失败（当前 version=%d dirty=%v）: %w", version, dirty, applyErr)
		}
		return fmt.Errorf("执行 up 失败且无法读取版本（version_err=%v）: %w", versionErr, applyErr)
	}
	if err := validateFinalVersion(version, dirty, versionErr, target.expectedMigrationVersion); err != nil {
		return err
	}
	if closeErr != nil {
		return fmt.Errorf("关闭迁移连接: %w", closeErr)
	}
	log.Printf("[migrate] 目标=%s 验收通过 version=%d dirty=false", target.Name, version)
	return nil
}

func rejectDirtyOrNewer(reader migrationVersionReader, expected uint) error {
	version, dirty, err := reader.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("迁移前读取 schema_migrations: %w", err)
	}
	if dirty {
		return fmt.Errorf("迁移前验收失败: version=%d dirty=true，禁止自动 force", version)
	}
	if version > expected {
		return fmt.Errorf("数据库 version=%d 高于迁移镜像 version=%d，禁止旧镜像继续发布", version, expected)
	}
	return nil
}

func validateFinalVersion(version uint, dirty bool, versionErr error, expected uint) error {
	if versionErr != nil {
		return fmt.Errorf("迁移后读取 schema_migrations: %w", versionErr)
	}
	if dirty {
		return fmt.Errorf("迁移后验收失败: version=%d dirty=true", version)
	}
	if version != expected {
		return fmt.Errorf("迁移后验收失败: version=%d，镜像期望=%d", version, expected)
	}
	return nil
}

func closeMigration(m *migrate.Migrate) error {
	sourceErr, databaseErr := m.Close()
	return errors.Join(sourceErr, databaseErr)
}

func runTargetWithDeadline(targetsFile, environment string, expectedTargets []targetDescriptor, target migrationTarget) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位迁移器可执行文件: %w", err)
	}
	timeout := time.Duration(target.TimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable,
		"-targets-file", targetsFile,
		"-expected-targets", formatExpectedTargetInventory(expectedTargets),
		"-environment", environment,
		"-bootstrap=false",
		"-worker-target", target.Name,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("目标 %s 超过硬时限 %s，worker 已终止，发布必须阻断", target.Name, timeout)
		}
		return fmt.Errorf("目标 %s worker 失败: %w", target.Name, err)
	}
	return nil
}

func statementTimeout(target migrationTarget) time.Duration {
	timeout := time.Duration(target.TimeoutSeconds) * time.Second
	// 给驱动取消、写入 dirty/version 状态和关闭连接留出确定窗口，避免 SQL 超时与父进程
	// 硬杀发生在同一瞬间。
	if timeout > workerShutdownGracePeriod {
		return timeout - workerShutdownGracePeriod
	}
	return time.Second
}

func advisoryLockTimeout(target migrationTarget) time.Duration {
	configured := time.Duration(target.LockWaitTimeoutSeconds) * time.Second
	minimum := mysqlAdvisoryLockWait + advisoryLockWrapperGrace
	if configured < minimum {
		return minimum
	}
	return configured
}

// bootstrapTargets 只供显式 local/dev 使用。管理员 DSN 与迁移 DSN 分文件，且管理员
// DSN 必须指向同一实例并且不能带 database，防止误连其它生产实例。
func bootstrapTargets(targets []migrationTarget) error {
	for _, target := range targets {
		migrationCfg, err := readAndHardenDSN(target.DSNFile, target, false)
		if err != nil {
			return fmt.Errorf("目标 %s 读取迁移 DSN: %w", target.Name, err)
		}
		if !mysqlAccountPattern.MatchString(migrationCfg.User) {
			return fmt.Errorf("目标 %s 的迁移账号名 %q 不满足 bootstrap 安全字符约束", target.Name, migrationCfg.User)
		}
		adminCfg, err := readAdminDSN(target.BootstrapAdminDSNFile)
		if err != nil {
			return fmt.Errorf("目标 %s 读取管理员 DSN: %w", target.Name, err)
		}
		if adminCfg.Net != migrationCfg.Net || adminCfg.Addr != migrationCfg.Addr {
			return fmt.Errorf("目标 %s 的管理员 DSN 与迁移 DSN 不在同一实例", target.Name)
		}
		if err := bootstrapTarget(adminCfg, target.Database, migrationCfg.User); err != nil {
			return fmt.Errorf("目标 %s: %w", target.Name, err)
		}
		log.Printf("[migrate] dev bootstrap 目标=%s database=%s 完成", target.Name, target.Database)
	}
	return nil
}

func readAdminDSN(path string) (*mysql.Config, error) {
	data, err := readBoundedFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := mysql.ParseDSN(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("管理员 DSN 格式错误: %w", err)
	}
	if cfg.User == "" {
		return nil, errors.New("管理员 DSN 缺少账号")
	}
	if cfg.DBName != "" {
		return nil, errors.New("管理员 DSN 不得指定 database")
	}
	cfg.MultiStatements = false
	cfg.Timeout = connectionTimeout
	cfg.ReadTimeout = 30 * time.Second
	cfg.WriteTimeout = 30 * time.Second
	return cfg, nil
}

func bootstrapTarget(adminCfg *mysql.Config, database, migrationUser string) error {
	db, err := sql.Open("mysql", adminCfg.FormatDSN())
	if err != nil {
		return fmt.Errorf("打开管理员连接: %w", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("管理员连接预检: %w", err)
	}
	createStatement := fmt.Sprintf(
		"CREATE DATABASE IF NOT EXISTS `%s` DEFAULT CHARACTER SET utf8mb4 DEFAULT COLLATE utf8mb4_0900_ai_ci",
		database,
	)
	if _, err := db.ExecContext(ctx, createStatement); err != nil {
		return fmt.Errorf("CREATE DATABASE: %w", err)
	}
	// 不创建账号、不设置密码，只给 dev 中已存在的迁移账号授予 Up 所需权限。
	grantStatement := fmt.Sprintf(
		"GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER, INDEX, REFERENCES ON `%s`.* TO '%s'@'%%'",
		database, migrationUser,
	)
	if _, err := db.ExecContext(ctx, grantStatement); err != nil {
		return fmt.Errorf("GRANT 迁移最小权限: %w", err)
	}
	return nil
}

func findTarget(targets []migrationTarget, name string) (migrationTarget, bool) {
	for _, target := range targets {
		if target.Name == name {
			return target, true
		}
	}
	return migrationTarget{}, false
}

func resolveSecretPath(base, path string) (string, error) {
	path = strings.TrimSpace(path)
	resolved := path
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(base, resolved)
	}
	resolved = filepath.Clean(resolved)
	relative, err := filepath.Rel(filepath.Clean(base), resolved)
	if err != nil {
		return "", fmt.Errorf("解析路径: %w", err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", errors.New("凭据文件必须位于目标清单所在目录或其子目录")
	}
	return resolved, nil
}

func readBoundedFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximumSecretFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maximumSecretFileBytes {
		return nil, fmt.Errorf("文件超过 %d 字节上限", maximumSecretFileBytes)
	}
	return data, nil
}

func isDevelopmentEnvironment(environment string) bool {
	switch strings.ToLower(strings.TrimSpace(environment)) {
	case "local", "dev", "development":
		return true
	default:
		return false
	}
}

func strictEnvBool(key string, fallback bool) (bool, error) {
	value, exists := os.LookupEnv(key)
	if !exists || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("环境变量 %s 必须是 true/false: %w", key, err)
	}
	return parsed, nil
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
