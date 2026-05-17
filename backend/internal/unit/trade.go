package unit

// 文件说明：单位交易服务，封装金币转移、赠送、交换、购买与相邻可交易约束校验。

import (
	"context"
	"fmt"
)

// TradeService 结构体用于承载该模块的核心数据。
type TradeService struct {
	repository *Repository
}

// NewTradeService 创建单位交易服务实例。
func NewTradeService(repository *Repository) *TradeService {
	return &TradeService{repository: repository}
}

// TransferGold 在相邻且非战斗状态单位之间转移金币。
func (service *TradeService) TransferGold(ctx context.Context, fromUnitID string, toUnitID string, amount int) (Record, Record, error) {
	if amount <= 0 {
		return Record{}, Record{}, fmt.Errorf("amount must be positive")
	}

	from, to, err := service.loadTradePair(ctx, fromUnitID, toUnitID)
	if err != nil {
		return Record{}, Record{}, err
	}
	if from.Status.Wallet < amount {
		return Record{}, Record{}, fmt.Errorf("insufficient wallet balance")
	}

	from.Status.Wallet -= amount
	to.Status.Wallet += amount
	if err := service.savePair(ctx, from, to); err != nil {
		return Record{}, Record{}, err
	}
	return from, to, nil
}

// GiftItem 把一件物品从赠与方转移到接收方背包。
func (service *TradeService) GiftItem(ctx context.Context, fromUnitID string, toUnitID string, itemID string) (Record, Record, error) {
	from, to, err := service.loadTradePair(ctx, fromUnitID, toUnitID)
	if err != nil {
		return Record{}, Record{}, err
	}

	stack, err := TakeItem(&from, itemID)
	if err != nil {
		return Record{}, Record{}, err
	}

	if err := AddBackpackItem(&to, stack.ItemID, stack.Quantity); err != nil {
		return Record{}, Record{}, err
	}
	RecalculateDerivedStats(&from)

	if err := service.savePair(ctx, from, to); err != nil {
		return Record{}, Record{}, err
	}
	return from, to, nil
}

// SwapItems 在两单位之间交换指定物品。
func (service *TradeService) SwapItems(
	ctx context.Context,
	leftUnitID string,
	rightUnitID string,
	leftItemID string,
	rightItemID string,
) (Record, Record, error) {
	left, right, err := service.loadTradePair(ctx, leftUnitID, rightUnitID)
	if err != nil {
		return Record{}, Record{}, err
	}

	leftStack, err := TakeItem(&left, leftItemID)
	if err != nil {
		return Record{}, Record{}, err
	}
	rightStack, err := TakeItem(&right, rightItemID)
	if err != nil {
		return Record{}, Record{}, err
	}

	if err := AddBackpackItem(&left, rightStack.ItemID, rightStack.Quantity); err != nil {
		return Record{}, Record{}, err
	}
	if err := AddBackpackItem(&right, leftStack.ItemID, leftStack.Quantity); err != nil {
		return Record{}, Record{}, err
	}

	RecalculateDerivedStats(&left)
	RecalculateDerivedStats(&right)
	if err := service.savePair(ctx, left, right); err != nil {
		return Record{}, Record{}, err
	}
	return left, right, nil
}

// PurchaseItem 执行买卖：转移物品并完成金币结算。
func (service *TradeService) PurchaseItem(
	ctx context.Context,
	buyerUnitID string,
	sellerUnitID string,
	itemID string,
	price int,
) (Record, Record, error) {
	if price <= 0 {
		return Record{}, Record{}, fmt.Errorf("price must be positive")
	}

	buyer, seller, err := service.loadTradePair(ctx, buyerUnitID, sellerUnitID)
	if err != nil {
		return Record{}, Record{}, err
	}
	if buyer.Status.Wallet < price {
		return Record{}, Record{}, fmt.Errorf("buyer does not have enough gold")
	}

	stack, err := TakeItem(&seller, itemID)
	if err != nil {
		return Record{}, Record{}, err
	}
	if err := AddBackpackItem(&buyer, stack.ItemID, stack.Quantity); err != nil {
		return Record{}, Record{}, err
	}

	buyer.Status.Wallet -= price
	seller.Status.Wallet += price
	RecalculateDerivedStats(&seller)
	RecalculateDerivedStats(&buyer)
	if err := service.savePair(ctx, buyer, seller); err != nil {
		return Record{}, Record{}, err
	}
	return buyer, seller, nil
}

// loadTradePair 加载交易双方并校验相邻约束。
// 是否处于允许交易的战场时机由 session 层结合局势统一决定。
func (service *TradeService) loadTradePair(ctx context.Context, fromUnitID string, toUnitID string) (Record, Record, error) {
	from, err := service.repository.GetByID(ctx, fromUnitID)
	if err != nil {
		return Record{}, Record{}, err
	}
	to, err := service.repository.GetByID(ctx, toUnitID)
	if err != nil {
		return Record{}, Record{}, err
	}

	if HexDistance(from.Status.PositionQ, from.Status.PositionR, to.Status.PositionQ, to.Status.PositionR) != 1 {
		return Record{}, Record{}, fmt.Errorf("trade requires adjacent units")
	}

	return from, to, nil
}

// savePair 在同一事务内落库交易双方单位记录。
func (service *TradeService) savePair(ctx context.Context, left Record, right Record) error {
	tx, err := service.repository.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin trade transaction: %w", err)
	}
	defer tx.Rollback()

	if err := service.repository.saveWithExecer(ctx, tx, left); err != nil {
		return err
	}
	if err := service.repository.saveWithExecer(ctx, tx, right); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit trade transaction: %w", err)
	}
	return nil
}
