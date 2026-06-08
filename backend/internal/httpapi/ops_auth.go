package httpapi

// 文件说明：ops / 跨玩家写接口的 opt-in 操作者鉴权中间件。
// 设计取向：原型默认开放、向后兼容——未配置 QUNXIANG_OPS_TOKEN 时一律放行；
// 一旦配置，则敏感端点（ops 仪表盘、social-objects 写、seven-interactions、consent 处理）
// 必须携带正确的 X-Ops-Token 头，否则 403。比较用 crypto/subtle 常量时间，避免时序侧信道。

import (
	"crypto/subtle"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"

	"qunxiang/backend/internal/account"
)

// opsEnvVar 是操作者令牌的环境变量名；为空字符串视为“未配置”，中间件放行。
const opsEnvVar = "QUNXIANG_OPS_TOKEN"

// opsTokenHeader 是请求需携带的令牌头名。
const opsTokenHeader = "X-Ops-Token"

// opsTokenGuard 返回一个 gin 中间件：opt-in 的操作者鉴权。
//
//	未设置 QUNXIANG_OPS_TOKEN  → 放行（原型默认开放、向后兼容）。
//	已设置                     → 要求请求头 X-Ops-Token 等于该值，否则 403 + Abort。
//
// 比较用 crypto/subtle.ConstantTimeCompare 防时序侧信道（避免按字节早退泄露前缀匹配长度）。
func opsTokenGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		expected := os.Getenv(opsEnvVar)
		if expected == "" {
			// 未配置：原型默认开放。
			c.Next()
			return
		}
		provided := c.GetHeader(opsTokenHeader)
		if subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
			return
		}
		c.Next()
	}
}

// authedAccountID 从 Authorization Bearer token 解析出**权威**账户 ID，用于 compliance/billing 这类
// 「只能操作自己账户」的端点——一律忽略客户端请求体/路径里传入的 account_id，杜绝越权为他人伪造实名/扣费
// （评审 load-bearing 修复）。账户服务不可用 / token 缺失 / token 无效时，已写好 503/401 响应并返回 ok=false。
func authedAccountID(accounts *account.Service, c *gin.Context) (string, bool) {
	if accounts == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account service is unavailable"})
		return "", false
	}
	token := account.ExtractBearerToken(c.GetHeader("Authorization"))
	if token == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
		return "", false
	}
	user, err := accounts.CurrentUser(c.Request.Context(), token)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
		return "", false
	}
	return user.ID, true
}

// softAccountID 软解析账户 ID：用于建局/合规门控这类「有 token 则归账、无 token 则匿名放行」的端点。
// 与 authedAccountID 的区别：本函数**绝不写响应、绝不 Abort**——账户服务不可用 / token 缺失 / token 无效
// 一律静默返回空字符串（匿名），由调用方决定匿名语义（建局匿名局、合规门匿名放行）。
// 这样既能在玩家登录时贯穿 accountID（成本归账 / 合规门控），又不破坏未登录玩家的原型默认开放体验。
func softAccountID(accounts *account.Service, c *gin.Context) string {
	if accounts == nil || c == nil {
		return ""
	}
	token := account.ExtractBearerToken(c.GetHeader("Authorization"))
	if token == "" {
		return ""
	}
	user, err := accounts.CurrentUser(c.Request.Context(), token)
	if err != nil {
		return ""
	}
	return user.ID
}
