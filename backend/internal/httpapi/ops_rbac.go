package httpapi

// 文件说明：ops / GM 运营后台的角色分级（RBAC）+ 操作审计中间件。
// 在原 opsTokenGuard（单 token fail-open / fail-closed 两态）之上，提供「多操作者 + 三档角色」的
// 数据库支撑鉴权——一旦 ops_operators 表非空，即以表为权威：按 X-Ops-Token 的 sha256 命中操作者，
// 比对其角色 rank 是否够 minRole；表为空时优雅降级回旧的单 token env（admin）/原型开放语义，向后兼容。
// 角色 rank 升序：viewer(1) < operator(2) < admin(3)。审计 best-effort 落 ops_audit_log，吞错不阻断主流程。

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// OpsRole 是运营操作者的角色枚举（点分小写字符串，落库列 role）。
type OpsRole string

// 三档角色：viewer 只读、operator 可写运营动作、admin 含操作者管理 / token 轮换。
const (
	RoleViewer   OpsRole = "viewer"
	RoleOperator OpsRole = "operator"
	RoleAdmin    OpsRole = "admin"
)

// roleRank 把角色映射为可比较的整数等级（未知角色按 0，恒小于 viewer，安全保守）。
func roleRank(r OpsRole) int {
	switch r {
	case RoleViewer:
		return 1
	case RoleOperator:
		return 2
	case RoleAdmin:
		return 3
	default:
		return 0
	}
}

// OpsOperator 是一名运营操作者的最小身份（注入 gin.Context 供审计 / 下游用）。
type OpsOperator struct {
	Name string
	Role OpsRole
}

// opsOperatorContextKey 是注入 gin.Context 的操作者键名。
const opsOperatorContextKey = "ops_operator"

// opsRBACGuard 返回一个 gin 中间件：基于 ops_operators 表的角色分级鉴权。
//
// 鉴权优先级（store 非空时以表为权威，否则降级回旧的单 token env）：
//
//	① 表非空（Count>0）：以表为准——读 X-Ops-Token，sha256 命中操作者且 roleRank(role)>=roleRank(minRole)
//	   则注入 operator + 放行；命中失败 / 角色不足一律 403。
//	② 表为空（Count==0）：降级——
//	     env(opsEnvVar) 非空：单 token 视为 admin，比对相符则以 admin 身份放行（满足任何 minRole），不符 403；
//	     env 也为空：minRole<=viewer 放行（原型默认开放），minRole>=operator 返 503（fail-closed，未配鉴权拒绝高危写）。
//	③ Count 查询失败：按「表空」降级保守处理（不因 DB 抖动把已配好的运营全锁死，仍回退到 env 这一道）。
//
// 这样既向后兼容旧的单 token 部署，又让运营方建好 ops_operators 表后无缝升级到多操作者分级。
func opsRBACGuard(store *OpsOperatorStore, minRole OpsRole) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 先尝试以表为权威。区分「确认表空(count==0,无错)」与「Count 查询失败(countErr!=nil)」两态——
		// 后者不能等同表空裸放行，否则一次 DB 抖动会把已建好 RBAC 的敏感端点瞬间打穿（见下方降级分支的 fail-safe）。
		count := 0
		var countErr error
		if store != nil {
			count, countErr = store.Count(c.Request.Context())
		}
		if countErr == nil && count > 0 {
			token := c.GetHeader(opsTokenHeader)
			op, ok, err := store.Resolve(c.Request.Context(), token)
			if err != nil || !ok {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
				return
			}
			if roleRank(op.Role) < roleRank(minRole) {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
				return
			}
			c.Set(opsOperatorContextKey, op)
			c.Next()
			return
		}

		// 降级回旧的单 token env 语义（表确认为空、或 Count 查询失败时的兜底）。
		expected := opsEnvToken()
		if expected != "" {
			// 配了单 token：相符即以 admin 身份放行（满足任何 minRole），否则 403。
			// 注意：此路径在 Count 失败时仍可用——已配单 token 的部署不应因 DB 抖动被锁死。
			provided := c.GetHeader(opsTokenHeader)
			if constantTimeEqual(provided, expected) {
				c.Set(opsOperatorContextKey, OpsOperator{Name: "env-admin", Role: RoleAdmin})
				c.Next()
				return
			}
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
			return
		}

		// env 也为空：仅当**确认表空**(countErr==nil && count==0)时才按原型默认开放 viewer 档；
		// Count 查询失败时无法确认表里是否已配 operator → fail-safe 拒绝（503），绝不因 DB 抖动对敏感读裸放行。
		// operator 及以上恒 fail-closed 拒绝。
		if countErr == nil && roleRank(minRole) <= roleRank(RoleViewer) {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "ops auth not configured"})
	}
}

// currentOperator 取注入 gin.Context 的操作者（opsRBACGuard 放行时写入）。未命中（如降级 viewer 路径未注入）返回 false。
func currentOperator(c *gin.Context) (OpsOperator, bool) {
	if c == nil {
		return OpsOperator{}, false
	}
	v, ok := c.Get(opsOperatorContextKey)
	if !ok {
		return OpsOperator{}, false
	}
	op, ok := v.(OpsOperator)
	return op, ok
}

// auditOps best-effort 写一条运营操作审计（吞错不阻断主流程；store 为空 / 写失败均静默）。
// operator/role 取自注入的 currentOperator（缺省 "anonymous"/"viewer"，对应降级 viewer 路径）。
func auditOps(store *OpsOperatorStore, c *gin.Context, action, target string) {
	if store == nil || c == nil {
		return
	}
	operator := "anonymous"
	role := string(RoleViewer)
	if op, ok := currentOperator(c); ok {
		if op.Name != "" {
			operator = op.Name
		}
		if op.Role != "" {
			role = string(op.Role)
		}
	}
	// 用请求上下文；吞错（审计不可阻断运营动作本身）。
	_ = store.AppendAudit(c.Request.Context(), operator, role, action, target)
}
