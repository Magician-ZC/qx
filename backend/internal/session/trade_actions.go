package session

// 文件说明：玩家直驱 NPC 交易（开发计划 2026-06-10 A13）——她与同格/相邻 NPC 买卖物品，也是行商 POI 的结算出口。
// 决策=玩家点击即玩家意志（零 LLM，不走自治交易的 LLM consent——玩家亲自拍板即双方意思表示的玩家侧）；
// 结算=镜像 unit.TradeService.PurchaseItem 的金币/物品写法（钱包是非受保护字段、照其现有直改写法；
//   物品经 ConsumeBackpackItem/AddBackpackItem，含派生攻防重算）。**不直接调 PurchaseItem 的原因**（取舍）：
//   ① 其 loadTradePair 强制 HexDistance==1，同格（distance 0）交易会被拒——玩家直驱允许同格/相邻；
//   ② 其 TakeItem 按「整堆」转移且只收一份价钱，数量语义对玩家直驱是漏洞（一份钱买走整堆口粮）。
//   故按数量逐件结算：ConsumeBackpackItem(卖方, qty) + AddBackpackItem(买方, qty) + 钱包按 单价×数量 调拨。
// 落痕=appendLog + ReasonPlayerTileAction 流程事件（recordPlayerActionTrace，含 Save）+ best-effort WS；
// 关系=复用 trade.go 的 applyTradeRelationShiftBestEffort 与 bestEffortLitTradeDebtAnchor（成交即结一根人情锚）。

import (
	"context"
	"fmt"
	"strings"

	"qunxiang/backend/internal/item"
	"qunxiang/backend/internal/unit"
)

// playerTradeMaxQuantity 是单次直驱交易的数量上限（防一次清空对方货底/钱包的极端操作）。
const playerTradeMaxQuantity = 5

// PlayerTradeRequest 是玩家直驱交易的请求体。
type PlayerTradeRequest struct {
	TargetUnitID string `json:"target_unit_id"`
	Mode         string `json:"mode"`     // buy=她向对方买入 / sell=她把东西卖给对方
	ItemID       string `json:"item_id"`
	Quantity     int    `json:"quantity"` // 缺省按 1，上限 playerTradeMaxQuantity
}

// PlayerTradeItem 是交易后她背包里的一件物品概要（供前端即时刷新行囊）。
type PlayerTradeItem struct {
	ItemID      string `json:"item_id"`
	DisplayName string `json:"display_name"`
	Quantity    int    `json:"quantity"`
}

// PlayerTradeResult 是一次玩家直驱交易的返回。
type PlayerTradeResult struct {
	OK          bool              `json:"ok"`
	SummaryZH   string            `json:"summary_zh"`
	WalletAfter int               `json:"wallet_after"` // 交易后她的钱包
	Items       []PlayerTradeItem `json:"items"`        // 交易后她的背包概要
}

// merchantSellPrice 是她把东西卖出的单价口径：目录基准价八成（与 trade.go purchasePriceOptions 的低档报价同源），至少 1 金。
func merchantSellPrice(basePrice int) int {
	price := basePrice * 8 / 10
	if price < 1 {
		price = 1
	}
	return price
}

// backpackStackSoulBound 判断某单位背包里该物品的堆栈是否灵魂绑定（SoulBound 连玩家手动交易也禁止，见 unit/profile.go）。
func backpackStackSoulBound(record unit.Record, itemID string) bool {
	for _, stack := range record.Inventory.Backpack {
		if stack.ItemID == itemID && stack.SoulBound {
			return true
		}
	}
	return false
}

// backpackSummary 把单位背包整理成交易返回的概要列表。
func backpackSummary(record unit.Record) []PlayerTradeItem {
	items := make([]PlayerTradeItem, 0, len(record.Inventory.Backpack))
	for _, stack := range record.Inventory.Backpack {
		if stack.ItemID == "" || stack.Quantity <= 0 {
			continue
		}
		items = append(items, PlayerTradeItem{
			ItemID:      stack.ItemID,
			DisplayName: itemDisplayName(stack.ItemID),
			Quantity:    stack.Quantity,
		})
	}
	return items
}

// PlayerTradeWithUnit 玩家直驱：让她与同格/相邻的 NPC 买卖物品。
// 校验：归属玩家方 → 执行互斥（ErrExecutionBusy → 409）→ 目标在本局且存活 → 同格或相邻（≤1）→ 货量/钱款充足。
// 结算零 LLM、确定性；钱包按 unit.TradeService 的既有直改写法（非受保护字段），物品经既有背包原语（含攻防重算）。
func (service *Service) PlayerTradeWithUnit(ctx context.Context, sessionID, unitID string, req PlayerTradeRequest) (PlayerTradeResult, error) {
	result := PlayerTradeResult{}
	if service == nil || service.units == nil || service.sessions == nil {
		return result, fmt.Errorf("player trade: service unavailable")
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode != "buy" && mode != "sell" {
		return result, fmt.Errorf("交易方式只有买（buy）或卖（sell）")
	}
	itemID := strings.TrimSpace(req.ItemID)
	if itemID == "" {
		return result, fmt.Errorf("要换哪件东西，先指明白")
	}
	quantity := req.Quantity
	if quantity <= 0 {
		quantity = 1
	}
	if quantity > playerTradeMaxQuantity {
		return result, fmt.Errorf("一次最多换 %d 件，分次来吧", playerTradeMaxQuantity)
	}

	// 统一前置（与 player_actions 同口径）：直驱锁 + 执行互斥 + 终局门 + 归属本局玩家方（guard 内五道门齐备）。
	state, units, rec, release, err := service.guardPlayerAction(ctx, sessionID, unitID)
	if err != nil {
		return result, err
	}
	defer release()
	if !isBattleReady(*rec) {
		return result, fmt.Errorf("她此刻顾不上买卖")
	}
	target := findUnitInSession(units, strings.TrimSpace(req.TargetUnitID))
	if target == nil {
		return result, fmt.Errorf("那人不在这片天地")
	}
	if target.ID == rec.ID {
		return result, fmt.Errorf("自己跟自己做不成买卖")
	}
	// 目标阵营限制（评审 M2）：买卖只对野外 NPC / 据点公共 NPC / 自家人开放——否则可按目录价
	// 强制「自愿交易」扫光敌方口粮药剂（饿死耗死对手）或把杂物强卖给敌人抽干其钱包。
	if unitInIDList(state.EnemyUnitIDs, target.ID) {
		return result, fmt.Errorf("两家不是做买卖的关系")
	}
	if !isBattleReady(*target) {
		return result, fmt.Errorf("对方已无心交易")
	}
	if unit.HexDistance(rec.Status.PositionQ, rec.Status.PositionR, target.Status.PositionQ, target.Status.PositionR) > 1 {
		return result, fmt.Errorf("先让她走到对方身边")
	}

	definition, ok := item.Lookup(itemID)
	if !ok {
		return result, fmt.Errorf("没有这样的物件")
	}
	unitPrice := definition.Price
	if mode == "sell" {
		unitPrice = merchantSellPrice(definition.Price)
	}
	if unitPrice <= 0 {
		return result, fmt.Errorf("这件东西没人出价")
	}
	total := unitPrice * quantity

	buyer, seller := rec, target
	if mode == "sell" {
		buyer, seller = target, rec
	}
	if backpackStackSoulBound(*seller, itemID) {
		return result, fmt.Errorf("这件东西与魂魄相系，换不得")
	}
	if !hasBackpackQuantity(*seller, itemID, quantity) {
		if mode == "buy" {
			return result, fmt.Errorf("对方没有这么多货")
		}
		return result, fmt.Errorf("她的行囊里没有这么多")
	}
	if buyer.Status.Wallet < total {
		if mode == "buy" {
			return result, fmt.Errorf("她带的钱不够（需 %d 金）", total)
		}
		return result, fmt.Errorf("对方掏不出这个价钱（需 %d 金）", total)
	}

	// 结算：物品按数量转移（含派生攻防重算）+ 钱包按 单价×数量 调拨（镜像 PurchaseItem 的直改写法）。
	if err := unit.ConsumeBackpackItem(seller, itemID, quantity); err != nil {
		return result, fmt.Errorf("player trade (take): %w", err)
	}
	if err := unit.AddBackpackItem(buyer, itemID, quantity); err != nil {
		// 买方背包放不下：把扣减补回（尚未持久化，内存回滚即完整回滚），交易不成立。
		_ = unit.AddBackpackItem(seller, itemID, quantity)
		if mode == "buy" {
			return result, fmt.Errorf("她的行囊放不下了")
		}
		return result, fmt.Errorf("对方的行囊放不下了")
	}
	buyer.Status.Wallet -= total
	seller.Status.Wallet += total

	// 持久化：先存对方、再存她（savePair 未导出，无法同事务双写；按此序，万一第二笔失败，她分文未动、物不凭空入她手——
	// 不给玩家留任何可重放套利的口子；极小概率的世界侧损耗记错误返回，由玩家重试）。
	if err := service.units.Save(ctx, *target); err != nil {
		return result, fmt.Errorf("player trade (save target): %w", err)
	}
	if err := service.units.Save(ctx, *rec); err != nil {
		return result, fmt.Errorf("player trade (save actor): %w", err)
	}

	var summary, memory string
	if mode == "buy" {
		summary = fmt.Sprintf("依你的撮合，%s花 %d 金向%s买下了「%s」×%d。", rec.DisplayName(), total, target.DisplayName(), definition.DisplayName, quantity)
		memory = fmt.Sprintf("我向%s买了%s。", target.DisplayName(), definition.DisplayName)
	} else {
		summary = fmt.Sprintf("依你的撮合，%s把「%s」×%d 卖给了%s，得 %d 金。", rec.DisplayName(), definition.DisplayName, quantity, target.DisplayName(), total)
		memory = fmt.Sprintf("我把%s卖给了%s。", definition.DisplayName, target.DisplayName())
	}

	// 成交后效（全部 best-effort，绝不回滚已成交的买卖）：
	// 关系增益（复用 trade.go 口径）+ 人情锚（debt_grudge_love，喂 relevance）+ 她的记忆。
	service.applyTradeRelationShiftBestEffort(ctx, &state, rec, target, true, "一桩两厢情愿的买卖")
	service.bestEffortLitTradeDebtAnchor(ctx, &state, buyer, seller, definition.DisplayName)
	service.rememberUnitBestEffort(ctx, rec, state.TurnState.Turn, memory)

	// 落痕（appendLog + ReasonPlayerTileAction 流程事件 + 合并保存，best-effort）+ WS 推送让命运 feed 即时冒出来。
	service.recordPlayerActionTrace(ctx, &state, rec, "player_trade", summary, "player_trade")
	service.pushRealtime(state.ID, "fate_life_beat", map[string]any{
		"unit_id":   rec.ID,
		"narrative": summary,
		"turn":      state.TurnState.Turn,
	})

	result.OK = true
	result.SummaryZH = summary
	result.WalletAfter = rec.Status.Wallet
	result.Items = backpackSummary(*rec)
	return result, nil
}
