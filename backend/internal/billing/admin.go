package billing

// 文件说明：billing 的 GM 客户管理管控能力——退款撤权益。把某账户某 SKU 的权益置为 revoked，
// 使其不再 active（下游权益判定/叙事 perk/配额据此失效）。供司命台「客户管理」退款入口（经 RBAC admin + 审计把关）。
// 注意：本操作只撤权益态，不触平台真实退款（平台退款走各 store 的退款流程；此处是账实对齐的权益撤销）。

import (
	"context"
	"fmt"
	"strings"
)

// RevokeEntitlement 把某账户某 SKU 的权益置为 revoked（退款/撤权益）。SKU 为空时撤该账户全部 active 权益。
// 返回受影响的权益行数。
func (s *Service) RevokeEntitlement(ctx context.Context, accountID, skuID string) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("billing revoke entitlement: nil db")
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return 0, fmt.Errorf("billing revoke entitlement: empty account id")
	}
	skuID = strings.TrimSpace(skuID)
	var res interface{ RowsAffected() (int64, error) }
	var err error
	if skuID == "" {
		res, err = s.db.ExecContext(ctx,
			`UPDATE account_entitlements SET status = 'revoked' WHERE account_id = ? AND status = 'active'`, accountID)
	} else {
		res, err = s.db.ExecContext(ctx,
			`UPDATE account_entitlements SET status = 'revoked' WHERE account_id = ? AND sku_id = ?`, accountID, skuID)
	}
	if err != nil {
		return 0, fmt.Errorf("billing revoke entitlement: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
