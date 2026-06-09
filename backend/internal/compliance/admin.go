package compliance

// 文件说明：合规状态的 GM 客户管理只读视图——把内部 record 暴露成「某账户的实名/防沉迷状态」公共结构，
// 供司命台「客户管理」面板展示。纯只读，不改任何合规位（核验/置位仍走既有 VerifyRealname/SetBirthDate）。

import "context"

// Status 是某账户的合规状态公共视图（实名 / 未成年模式 / 生日 / 今日已玩时长）。
type Status struct {
	AccountID        string `json:"account_id"`
	BirthDate        string `json:"birth_date"`
	RealnameVerified bool   `json:"realname_verified"`
	MinorMode        bool   `json:"minor_mode"`
	DayBucket        string `json:"day_bucket"`
	DailyPlaySeconds int64  `json:"daily_play_seconds"`
}

// GetStatus 读某账户的合规状态（无登记行时返回零值状态、非错误——视为未实名/未登记）。
func (s *Service) GetStatus(ctx context.Context, accountID string) (Status, error) {
	if s == nil || s.db == nil {
		return Status{}, nil
	}
	rec, err := s.load(ctx, accountID)
	if err != nil {
		return Status{}, err
	}
	return Status{
		AccountID:        accountID,
		BirthDate:        rec.birthDate,
		RealnameVerified: rec.realnameVerified,
		MinorMode:        rec.minorMode,
		DayBucket:        rec.dayBucket,
		DailyPlaySeconds: rec.dailyPlaySeconds,
	}, nil
}
