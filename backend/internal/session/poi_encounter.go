package session

// 文件说明：地图 POI 遭遇的玩家直驱结算闭环（开发计划 2026-06-10 §3.3，A3+A14）。
// 玩家点地图上的 POI 徽标「探一探/迎战」→ 按 map_pois.go 同口径解析该格 POI 类型 → 纯规则路由结算：
//   - 资源 POI → 「探明」：只落叙事+一条记忆（**不动钱包、不标 consumed**——若给点按发钱而又不消耗，
//     玩家可反复点击刷金币；故经济收益与消耗都留给采集路径（A-1 的 gather 加成），这里只做确认与铺垫）；
//   - 埋伏 → ResolveEliteEncounter（threat.go 的单人 elite 全链路：反射护栏/多回合消耗战/分赃/惩罚/收件箱），结后标 consumed；
//   - 行商 → 幂等铺货后返回货单（真正买卖走 trade_actions.go 的 PlayerTradeWithUnit），不标 consumed（行商可反复交易）；
//   - 求助/奇遇 → 2~3 分支确定性裁决（FNV(sessionID+坐标+turn+salt)，禁全局 rand），效果复用 random_events 的
//     施加路径 applyRandomEventBranch（受保护字段恒经 status.Mutator + 既有 reason code），结后标 consumed；
//   - 迷途 → 与该野外 NPC 结识（relation 四轴小幅正向，clamp 在 relation.go 内），结后标 consumed。
// 决策=玩家点击即玩家意志（零 LLM）；叙事=纯规则文案（祖魂语气、宣纸墨色调）；落痕=appendLog +
// POI_ENCOUNTER_RESOLVED 流程事件 + best-effort WS（fate_life_beat，前端命运 feed 零改即显示）；
// 防重放=State.ConsumedPOIs（poi_state.go 的 isPOIConsumed/markPOIConsumed）。

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strings"

	"qunxiang/backend/internal/engine/encounter"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/item"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// POI 遭遇结果的 kind 取值（前端按 kind 分流展示：货单面板/战报卡/分支叙事）。
const (
	poiKindResource  = "resource"
	poiKindAmbush    = "ambush"
	poiKindMerchant  = "merchant"
	poiKindHelp      = "help"
	poiKindAdventure = "adventure"
	poiKindLost      = "lost"
)

// POIEncounterOutcome 是一次 POI 结算的状态增减明细（按需填充，零值省略）。
type POIEncounterOutcome struct {
	WalletDelta      int     `json:"wallet_delta,omitempty"`
	HungerDelta      int     `json:"hunger_delta,omitempty"`
	MoraleDelta      float64 `json:"morale_delta,omitempty"`
	GainedItemID     string  `json:"gained_item_id,omitempty"`
	GainedItemName   string  `json:"gained_item_name,omitempty"`
	GainedItemQty    int     `json:"gained_item_qty,omitempty"`
	RelationZH       string  `json:"relation_zh,omitempty"`       // 迷途结识：关系变化的中文说明
	EffectSummaryZH  string  `json:"effect_summary_zh,omitempty"` // 状态增减的中文一句话（复用 random_events 口径）
	EncounterOutcome string  `json:"encounter_outcome,omitempty"` // 埋伏：defeated / fled / down
	DamageTaken      int     `json:"damage_taken,omitempty"`      // 埋伏：承受伤害
	PenaltyLayer     int     `json:"penalty_layer,omitempty"`     // 埋伏失败：经分级闸落地的后果层
	Awards           []string `json:"awards,omitempty"`           // 埋伏胜利：战利品概要（金币×15 等）
}

// MerchantGood 是行商货单里的一件商品报价。
type MerchantGood struct {
	ItemID      string `json:"item_id"`
	DisplayName string `json:"display_name"`
	Quantity    int    `json:"quantity"`
	BuyPrice    int    `json:"buy_price"`  // 她买入的单价（物品目录基准价）
	SellPrice   int    `json:"sell_price"` // 她卖出的参考单价（基准价八成，与 trade.go 低档报价同源）
}

// POIEncounterResult 是一次 POI 遭遇结算的整体返回。
type POIEncounterResult struct {
	OK             bool                `json:"ok"`
	Kind           string              `json:"kind"`      // resource/ambush/merchant/help/adventure/lost
	TypeCode       string              `json:"type_code"` // 矿脉/药田/灵泉/古迹遗物 或 奇遇/求助/埋伏/行商/迷途
	SummaryZH      string              `json:"summary_zh"`
	Outcome        POIEncounterOutcome `json:"outcome"`
	MerchantGoods  []MerchantGood      `json:"merchant_goods,omitempty"`
	MerchantUnitID string              `json:"merchant_unit_id,omitempty"`
	Consumed       bool                `json:"consumed"` // 本次结算后该 POI 是否已被消耗
}

// unitOwnedByPlayer 判断某单位是否归本会话玩家方（在 state.PlayerUnitIDs 里）。
// 玩家直驱只许差遣自己的人——guardPlayerAction 只校验「归属本局」，这里补「归属玩家方」。
func unitOwnedByPlayer(state State, unitID string) bool {
	for _, id := range state.PlayerUnitIDs {
		if id == unitID {
			return true
		}
	}
	return false
}

// poiEncounterRoll 是 POI 结算的确定性掷骰：FNV(sessionID | 坐标 | turn | salt) → [0,1)。
// 与 map_pois.go 的 poiRoll 区别：这里**拌 turn**（分支裁决允许不同回合走向不同），POI 本体标注不拌。
func poiEncounterRoll(state State, q, r int, salt string) float64 {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(state.ID))
	_, _ = hasher.Write([]byte("|poi_branch|"))
	_, _ = hasher.Write([]byte(fmt.Sprintf("%d,%d|%d|", q, r, state.TurnState.Turn)))
	_, _ = hasher.Write([]byte(salt))
	return float64(hasher.Sum32()%10000) / 10000
}

// pickByRoll 按掷骰从候选里确定性选一个下标（clamp 到合法区间）。
func pickByRoll(roll float64, count int) int {
	idx := int(roll * float64(count))
	if idx < 0 {
		idx = 0
	}
	if idx >= count {
		idx = count - 1
	}
	return idx
}

// findPOIAt 在 (q,r) 解析该格 POI——与 map_pois.go 的派生口径完全一致（复用其 compute 函数）。
// 同格既有野外 NPC 事件又有地块资源时，人比物显眼：NPC 事件优先。
func findPOIAt(state State, byID map[string]*unit.Record, q, r int) (MapPOI, bool) {
	for _, poi := range computeMapNPCEventPOIs(state, byID) {
		if poi.Q == q && poi.R == r {
			return poi, true
		}
	}
	for _, poi := range computeMapResourcePOIs(state) {
		if poi.Q == q && poi.R == r {
			return poi, true
		}
	}
	return MapPOI{}, false
}

// poiKindFromTypeCode 把 map_pois 的中文事件类型映射为结果 kind。
func poiKindFromTypeCode(kind MapPOIKind, typeCode string) string {
	if kind == MapPOIResource {
		return poiKindResource
	}
	switch typeCode {
	case "埋伏":
		return poiKindAmbush
	case "行商":
		return poiKindMerchant
	case "求助":
		return poiKindHelp
	case "奇遇":
		return poiKindAdventure
	case "迷途":
		return poiKindLost
	default:
		return poiKindAdventure // 未知事件类型按奇遇兜底（npcEventTypes 之外不应出现）
	}
}

// poiImportance 按类型给流程事件定 importance（2~6：埋伏最重、资源/行商最轻）。
func poiImportance(kind string) int {
	switch kind {
	case poiKindAmbush:
		return 6
	case poiKindAdventure:
		return 5
	case poiKindHelp:
		return 4
	case poiKindLost:
		return 3
	default: // resource / merchant
		return 2
	}
}

// poiBranchSpec 是求助/奇遇的一个确定性分支：效果复用 random_events 的 randomEventBranch 结构与施加路径，
// 叙事是本文件自己的祖魂语气模板（确定性选一，零 LLM）。
type poiBranchSpec struct {
	branch     randomEventBranch
	narratives []string // 2~3 套中文文案模板（含 %s 占位时填角色名）
	memory     string   // 她记下的一句（第一人称）
}

// helpBranchSpecs 是「求助」POI 的三个分支（善报/微劳/拖累）。
func helpBranchSpecs() []poiBranchSpec {
	return []poiBranchSpec{
		{
			branch: randomEventBranch{ID: "poi_help_reward", Label: "出手相助", WalletDelta: 6, MoraleDelta: 0.03},
			narratives: []string{
				"%s停下脚步帮了那人一把。事毕，对方千恩万谢，执意塞给她几枚铜钱。",
				"%s没有袖手。忙完那桩难处，那人留下些谢礼便匆匆赶路去了。",
			},
			memory: "我帮了一个求助的路人，得了谢礼。",
		},
		{
			branch: randomEventBranch{ID: "poi_help_guide", Label: "指点迷津", MoraleDelta: 0.02},
			narratives: []string{
				"%s帮不上大忙，只给那人细细指了条稳妥的路。对方拜谢而去，她心里也松快了些。",
				"%s把所知的路径一一说了。那人作揖道谢，她目送了一程才转身。",
			},
			memory: "我给求助的人指了条路。",
		},
		{
			branch: randomEventBranch{ID: "poi_help_drained", Label: "受了拖累", HungerDelta: -3, MoraleDelta: -0.02},
			narratives: []string{
				"那桩求助比看上去棘手得多。%s搭进去半日气力，事却没办利索，落得又累又饿。",
				"%s费了一番周折，终究没能帮到点子上。日头偏西，她拍拍尘土继续赶路。",
			},
			memory: "我管了桩闲事，白搭了半日气力。",
		},
	}
}

// adventureBranchSpecs 是「奇遇」POI 的三个分支（旧藏/点拨/空欢喜）。
func adventureBranchSpecs() []poiBranchSpec {
	return []poiBranchSpec{
		{
			branch: randomEventBranch{ID: "poi_adventure_cache", Label: "拾获旧藏", WalletDelta: 8, GainItemID: "herb_bundle", GainItemQty: 1},
			narratives: []string{
				"%s在草窠里翻出一只遗落的旧包袱——几枚铜钱，还有一束晒干的药草。",
				"机缘巧合，%s寻见一处无主的旧藏。钱物不多，却是白来的进项。",
			},
			memory: "我拾到一只遗落的包袱。",
		},
		{
			branch: randomEventBranch{ID: "poi_adventure_insight", Label: "高人点拨", MoraleDelta: 0.05},
			narratives: []string{
				"%s遇上一位过路的老者，听了几句没头没尾的话——细想之下，竟句句在理。",
				"一场萍水相逢的闲谈，叫%s心里亮堂了不少。",
			},
			memory: "我得了一位过路人的点拨。",
		},
		{
			branch: randomEventBranch{ID: "poi_adventure_futile", Label: "空欢喜", HungerDelta: -4, MoraleDelta: -0.03},
			narratives: []string{
				"所谓奇遇原是个空欢喜。%s白绕了远路，耗了气力，悻悻而归。",
				"%s追着那点异样寻了半日，一无所获。她拢了拢衣襟，把这事抛在脑后。",
			},
			memory: "我追了半日的奇遇，原是空欢喜。",
		},
	}
}

// resourceScoutNarratives 是资源 POI「探明」的祖魂语气文案（%s 填资源名）。
var resourceScoutNarratives = []string{
	"她俯身细看，探明了这处%s的来路。再动手采掘时，当有更丰的收成。",
	"她在这处%s前驻足良久，把地脉纹理记进了心里——这里的出产，瞒不过她的眼了。",
	"她拨开浮土辨认了一番，认下了这处%s。她记住了位置，回头再来取。",
}

// lostNarratives 是「迷途」结识的祖魂语气文案（第一个 %s 填她名、第二个填对方名）。
var lostNarratives = []string{
	"%s给迷途的%s指了归路。两人同行了一程，话越说越投机，算是相识了。",
	"%s领着%s走出了岔路。临别时对方郑重报了姓名，说后会有期。",
	"一程带路下来，%s与%s熟络了几分——萍水相逢，也是缘分。",
}

// merchantStockCandidates 是行商的候选货底（确定性轮转铺货 3~5 件；消耗品/材料为主、夹一两件轻兵刃）。
var merchantStockCandidates = []unit.ItemStack{
	{ItemID: "ration", Quantity: 2},
	{ItemID: "herb_bundle", Quantity: 2},
	{ItemID: "healing_potion", Quantity: 1},
	{ItemID: "torch", Quantity: 2},
	{ItemID: "rope", Quantity: 1},
	{ItemID: "antidote", Quantity: 1},
	{ItemID: "pickaxe", Quantity: 1},
	{ItemID: "dagger", Quantity: 1},
}

// ResolvePOIEncounter 玩家直驱触发并结算 (q,r) 格的 POI 遭遇。
// 校验：归属玩家方 → 执行互斥（ErrExecutionBusy → 409）→ 站在该格或相邻（≤1）→ 该格确有 POI 且未消耗。
// 结算按类型路由（见文件头）；全程零 LLM、确定性、受保护字段恒经 status.Mutator。
func (service *Service) ResolvePOIEncounter(ctx context.Context, sessionID, unitID string, q, r int) (POIEncounterResult, error) {
	result := POIEncounterResult{}
	if service == nil || service.units == nil || service.sessions == nil {
		return result, fmt.Errorf("poi encounter: service unavailable")
	}
	// 统一前置（与 player_actions 同口径）：直驱锁 + 执行互斥 + 终局门 + 归属本局玩家方（guard 内五道门齐备）。
	state, units, rec, release, err := service.guardPlayerAction(ctx, sessionID, unitID)
	if err != nil {
		return result, err
	}
	defer release()
	if !isBattleReady(*rec) {
		return result, fmt.Errorf("她此刻没有力气探看")
	}
	coord := world.Coord{Q: q, R: r}
	if !inBounds(state.Map, coord) {
		return result, fmt.Errorf("那里在天地之外，去不得")
	}
	if unit.HexDistance(rec.Status.PositionQ, rec.Status.PositionR, q, r) > 1 {
		return result, fmt.Errorf("她还没走到那里")
	}
	byID := mapRecordsByID(units)
	poi, found := findPOIAt(state, byID, q, r)
	if !found {
		return result, fmt.Errorf("这里并无可探看之事")
	}
	// 消耗检查按 POI 类型分键：NPC 事件跟人走（unitID 键，防 NPC 游走后在新格「重新长出」可刷事件），
	// 资源跟格子走（坐标键，与采集 ×1.5 加成的消耗口径一致）。
	if poi.Kind == MapPOINPCEvent && poi.UnitID != "" {
		if isNPCEventConsumed(&state, poi.UnitID) {
			return result, fmt.Errorf("这里已被探看过了")
		}
	} else if isPOIConsumed(&state, q, r) {
		return result, fmt.Errorf("这里已被探看过了")
	}

	result.Kind = poiKindFromTypeCode(poi.Kind, poi.TypeCode)
	result.TypeCode = poi.TypeCode

	switch result.Kind {
	case poiKindResource:
		err = service.resolvePOIResource(ctx, &state, rec, poi, &result)
	case poiKindAmbush:
		err = service.resolvePOIAmbush(ctx, &state, rec, q, r, poi, &result)
	case poiKindMerchant:
		err = service.resolvePOIMerchant(ctx, &state, byID, rec, poi, &result)
	case poiKindHelp, poiKindAdventure:
		err = service.resolvePOIBranchEvent(ctx, &state, rec, q, r, poi, &result)
	case poiKindLost:
		err = service.resolvePOILost(ctx, &state, byID, rec, q, r, poi, &result)
	}
	if err != nil {
		return result, err
	}
	result.OK = true

	// 统一落痕：appendLog + 流程事件（POI_ENCOUNTER_RESOLVED，best-effort）+ WS 推送（best-effort）+ 持久化。
	appendLog(&state, "poi_encounter", result.SummaryZH, rec.ID, poi.UnitID)
	service.recordPOIEncounterTrace(ctx, &state, rec.ID, poi, result)
	service.pushRealtime(state.ID, "fate_life_beat", map[string]any{
		"unit_id":   rec.ID,
		"narrative": result.SummaryZH,
		"turn":      state.TurnState.Turn,
	})
	// 合并保存（与异步落盘同口径）：ConsumedPOIs/日志随会话整块 JSON 持久化，不覆盖外部并发写入的指令。
	if err := service.saveSessionMergingExternalEvents(ctx, &state); err != nil {
		return result, fmt.Errorf("poi encounter (save): %w", err)
	}
	return result, nil
}

// resolvePOIResource 结算资源 POI「探明」：只落叙事+一条记忆，不动钱包、不标 consumed。
// 取舍：消耗与经济收益统一留给采集路径（A-1 的 gather ×1.5 加成会标 consumed）——若这里发钱而不消耗，
// 点按可无限刷金币；若发钱又消耗，则探明反而吃掉了采集加成。叙事确认是这里唯一的职责。
func (service *Service) resolvePOIResource(ctx context.Context, state *State, rec *unit.Record, poi MapPOI, result *POIEncounterResult) error {
	narrative := fmt.Sprintf(
		resourceScoutNarratives[pickByRoll(poiEncounterRoll(*state, poi.Q, poi.R, "resource_narrative"), len(resourceScoutNarratives))],
		poi.TypeCode,
	)
	result.SummaryZH = narrative
	result.Consumed = false
	service.rememberUnitBestEffort(ctx, rec, state.TurnState.Turn, fmt.Sprintf("我探明了一处%s。", poi.TypeCode))
	return nil
}

// resolvePOIAmbush 结算埋伏 POI：照 TriggerEliteEncounter 的构造口径（scaledElite 生成与她战力相称的精英怪），
// 走 ResolveEliteEncounter 全链路（反射护栏/多回合消耗战/分赃/惩罚/收件箱，HP/士气/钱包恒经 Mutator）。
// 区别于 TriggerEliteEncounter 的临时 State{ID}：这里传入**真实加载的 state**，其内部 appendLog/收件箱随后一并持久化。
func (service *Service) resolvePOIAmbush(ctx context.Context, state *State, rec *unit.Record, q, r int, poi MapPOI, result *POIEncounterResult) error {
	elite := scaledElite(*rec)
	// 威胁 ID 确定性化：scaledElite 默认给 uuid，而 combat_roll 会把该 ID 拌进掷骰——同一会话同一回合
	// 重放同一次埋伏必须得到同一战斗轨迹（仓库确定性纪律：sessionID+turn+actor 哈希，禁随机源）。
	elite.ID = fmt.Sprintf("poi_ambush_%d_%d_t%d", q, r, state.TurnState.Turn)
	encResult, err := service.ResolveEliteEncounter(ctx, state, rec, elite)
	if err != nil {
		return fmt.Errorf("poi ambush: %w", err)
	}
	result.SummaryZH = encResult.InboxCard
	result.Outcome.EncounterOutcome = encResult.Outcome
	result.Outcome.DamageTaken = encResult.DamageTaken
	result.Outcome.PenaltyLayer = encResult.PenaltyLayer
	for _, award := range encResult.Awards {
		name := itemDisplayName(award.ItemID)
		if award.ItemID == "gold" {
			name = "金币"
		}
		if award.Reason == encounter.AwardConsolation && award.Quantity == 0 {
			continue
		}
		result.Outcome.Awards = append(result.Outcome.Awards, fmt.Sprintf("%s×%d", name, award.Quantity))
	}
	// 埋伏无论胜负都已「发生过」：按 NPC 键标 consumed（跟人走），防同一伙埋伏游走后反复触发刷战利品/刷惩罚。
	markNPCEventConsumed(state, poi.UnitID)
	result.Consumed = true
	return nil
}

// resolvePOIMerchant 结算行商 POI：幂等铺货后返回货单。真正买卖走 trade_actions.go 的 PlayerTradeWithUnit；
// 不标 consumed——行商可反复交易（货底由背包真实存量约束，卖完自然无货）。
func (service *Service) resolvePOIMerchant(ctx context.Context, state *State, byID map[string]*unit.Record, rec *unit.Record, poi MapPOI, result *POIEncounterResult) error {
	merchant := byID[poi.UnitID]
	if merchant == nil || !isBattleReady(*merchant) {
		return fmt.Errorf("那行商已不在此处")
	}
	if err := service.ensureMerchantStock(ctx, state, merchant); err != nil {
		return fmt.Errorf("poi merchant (stock): %w", err)
	}
	result.MerchantUnitID = merchant.ID
	result.MerchantGoods = merchantManifest(*merchant)
	result.SummaryZH = fmt.Sprintf("%s遇上了行商%s。对方摊开货担——要买要卖，由你定夺。", rec.DisplayName(), merchant.DisplayName())
	result.Consumed = false
	return nil
}

// ensureMerchantStock 给行商幂等铺货：已有 ≥3 种货底则不动；否则按 FNV(sessionID+坐标+salt) 确定性轮转
// 从候选货底取 3~5 件入其背包并持久化。**不拌 turn**（与 map_pois 的静态标注同理：货底不应逐回合漂移）。
// 单件入包失败（背包满等）best-effort 跳过，不阻断整体。
func (service *Service) ensureMerchantStock(ctx context.Context, state *State, merchant *unit.Record) error {
	if len(merchant.Inventory.Backpack) >= 3 {
		return nil // 已有货底：幂等，不重复铺
	}
	coord := world.Coord{Q: merchant.Status.PositionQ, R: merchant.Status.PositionR}
	count := 3 + pickByRoll(poiRoll(state.ID, coord, "merchant_stock_count:"+merchant.ID), 3) // 3~5 件
	start := pickByRoll(poiRoll(state.ID, coord, "merchant_stock_start:"+merchant.ID), len(merchantStockCandidates))
	for i := 0; i < count; i++ {
		entry := merchantStockCandidates[(start+i)%len(merchantStockCandidates)]
		if err := unit.AddBackpackItem(merchant, entry.ItemID, entry.Quantity); err != nil {
			slog.Warn("merchant stock item skipped (best-effort)", "merchant", merchant.ID, "item", entry.ItemID, "err", err)
		}
	}
	return service.units.Save(ctx, *merchant)
}

// merchantManifest 把行商背包的真实存量整理成货单报价（最多 6 行；灵魂绑定之物不入市）。
// 定价与 trade.go 同源：买入=目录基准价；卖出参考=基准价八成（见 trade_actions.go 的 merchantSellPrice）。
func merchantManifest(merchant unit.Record) []MerchantGood {
	goods := make([]MerchantGood, 0, len(merchant.Inventory.Backpack))
	for _, stack := range merchant.Inventory.Backpack {
		if stack.ItemID == "" || stack.Quantity <= 0 || stack.SoulBound {
			continue
		}
		definition, ok := item.Lookup(stack.ItemID)
		if !ok || definition.Price <= 0 {
			continue
		}
		goods = append(goods, MerchantGood{
			ItemID:      stack.ItemID,
			DisplayName: definition.DisplayName,
			Quantity:    stack.Quantity,
			BuyPrice:    definition.Price,
			SellPrice:   merchantSellPrice(definition.Price),
		})
		if len(goods) >= 6 {
			break
		}
	}
	return goods
}

// resolvePOIBranchEvent 结算求助/奇遇 POI：FNV 确定性选分支，效果复用 random_events 的施加路径
// applyRandomEventBranch（钱包/饥饿/士气恒经 status.Mutator + 既有 reason code，物品发放入背包），叙事纯规则文案。
func (service *Service) resolvePOIBranchEvent(ctx context.Context, state *State, rec *unit.Record, q, r int, poi MapPOI, result *POIEncounterResult) error {
	specs := helpBranchSpecs()
	if result.Kind == poiKindAdventure {
		specs = adventureBranchSpecs()
	}
	spec := specs[pickByRoll(poiEncounterRoll(*state, q, r, "branch:"+result.Kind), len(specs))]

	before := rec.Status
	effectSummary, err := service.applyRandomEventBranch(ctx, state, rec, spec.branch)
	if err != nil {
		return fmt.Errorf("poi branch event: %w", err)
	}
	// 物品发放等背包变更只改内存，需持久化（与 resolveTurnRandomEvent 的施加后 Save 同口径）。
	if err := service.units.Save(ctx, *rec); err != nil {
		return fmt.Errorf("poi branch event (save unit): %w", err)
	}

	result.Outcome.WalletDelta = rec.Status.Wallet - before.Wallet
	result.Outcome.HungerDelta = rec.Status.Hunger - before.Hunger
	result.Outcome.MoraleDelta = rec.Status.Morale - before.Morale
	result.Outcome.EffectSummaryZH = effectSummary
	if spec.branch.GainItemID != "" && strings.Contains(effectSummary, "获得") {
		result.Outcome.GainedItemID = spec.branch.GainItemID
		result.Outcome.GainedItemName = itemDisplayName(spec.branch.GainItemID)
		result.Outcome.GainedItemQty = spec.branch.GainItemQty
		if result.Outcome.GainedItemQty <= 0 {
			result.Outcome.GainedItemQty = 1
		}
	}

	narrative := spec.narratives[pickByRoll(poiEncounterRoll(*state, q, r, "narrative:"+spec.branch.ID), len(spec.narratives))]
	result.SummaryZH = fmt.Sprintf(narrative, rec.DisplayName())
	service.rememberUnitBestEffort(ctx, rec, state.TurnState.Turn, spec.memory)

	// 求助/奇遇挂在野外 NPC 身上：按 NPC 键标 consumed（跟人走，防游走后重新可刷）。
	markNPCEventConsumed(state, poi.UnitID)
	result.Consumed = true
	return nil
}

// resolvePOILost 结算迷途 POI：她给迷途的野外 NPC 带路，两人小幅结识（trust/affection +1~2，
// 经 relation.go 的既有路径写入并 clamp，best-effort 不阻断）。
func (service *Service) resolvePOILost(ctx context.Context, state *State, byID map[string]*unit.Record, rec *unit.Record, q, r int, poi MapPOI, result *POIEncounterResult) error {
	stranger := byID[poi.UnitID]
	if stranger == nil || !isBattleReady(*stranger) {
		return fmt.Errorf("那迷途之人已不在此处")
	}
	trustDelta := 1 + poiEncounterRoll(*state, q, r, "lost_trust")         // [1,2)
	affectionDelta := 1 + poiEncounterRoll(*state, q, r, "lost_affection") // [1,2)
	delta := relationDelta{Trust: trustDelta, Affection: affectionDelta}
	service.applyMutualRelationShiftBestEffort(
		ctx, state, rec, stranger, delta, delta,
		fmt.Sprintf("迷途相遇，%s给%s指了归路", rec.DisplayName(), stranger.DisplayName()),
	)
	result.Outcome.RelationZH = fmt.Sprintf("她与%s结识了（信任+%.1f 好感+%.1f）", stranger.DisplayName(), trustDelta, affectionDelta)

	narrative := lostNarratives[pickByRoll(poiEncounterRoll(*state, q, r, "lost_narrative"), len(lostNarratives))]
	result.SummaryZH = fmt.Sprintf(narrative, rec.DisplayName(), stranger.DisplayName())
	service.rememberUnitBestEffort(ctx, rec, state.TurnState.Turn, fmt.Sprintf("我在路上结识了%s。", stranger.DisplayName()))

	// 迷途结识挂在那个人身上：按 NPC 键标 consumed（同一人只结识一次，换地方也不重复）。
	markNPCEventConsumed(state, poi.UnitID)
	result.Consumed = true
	return nil
}

// recordPOIEncounterTrace 给一次已成功的 POI 结算写流程事件（POI_ENCOUNTER_RESOLVED，best-effort 吞错只 log）。
func (service *Service) recordPOIEncounterTrace(ctx context.Context, state *State, actorID string, poi MapPOI, result POIEncounterResult) {
	if service == nil || service.db == nil || state == nil {
		return
	}
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:     state.ID,
		OwnerUnitID:   actorID,
		RelatedUnitID: poi.UnitID,
		Code:          events.ReasonPOIEncounterResolve,
		Category:      events.CategoryLifecycle,
		Payload: map[string]any{
			"narrative":  result.SummaryZH,
			"kind":       result.Kind,
			"type_code":  result.TypeCode,
			"q":          poi.Q,
			"r":          poi.R,
			"consumed":   result.Consumed,
			"importance": poiImportance(result.Kind),
			"reason":     string(events.ReasonPOIEncounterResolve),
		},
		WorldID:  state.WorldID,
		RegionID: state.ID,
		Tick:     state.TurnState.Turn,
	}); err != nil {
		slog.Warn("record poi encounter trace failed (best-effort)", "session", state.ID, "unit", actorID, "err", err)
	}
}
