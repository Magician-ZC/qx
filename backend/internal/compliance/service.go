package compliance

// 文件说明：合规前置门服务（PRD §5/§9：年龄分级 / 未成年模式 / 实名 / 防沉迷）。
// 这是「可致项目归零」的前置门——出海/版号双闸门均要求实名 + 未成年宵禁 + 防沉迷时长。
// 全功能受进程级 flag QUNXIANG_COMPLIANCE_ENABLED 控制，默认关 → Gate 恒放行，对默认链路零行为变化。
// 年龄/宵禁/日切计算尽量做成纯函数并允许注入时钟，以便确定性测试（不依赖 time.Now / 全局 rand）。
//
// 数据落在独立表 account_compliance（不碰 account 包 / 不改 accounts_users，仅以 account_id 关联）：
//   account_compliance(account_id PK, birth_date, realname_verified, minor_mode,
//                       day_bucket, daily_play_seconds, updated_at)
// 双驱动 SQLite/MySQL：经 dbdialect 分支 upsert，统一 ? 占位（modernc sqlite 与 go-sql-driver/mysql 均接受 ?）。

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"time"

	"qunxiang/backend/internal/storage/dbdialect"
)

const (
	// MinorDailyPlayCap 未成年单日累计游戏时长上限（秒）= 90 分钟。
	MinorDailyPlayCap int64 = 5400
	// CurfewStartHour 未成年宵禁开始小时（含），22:00 起。
	CurfewStartHour = 22
	// CurfewEndHour 未成年宵禁结束小时（不含），08:00 止。即 [22:00,24:00) ∪ [00:00,08:00) 为宵禁。
	CurfewEndHour = 8
	// AdultAge 成年年龄阈值（含）；满 18 岁视为成年。
	AdultAge = 18
)

// 拦截原因文案（稳定字符串，前端/遥测可据此分流）。
const (
	ReasonNeedRealname = "需实名"
	ReasonCurfew       = "未成年宵禁时段"
	ReasonPlayCapHit   = "防沉迷时长达上限"
)

// flagEnabled 判断进程级 flag 是否开启（true/1/yes/on，大小写不敏感）。
func flagEnabled(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// ComplianceEnabled 报告合规门是否启用（供调用方早返回判断，亦便于测试）。
func ComplianceEnabled() bool {
	return flagEnabled("QUNXIANG_COMPLIANCE_ENABLED")
}

// Verdict 是一次合规裁决结果。
type Verdict struct {
	Allowed   bool   // 是否允许继续游玩
	MinorMode bool   // 是否处于未成年模式（关闭恋爱生育 / 降暴力，由上层据此分级）
	Reason    string // 不允许时的稳定原因文案；允许时为空
}

// record 是 account_compliance 表的一行（内部读写用）。
type record struct {
	accountID        string
	birthDate        string // YYYY-MM-DD；空串表示未登记生日
	realnameVerified bool
	minorMode        bool
	dayBucket        string // YYYY-MM-DD，防沉迷计时日切桶
	dailyPlaySeconds int64
}

// Service 是合规前置门服务，持有 DB 连接、可注入时钟与实名核验器。
type Service struct {
	db       *sql.DB
	now      func() time.Time // 可注入时钟，默认 time.Now；确定性测试可替换
	realname RealnameVerifier // 实名核验网关；默认 stub（恒过，向后兼容），生产经 env 注入真实 HTTP 网关
}

// NewService 构造合规服务，使用真实时钟。
// 默认实名核验器：若 env 配了 QUNXIANG_REALNAME_ENDPOINT 则用真实 HTTP 网关，否则回退 stub（零行为变化）。
func NewService(db *sql.DB) *Service {
	svc := &Service{db: db, now: time.Now, realname: stubRealnameVerifier{}}
	if v := NewHTTPRealnameVerifierFromEnv(); v != nil {
		svc.realname = v
	}
	return svc
}

// WithRealnameVerifier 返回一个使用注入实名核验器的浅拷贝（用于注入真实 HTTP 网关或测试 mock）。
// 不修改原 Service；verifier 为 nil 时保持原核验器不变。
func (s *Service) WithRealnameVerifier(verifier RealnameVerifier) *Service {
	if s == nil {
		return nil
	}
	clone := *s
	if verifier != nil {
		clone.realname = verifier
	}
	return &clone
}

// WithClock 返回一个使用注入时钟的浅拷贝（用于确定性测试宵禁/日切）。
// 不修改原 Service，便于并发安全地派生测试实例。
func (s *Service) WithClock(now func() time.Time) *Service {
	if s == nil {
		return nil
	}
	clone := *s
	if now != nil {
		clone.now = now
	}
	return &clone
}

// clock 返回当前时间（带 nil 兜底）。
func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// dayBucketOf 返回某时刻所属的日切桶（按本地日期 YYYY-MM-DD）——纯函数。
func dayBucketOf(t time.Time) string {
	return t.Format("2006-01-02")
}

// ageAt 计算某出生日期在参考时刻的周岁年龄——纯函数。
// birthDate 解析失败（含空串）返回 (-1, false)；调用方据此把未知生日按「未登记」处理。
func ageAt(birthDate string, at time.Time) (int, bool) {
	bd, err := time.Parse("2006-01-02", strings.TrimSpace(birthDate))
	if err != nil {
		return -1, false
	}
	years := at.Year() - bd.Year()
	// 若今年生日还没到（月/日更晚），减一岁。
	if at.Month() < bd.Month() || (at.Month() == bd.Month() && at.Day() < bd.Day()) {
		years--
	}
	if years < 0 {
		years = 0
	}
	return years, true
}

// isMinor 判断在参考时刻是否未成年（生日未知按「非未成年」处理，由实名门兜底拦截）——纯函数。
func isMinor(birthDate string, at time.Time) bool {
	age, ok := ageAt(birthDate, at)
	if !ok {
		return false
	}
	return age < AdultAge
}

// inCurfew 判断某时刻是否落在未成年宵禁时段 [22:00,08:00)——纯函数。
func inCurfew(at time.Time) bool {
	h := at.Hour()
	return h >= CurfewStartHour || h < CurfewEndHour
}

// Gate 是合规前置门：读 account_compliance 表，按实名 / 未成年宵禁 / 防沉迷时长裁决。
// flag 关时恒放行（Allowed=true, MinorMode=false），对默认链路零影响。
func (s *Service) Gate(ctx context.Context, accountID string) (Verdict, error) {
	if !ComplianceEnabled() {
		return Verdict{Allowed: true}, nil
	}
	rec, err := s.load(ctx, accountID)
	if err != nil {
		return Verdict{}, err
	}
	now := s.clock()

	// 1) 未实名 → 直接拦截（最高优先级，出海/版号硬门）。
	if !rec.realnameVerified {
		return Verdict{Allowed: false, MinorMode: false, Reason: ReasonNeedRealname}, nil
	}

	minor := isMinor(rec.birthDate, now) || rec.minorMode
	if !minor {
		// 成年人：放行，无未成年模式。
		return Verdict{Allowed: true, MinorMode: false}, nil
	}

	// 2) 未成年宵禁时段 [22:00,08:00) → 拦截。
	if inCurfew(now) {
		return Verdict{Allowed: false, MinorMode: true, Reason: ReasonCurfew}, nil
	}

	// 3) 防沉迷：当日累计时长达上限 → 拦截（按当日桶计，跨天自动归零）。
	today := dayBucketOf(now)
	played := rec.dailyPlaySeconds
	if rec.dayBucket != today {
		played = 0 // 跨天重置，旧桶不计入今日
	}
	if played >= MinorDailyPlayCap {
		return Verdict{Allowed: false, MinorMode: true, Reason: ReasonPlayCapHit}, nil
	}

	// 未成年但未触线：放行，置未成年模式（上层据此关闭恋爱生育 / 降暴力）。
	return Verdict{Allowed: true, MinorMode: true}, nil
}

// RecordPlaySeconds 累加某账号当日游戏时长（防沉迷计时）。
// flag 关时 no-op。按 day_bucket 日切：当前桶等于今日则累加，否则重置为本次时长。
func (s *Service) RecordPlaySeconds(ctx context.Context, accountID string, seconds int64) error {
	if !ComplianceEnabled() {
		return nil
	}
	if seconds < 0 {
		seconds = 0
	}
	rec, err := s.load(ctx, accountID)
	if err != nil {
		return err
	}
	now := s.clock()
	today := dayBucketOf(now)
	total := seconds
	if rec.dayBucket == today {
		total = rec.dailyPlaySeconds + seconds
	}
	rec.dayBucket = today
	rec.dailyPlaySeconds = total
	return s.save(ctx, rec, now)
}

// VerifyRealname 登记/更新账号实名状态（Verify 类方法，PRD 强制实名前置）。
//
// 历史/向后兼容路径：直接落客户端传入的 bool——这是「客户端自报即过」语义，**不做真实核验**。
// 保留此签名仅为整合方/既有路由不被破坏；新接入应改用 VerifyRealnameWithIdentity 走真实核验网关。
func (s *Service) VerifyRealname(ctx context.Context, accountID string, verified bool) error {
	rec, err := s.load(ctx, accountID)
	if err != nil {
		return err
	}
	rec.realnameVerified = verified
	return s.save(ctx, rec, s.clock())
}

// VerifyRealnameWithIdentity 是真实实名核验路径（PRD §5/§9 强制实名前置门）。
// 把「真实姓名 + 身份证号」交给 RealnameVerifier 核验，仅在核验通过（matched=true）时
// 把 realname_verified 置 1 落库；核验不过或网关报错则返回错误、不置位。
//
// PII 安全：姓名/身份证号仅用于核验，**绝不落 account_compliance**（只落结果位 realname_verified
// + 可选脱敏 ref）；本方法不打印姓名/身份证号到任何日志。
//
// 默认核验器为 stub（恒过，向后兼容）；生产须经 env（QUNXIANG_REALNAME_ENDPOINT）或
// WithRealnameVerifier 注入真实 HTTP 网关，否则实名门形同虚设。
func (s *Service) VerifyRealnameWithIdentity(ctx context.Context, accountID, name, idNumber string) error {
	verifier := s.realname
	if verifier == nil {
		verifier = stubRealnameVerifier{}
	}
	matched, _, err := verifier.Verify(ctx, name, idNumber)
	if err != nil {
		// 核验失败（不匹配 / 格式非法 / 网关错误）：不置位，原样上抛供调用方区分处理。
		return err
	}
	if !matched {
		return ErrRealnameMismatch
	}
	rec, err := s.load(ctx, accountID)
	if err != nil {
		return err
	}
	rec.realnameVerified = true
	// 注意：rec 中绝不写入 name/idNumber——只落结果位。
	return s.save(ctx, rec, s.clock())
}

// SetBirthDate 登记/更新账号出生日期（YYYY-MM-DD），并据此刷新未成年模式标记。
func (s *Service) SetBirthDate(ctx context.Context, accountID, birthDate string) error {
	rec, err := s.load(ctx, accountID)
	if err != nil {
		return err
	}
	rec.birthDate = strings.TrimSpace(birthDate)
	rec.minorMode = isMinor(rec.birthDate, s.clock())
	return s.save(ctx, rec, s.clock())
}

// load 读取一行 account_compliance；不存在时返回该 accountID 的零值记录（未实名、未登记生日）。
func (s *Service) load(ctx context.Context, accountID string) (record, error) {
	rec := record{accountID: accountID}
	const q = `SELECT birth_date, realname_verified, minor_mode, day_bucket, daily_play_seconds
		FROM account_compliance WHERE account_id = ?`
	var (
		birthDate sql.NullString
		realname  sql.NullInt64
		minorMode sql.NullInt64
		dayBucket sql.NullString
		playSecs  sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, q, accountID).Scan(&birthDate, &realname, &minorMode, &dayBucket, &playSecs)
	if err == sql.ErrNoRows {
		return rec, nil
	}
	if err != nil {
		return record{}, err
	}
	rec.birthDate = strings.TrimSpace(birthDate.String)
	rec.realnameVerified = realname.Int64 != 0
	rec.minorMode = minorMode.Int64 != 0
	rec.dayBucket = strings.TrimSpace(dayBucket.String)
	rec.dailyPlaySeconds = playSecs.Int64
	return rec, nil
}

// save 写入/更新一行 account_compliance（双驱动 upsert，updated_at 记当前时刻 RFC3339）。
func (s *Service) save(ctx context.Context, rec record, now time.Time) error {
	realname := boolToInt(rec.realnameVerified)
	minorMode := boolToInt(rec.minorMode)
	updatedAt := now.UTC().Format(time.RFC3339)

	var query string
	if dbdialect.IsMySQL(s.db) {
		query = `INSERT INTO account_compliance
			(account_id, birth_date, realname_verified, minor_mode, day_bucket, daily_play_seconds, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				birth_date = VALUES(birth_date),
				realname_verified = VALUES(realname_verified),
				minor_mode = VALUES(minor_mode),
				day_bucket = VALUES(day_bucket),
				daily_play_seconds = VALUES(daily_play_seconds),
				updated_at = VALUES(updated_at)`
	} else {
		query = `INSERT INTO account_compliance
			(account_id, birth_date, realname_verified, minor_mode, day_bucket, daily_play_seconds, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(account_id) DO UPDATE SET
				birth_date = excluded.birth_date,
				realname_verified = excluded.realname_verified,
				minor_mode = excluded.minor_mode,
				day_bucket = excluded.day_bucket,
				daily_play_seconds = excluded.daily_play_seconds,
				updated_at = excluded.updated_at`
	}
	_, err := s.db.ExecContext(ctx, query,
		rec.accountID, rec.birthDate, realname, minorMode, rec.dayBucket, rec.dailyPlaySeconds, updatedAt)
	return err
}

// boolToInt 把 bool 转 0/1（兼容 SQLite INTEGER / MySQL TINYINT）。
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
