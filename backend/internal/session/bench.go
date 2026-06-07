package session

// 文件说明：成本基准用的代表性决策请求（供 cmd/costbench 测真实 LLM 单次决策成本，验证 §11.6 W0 预实验 part①）。
// system 用真实的静态决策 prompt（M1.3 前缀缓存的核心大块）；user 用一段代表性长度的上下文；
// schema 用代表性的决策输出结构。如此测出的 token/成本贴近线上一次单位决策。

import "qunxiang/backend/internal/ai"

// BenchDecisionRequest 返回一个代表性的单位决策 CompletionRequest（不依赖真实会话/单位，便于离线基准）。
func BenchDecisionRequest() ai.CompletionRequest {
	return ai.CompletionRequest{
		Task:           ai.TaskUnitDecision,
		SchemaName:     "session_unit_decision",
		ResponseSchema: benchDecisionSchema,
		SystemPrompt:   unitDecisionSystemPrompt(),
		UserPrompt:     benchRepresentativeUserPrompt,
		Temperature:    0.35,
		MaxTokens:      220,
		Timeout:        llmRequestTimeout,
	}
}

// benchDecisionSchema 是一份代表性的决策输出 JSON Schema（与线上结构同量级）。
var benchDecisionSchema = []byte(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "action": {"type": "string", "enum": ["hold", "move", "gather", "eat", "engage", "talk", "trade", "assist"]},
    "target_q": {"type": "integer"},
    "target_r": {"type": "integer"},
    "target_unit_id": {"type": "string"},
    "speak": {"type": "string"},
    "reason": {"type": "string"}
  },
  "required": ["action", "speak", "reason"]
}`)

// benchRepresentativeUserPrompt 是一段代表性长度的单位决策 user prompt（贴近 buildDecisionPrompt 的结构与体量）。
const benchRepresentativeUserPrompt = `单位决策提示词版本: action_params_v4
你的身份: 名称=阿采；ID=u_8f3a；姓名=林采薇；昵称=采儿；阵营=player
当前回合: 14
当前阶段: execution
你本次可用 AP: 2
MOVE 坐标白名单: (3,4) (3,5) (4,4)。一个格子只能站一个单位。
你所属阵营: player
当前抗命标记(defiant): false
阵营自然语言方针上下文: 稳住北线，优先救援受伤的同伴，不要恋战。
你的资料: HP=62/100，饥饿=44，士气=0.6，攻击=18，防御=7，位置=(3,4)，状态=轻伤。
你的家庭关系: 与「周南」是青梅竹马（亲密度高），与「赵无咎」有旧怨。
你的性格: 勇敢偏高、谨慎中等、对弱者心软、厌恶背叛。
你的生平: 出身边境猎户，父母死于一场劫掠，自幼习武，立志护住身边人。
你的环境摘要: 西侧两格有一名濒死的友军「周南」(HP=8)，东侧三格有一名敌军弓手正在逼近。
你记得的重点:
- 上一回合你答应过要护住周南。
- 三回合前赵无咎在你背后放了冷箭。
你与周围单位的关系: 周南(信任+8,亲密+7)；赵无咎(敌意+6,信任-5)。
你掌握的世界知识: 北境冬季补给紧张；弓手在开阔地威胁大。
候选动作:
1) assist target_unit_id=u_zhou 救援濒死友军
2) move target_q=3 target_r=5 向西靠近周南
3) engage target_unit_id=u_archer 迎击敌方弓手
4) eat 进食恢复饥饿
请综合人设、记忆、关系、当前威胁与方针，决定本执行阶段做的一件事，并用一句话说出她此刻会说的话。`
