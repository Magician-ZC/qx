package httpapi

// 文件说明：开发/演示用「demo 测试角色」一键播种（flag QUNXIANG_SEED_DEMO 开启时启动期幂等执行）。
// 建一个 demo 账号 + 给它降生一个主世界角色——降生本身自动织 20 人村庄，故 demo 登录后地图上即有
// 主角 + 20 个有名有姓的村民 = 21 个可见角色，供在全屏世界地图上观察。
// 幂等：账号已存在则登录取 id；CreateMainWorldCharacter 对同账号 resume 同一角色（不重复造人）。best-effort 吞错不阻断启动。

import (
	"context"
	"log/slog"

	"qunxiang/backend/internal/account"
	"qunxiang/backend/internal/session"
)

// demoSeedCredentials 是 demo 账号登录凭据（运营/开发自知；仅 QUNXIANG_SEED_DEMO 开时存在）。
const (
	demoSeedUsername = "demo"
	demoSeedPassword = "demo1234"
)

// seedDemoCharacter 幂等播种 demo 账号 + 主世界角色（自动 20 人村庄）。logger 可空。
func seedDemoCharacter(ctx context.Context, accounts *account.Service, newSvc func() *session.Service, logger *slog.Logger) {
	if accounts == nil || newSvc == nil {
		return
	}
	logf := func(msg string, args ...any) {
		if logger != nil {
			logger.Info(msg, args...)
		}
	}
	// 1) 注册 demo 账号；已存在则登录取 id。
	user, err := accounts.Register(ctx, demoSeedUsername, "Demo·阿宁", demoSeedPassword)
	if err != nil {
		login, lerr := accounts.Login(ctx, demoSeedUsername, demoSeedPassword)
		if lerr != nil {
			if logger != nil {
				logger.Error("seed demo: register and login both failed", "register_err", err, "login_err", lerr)
			}
			return
		}
		user = login.User
	}
	// 2) 给 demo 降生一个主世界角色（幂等 resume；自动织 20 人村庄铺到地图上）。
	character, err := newSvc().CreateMainWorldCharacter(ctx, user.ID, session.MainWorldCharacterInput{
		Name:    "阿宁",
		Origin:  "山村药农之女",
		Desire:  "想看看山外面的世界",
		Wound:   "幼年失怙，由祖母带大",
		Redline: "绝不抛下身边的人独活",
		Faction: "freedom",
	})
	if err != nil {
		if logger != nil {
			logger.Error("seed demo: create character failed", "error", err)
		}
		return
	}
	logf("seeded demo character", "account", demoSeedUsername, "password", demoSeedPassword, "session", character.SessionID, "unit", character.UnitID)
}
