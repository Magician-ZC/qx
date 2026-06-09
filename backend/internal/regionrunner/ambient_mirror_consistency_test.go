package regionrunner

// 文件说明：「本包野心打分镜像 ↔ session 权威实现」一致性钉死（M7.3-real 动机栈消费）。
// 因 session 测试反向 import regionrunner 致 regionrunner 非测试不可 import session（cycle），本包在 ambient_decision.go
// 内**镜像**了 session/ambition_scoring.go 的打分纯函数。本测试（regionrunner 测试包可 import session，无 cycle）
// 用同一批输入逐位比对两侧输出——任一侧改了常量/映射/分档而忘了同步，此测试即 fail，防镜像漂移。

import (
	"testing"

	"qunxiang/backend/internal/session"
	"qunxiang/backend/internal/unit"
)

// TestAmbientScoringMirrorsSession 逐位比对本包镜像与 session 权威实现的三件套：
// ActionTag 映射、AmbientBaseScore 需求强度、AmbitionActionWeight 野心乘权（含 flag 开/关两态）。
func TestAmbientScoringMirrorsSession(t *testing.T) {
	actions := []ambientAction{actForage, actRest, actSocialize, actReflect}

	// —— 动作 → 野心标签：本包 ambientActionTagTable vs session.ActionAmbitionTagForAmbient。
	for _, act := range actions {
		mine := ambientActionTagTable[act]
		theirs := session.ActionAmbitionTagForAmbient(string(act))
		if mine != theirs {
			t.Fatalf("动作标签镜像漂移：%s 本包=%q session=%q", act, mine, theirs)
		}
	}

	// —— 基础分 + 野心乘权：遍历状态/野心组合，两态 flag 各比对。
	hungers := []int{10, 39, 40, 80}
	morales := []float64{0.1, 0.4, 0.6, 0.85}
	ambitions := []unit.Ambition{
		{},
		{Wealth: 1.0},
		{Power: 0.8, Vengeance: 0.5},
		{Lineage: 0.6, Mastery: 0.9, Freedom: 0.3},
	}
	for _, flag := range []string{"", "true"} {
		t.Setenv("QUNXIANG_AMBITION_SCORING", flag)
		for _, amb := range ambitions {
			for _, h := range hungers {
				for _, m := range morales {
					var rec unit.Record
					rec.Status.Hunger = h
					rec.Status.Morale = m
					rec.Ambition = amb
					for _, act := range actions {
						// 基础分（与 flag 无关，但一并覆盖）。
						mineBase := ambientBaseScore(rec, act)
						theirBase := session.AmbientBaseScore(rec, string(act), forageThreshold, moraleLow, moraleHigh)
						if mineBase != theirBase {
							t.Fatalf("基础分镜像漂移：act=%s h=%d m=%.2f 本包=%.4f session=%.4f", act, h, m, mineBase, theirBase)
						}
						// 野心乘权（受 flag 门控）。
						mineW := ambitionActionWeight(rec, act)
						theirW := session.AmbitionActionWeight(rec, session.ActionAmbitionTagForAmbient(string(act)))
						if mineW != theirW {
							t.Fatalf("野心乘权镜像漂移：flag=%q act=%s amb=%+v 本包=%.4f session=%.4f", flag, act, amb, mineW, theirW)
						}
					}
				}
			}
		}
	}
}
