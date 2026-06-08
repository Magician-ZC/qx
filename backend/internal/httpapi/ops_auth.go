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
