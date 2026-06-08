package compliance

// 文件说明：合规前置门聚焦测试——起临时内存 SQLite，断言 flag 关恒放行、
// 未实名 / 未成年宵禁 / 防沉迷各被拦、成年放行、跨天重置。
// 时间全部经 WithClock 注入，宵禁/日切计算确定性可复算，不依赖 time.Now。
// 测试自建 account_compliance 表（schema 由独立 schema agent 落地，测试不依赖其文件）。

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"qunxiang/backend/internal/storage/dbdialect"
)

// complianceTestDB 起内存 SQLite 并建 account_compliance 表（SQLite 形态，与 SPEC 一致）。
func complianceTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("打开内存 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	dbdialect.Register(db, dbdialect.DialectSQLite)

	const ddl = `CREATE TABLE IF NOT EXISTS account_compliance (
		account_id TEXT PRIMARY KEY,
		birth_date TEXT DEFAULT '',
		realname_verified INTEGER DEFAULT 0,
		minor_mode INTEGER DEFAULT 0,
		day_bucket TEXT DEFAULT '',
		daily_play_seconds INTEGER DEFAULT 0,
		updated_at TEXT DEFAULT ''
	)`
	if _, err := db.ExecContext(context.Background(), ddl); err != nil {
		t.Fatalf("建 account_compliance 表失败: %v", err)
	}
	return db
}

// fixedClock 返回一个固定时刻的时钟。
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// at 构造一个 UTC 时刻（年月日时分）。
func at(year int, month time.Month, day, hour, min int) time.Time {
	return time.Date(year, month, day, hour, min, 0, 0, time.UTC)
}

func TestComplianceFlagOffAlwaysAllows(t *testing.T) {
	// flag 关（不设置环境变量即关）：即便账号不存在 / 未实名，也恒放行、无未成年模式。
	t.Setenv("QUNXIANG_COMPLIANCE_ENABLED", "")
	db := complianceTestDB(t)
	svc := NewService(db).WithClock(fixedClock(at(2026, time.June, 8, 23, 0))) // 宵禁时段
	ctx := context.Background()

	v, err := svc.Gate(ctx, "acc-unknown")
	if err != nil {
		t.Fatalf("Gate 报错: %v", err)
	}
	if !v.Allowed || v.MinorMode || v.Reason != "" {
		t.Fatalf("flag 关应恒放行无未成年模式，得到 %+v", v)
	}

	// RecordPlaySeconds 在 flag 关时为 no-op，不应建行。
	if err := svc.RecordPlaySeconds(ctx, "acc-unknown", 9999); err != nil {
		t.Fatalf("RecordPlaySeconds no-op 报错: %v", err)
	}
	var n int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_compliance`).Scan(&n)
	if n != 0 {
		t.Fatalf("flag 关时 RecordPlaySeconds 不应写表，count=%d", n)
	}
}

func TestComplianceBlocksUnverifiedRealname(t *testing.T) {
	t.Setenv("QUNXIANG_COMPLIANCE_ENABLED", "1")
	db := complianceTestDB(t)
	// 白天非宵禁，避免与宵禁混淆。
	svc := NewService(db).WithClock(fixedClock(at(2026, time.June, 8, 12, 0)))
	ctx := context.Background()

	// 未知账号（无行）→ 未实名拦截。
	v, err := svc.Gate(ctx, "acc-new")
	if err != nil {
		t.Fatalf("Gate 报错: %v", err)
	}
	if v.Allowed || v.Reason != ReasonNeedRealname {
		t.Fatalf("未实名应拦截且 Reason=%q，得到 %+v", ReasonNeedRealname, v)
	}
}

func TestComplianceMinorCurfewBlocks(t *testing.T) {
	t.Setenv("QUNXIANG_COMPLIANCE_ENABLED", "1")
	db := complianceTestDB(t)
	ctx := context.Background()

	// 未成年（出生 2012 → 2026 年 14 岁），已实名。
	setup := NewService(db).WithClock(fixedClock(at(2026, time.June, 8, 12, 0)))
	if err := setup.SetBirthDate(ctx, "kid", "2012-01-01"); err != nil {
		t.Fatalf("SetBirthDate 报错: %v", err)
	}
	if err := setup.VerifyRealname(ctx, "kid", true); err != nil {
		t.Fatalf("VerifyRealname 报错: %v", err)
	}

	// 宵禁时段 23:00 → 拦截，且置未成年模式。
	curfew := NewService(db).WithClock(fixedClock(at(2026, time.June, 8, 23, 0)))
	v, err := curfew.Gate(ctx, "kid")
	if err != nil {
		t.Fatalf("Gate 报错: %v", err)
	}
	if v.Allowed || !v.MinorMode || v.Reason != ReasonCurfew {
		t.Fatalf("未成年宵禁应拦截+未成年模式，得到 %+v", v)
	}

	// 凌晨 07:00 仍宵禁（[22:00,08:00)）。
	earlyMorning := NewService(db).WithClock(fixedClock(at(2026, time.June, 8, 7, 0)))
	v2, _ := earlyMorning.Gate(ctx, "kid")
	if v2.Allowed || v2.Reason != ReasonCurfew {
		t.Fatalf("07:00 仍应宵禁拦截，得到 %+v", v2)
	}

	// 非宵禁 12:00 且未触防沉迷线 → 放行但仍未成年模式。
	daytime := NewService(db).WithClock(fixedClock(at(2026, time.June, 8, 12, 0)))
	v3, _ := daytime.Gate(ctx, "kid")
	if !v3.Allowed || !v3.MinorMode || v3.Reason != "" {
		t.Fatalf("白天非宵禁应放行但置未成年模式，得到 %+v", v3)
	}
}

func TestComplianceMinorPlayCapBlocks(t *testing.T) {
	t.Setenv("QUNXIANG_COMPLIANCE_ENABLED", "1")
	db := complianceTestDB(t)
	ctx := context.Background()

	noon := fixedClock(at(2026, time.June, 8, 12, 0))
	setup := NewService(db).WithClock(noon)
	if err := setup.SetBirthDate(ctx, "kid", "2012-01-01"); err != nil {
		t.Fatalf("SetBirthDate 报错: %v", err)
	}
	if err := setup.VerifyRealname(ctx, "kid", true); err != nil {
		t.Fatalf("VerifyRealname 报错: %v", err)
	}

	svc := NewService(db).WithClock(noon)
	// 累计到上限（5400s）。
	if err := svc.RecordPlaySeconds(ctx, "kid", MinorDailyPlayCap); err != nil {
		t.Fatalf("RecordPlaySeconds 报错: %v", err)
	}
	v, err := svc.Gate(ctx, "kid")
	if err != nil {
		t.Fatalf("Gate 报错: %v", err)
	}
	if v.Allowed || v.Reason != ReasonPlayCapHit {
		t.Fatalf("达防沉迷上限应拦截，得到 %+v", v)
	}
}

func TestCompliancePlayCapDayRollover(t *testing.T) {
	t.Setenv("QUNXIANG_COMPLIANCE_ENABLED", "1")
	db := complianceTestDB(t)
	ctx := context.Background()

	day1 := fixedClock(at(2026, time.June, 8, 12, 0))
	setup := NewService(db).WithClock(day1)
	if err := setup.SetBirthDate(ctx, "kid", "2012-01-01"); err != nil {
		t.Fatalf("SetBirthDate 报错: %v", err)
	}
	if err := setup.VerifyRealname(ctx, "kid", true); err != nil {
		t.Fatalf("VerifyRealname 报错: %v", err)
	}

	// 第一天打满上限 → 被拦。
	svc1 := NewService(db).WithClock(day1)
	if err := svc1.RecordPlaySeconds(ctx, "kid", MinorDailyPlayCap); err != nil {
		t.Fatalf("第一天计时报错: %v", err)
	}
	if v, _ := svc1.Gate(ctx, "kid"); v.Allowed {
		t.Fatalf("第一天达上限应被拦")
	}

	// 第二天同一时刻：跨天重置，未再累计 → 放行（未成年模式仍在）。
	day2 := fixedClock(at(2026, time.June, 9, 12, 0))
	svc2 := NewService(db).WithClock(day2)
	v, err := svc2.Gate(ctx, "kid")
	if err != nil {
		t.Fatalf("Gate 报错: %v", err)
	}
	if !v.Allowed || !v.MinorMode {
		t.Fatalf("跨天应重置防沉迷并放行，得到 %+v", v)
	}

	// 第二天累计应从 0 起：再打 100s 后，库里当日时长应为 100 而非叠加。
	if err := svc2.RecordPlaySeconds(ctx, "kid", 100); err != nil {
		t.Fatalf("第二天计时报错: %v", err)
	}
	var (
		bucket string
		secs   int64
	)
	if err := db.QueryRowContext(ctx,
		`SELECT day_bucket, daily_play_seconds FROM account_compliance WHERE account_id = ?`, "kid").
		Scan(&bucket, &secs); err != nil {
		t.Fatalf("读回报错: %v", err)
	}
	if bucket != "2026-06-09" || secs != 100 {
		t.Fatalf("跨天应日切重置，得到 bucket=%q secs=%d", bucket, secs)
	}
}

func TestComplianceAdultAllowedAlways(t *testing.T) {
	t.Setenv("QUNXIANG_COMPLIANCE_ENABLED", "1")
	db := complianceTestDB(t)
	ctx := context.Background()

	noon := fixedClock(at(2026, time.June, 8, 12, 0))
	setup := NewService(db).WithClock(noon)
	// 成年（2000 出生 → 26 岁），已实名。
	if err := setup.SetBirthDate(ctx, "adult", "2000-05-01"); err != nil {
		t.Fatalf("SetBirthDate 报错: %v", err)
	}
	if err := setup.VerifyRealname(ctx, "adult", true); err != nil {
		t.Fatalf("VerifyRealname 报错: %v", err)
	}

	// 即便在宵禁时段、即便打了很多时长，成年都放行、无未成年模式。
	curfew := NewService(db).WithClock(fixedClock(at(2026, time.June, 8, 23, 0)))
	if err := curfew.RecordPlaySeconds(ctx, "adult", MinorDailyPlayCap*10); err != nil {
		t.Fatalf("RecordPlaySeconds 报错: %v", err)
	}
	v, err := curfew.Gate(ctx, "adult")
	if err != nil {
		t.Fatalf("Gate 报错: %v", err)
	}
	if !v.Allowed || v.MinorMode || v.Reason != "" {
		t.Fatalf("成年应恒放行无未成年模式，得到 %+v", v)
	}
}

func TestComplianceAgeBoundary(t *testing.T) {
	// 边界：生日当天满 18 视为成年；差一天为未成年。
	ref := at(2026, time.June, 8, 12, 0)
	if isMinor("2008-06-08", ref) {
		t.Fatalf("2008-06-08 在 2026-06-08 应满 18 岁成年")
	}
	if !isMinor("2008-06-09", ref) {
		t.Fatalf("2008-06-09 在 2026-06-08 应未满 18 岁未成年")
	}
	// 生日未知按非未成年处理（由实名门兜底）。
	if isMinor("", ref) {
		t.Fatalf("生日未知不应判定未成年")
	}
	// 宵禁边界：22:00 入、08:00 出。
	if !inCurfew(at(2026, time.June, 8, 22, 0)) {
		t.Fatalf("22:00 应在宵禁内")
	}
	if inCurfew(at(2026, time.June, 8, 8, 0)) {
		t.Fatalf("08:00 应已出宵禁")
	}
	if inCurfew(at(2026, time.June, 8, 21, 59)) {
		t.Fatalf("21:59 应未进宵禁")
	}
}
