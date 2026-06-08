package analytics

import (
	"encoding/json"
	"testing"
)

// TestP1FunnelConstantValues 锁定 P1 漏斗补全的事件常量字面量值，
// 防止跨 agent 引用（billing/compliance/router）时因常量值漂移而埋点对不齐。
func TestP1FunnelConstantValues(t *testing.T) {
	cases := map[string]string{
		EventAccountRegistered: "account_registered",
		EventCharterCompleted:  "charter_completed",
		EventInboxOpened:       "inbox_opened",
		EventStatusCardViewed:  "status_card_viewed",
		EventShareInitiated:    "share_initiated",
		EventPurchase:          "purchase",
		EventComplianceBlocked: "compliance_blocked",
	}
	for got, want := range cases {
		if got != want {
			t.Fatalf("事件常量值漂移：得到 %q，应为 %q", got, want)
		}
	}
}

// TestP1FunnelConstantsDistinct 确保新旧事件常量两两不重复，
// 否则不同漏斗阶段会串名导致统计污染。
func TestP1FunnelConstantsDistinct(t *testing.T) {
	all := []string{
		EventSessionCreated, EventCharacterCreated, EventDecisionPending,
		EventDecisionResolved, EventIntervention, EventReturnVisit,
		EventAccountRegistered, EventCharterCompleted, EventInboxOpened,
		EventStatusCardViewed, EventShareInitiated, EventPurchase,
		EventComplianceBlocked,
	}
	seen := make(map[string]bool, len(all))
	for _, name := range all {
		if name == "" {
			t.Fatalf("事件常量不应为空")
		}
		if seen[name] {
			t.Fatalf("事件常量重复：%q", name)
		}
		seen[name] = true
	}
}

// TestP1RichPropsPayload 验证 Props(map[string]any) 已能承载验证设计 §5.2
// 所需的更丰富字段（resolve_latency_sec / was_against_will 等），无需改 Emit 签名。
func TestP1RichPropsPayload(t *testing.T) {
	ctx, db := newDB(t)
	props := map[string]any{
		"resolve_latency_sec": 42.5,
		"was_against_will":    true,
		"amount_cents":        int64(1200),
	}
	if err := Emit(ctx, db, Event{Stage: StageRevenue, Name: EventPurchase, SessionID: "s9", Props: props}); err != nil {
		t.Fatalf("富 payload emit 失败: %v", err)
	}
	var stored string
	if err := db.QueryRowContext(ctx, `SELECT properties_json FROM product_events WHERE event_name = ?`, EventPurchase).Scan(&stored); err != nil {
		t.Fatalf("scan 失败: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal([]byte(stored), &round); err != nil {
		t.Fatalf("properties_json 非合法 JSON: %v", err)
	}
	if round["was_against_will"] != true {
		t.Fatalf("was_against_will 未原样落库，得到 %#v", round["was_against_will"])
	}
	if _, ok := round["resolve_latency_sec"]; !ok {
		t.Fatalf("resolve_latency_sec 应可承载于 Props")
	}
}
