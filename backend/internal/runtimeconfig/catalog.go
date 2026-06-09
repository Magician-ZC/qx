package runtimeconfig

// 文件说明：可运营参数白名单（catalog）。集中登记全部「可运行时调」的平衡常量、阈值、节奏与 LLM 热切参数。
// 各域代码把原硬编码 const 的读取站点换成 runtimeconfig.GetFloat/GetInt/GetString/GetEnum(name)，
// spec.Default 严格等于原 const 值 —— GM 未动手前行为逐位不变（迁移的核心保证）。
//
// 命名约定：小写点分 namespace 前缀（combat./memory./fate./threat./obedience./social./faction./anchor./llm.）。
// 反 P2W 红线：llm.* 是**全局单值**热切（GM 运营），绝不接入按任务重要度的 tier-routing 分档 —— 付费买不到更强模型。
//
// init() 在包加载时自动注册（任何 import runtimeconfig 的包都会触发），保证 session/regionrunner 等读取站点
// 在进程内恒有 spec 兜底。重复注册 panic（启动即暴露编程错误）。

func init() {
	Register(
		// —— 战斗 / ATB —— //
		ParamSpec{Name: "combat.atb_momentum_penalty", Namespace: "combat", Type: TypeFloat, Default: "0.85",
			Min: Ptr(0.5), Max: Ptr(1.0), HotReload: true,
			Description: "同阵营连续行动的势头惩罚系数（越低惩罚越重，越鼓励轮换出手）。热循环：每 tick×每 actor。"},

		// —— 记忆衰减 —— //
		ParamSpec{Name: "memory.decay_tau_turns", Namespace: "memory", Type: TypeFloat, Default: "120",
			Min: Ptr(30), Max: Ptr(365), HotReload: true,
			Description: "记忆显著度指数衰减时间常数 tau（回合）。越大记忆留存越久。"},
		ParamSpec{Name: "memory.decay_alpha", Namespace: "memory", Type: TypeFloat, Default: "2.5",
			Min: Ptr(1.0), Max: Ptr(5.0), HotReload: true,
			Description: "记忆衰减曲线陡度 alpha。越大近因偏置越强。"},

		// —— 命运 / 收件箱 / 路由 —— //
		ParamSpec{Name: "fate.pending_daily_budget", Namespace: "fate", Type: TypeInt, Default: "3",
			Min: Ptr(1), Max: Ptr(10), HotReload: true,
			Description: "每角色每日待决策收件箱上限（直控玩家每日被打扰的决策量）。"},
		ParamSpec{Name: "fate.serendipity_daily_budget", Namespace: "fate", Type: TypeInt, Default: "1",
			Min: Ptr(0), Max: Ptr(5), HotReload: true,
			Description: "每日破圈/惊喜（零锚事件升档作新锚种子）预算。0=关闭破圈。"},
		ParamSpec{Name: "fate.irreversible_attachment_gate", Namespace: "fate", Type: TypeFloat, Default: "70",
			Min: Ptr(0), Max: Ptr(100), HotReload: true,
			Description: "不可逆后果的牵挂解锁线：牵挂高于此值即拒绝替玩家自动放手、强制玩家亲自决策。"},
		ParamSpec{Name: "fate.urge_cost_max_scale", Namespace: "fate", Type: TypeFloat, Default: "2.0",
			Min: Ptr(1.0), Max: Ptr(5.0), HotReload: true,
			Description: "越界渴望在满牵挂时的 loyalty 代价最大放大倍数。"},
		ParamSpec{Name: "fate.irreversibility_floor", Namespace: "fate", Type: TypeFloat, Default: "0.70",
			Min: Ptr(0), Max: Ptr(1), HotReload: true,
			Description: "不可逆事件单因子退化兜底强度（防误杀本该牵动她的事，越高命运路由越灵敏）。"},
		ParamSpec{Name: "fate.emotion_floor", Namespace: "fate", Type: TypeFloat, Default: "0.70",
			Min: Ptr(0), Max: Ptr(1), HotReload: true,
			Description: "情感事件单因子退化兜底强度。"},

		// —— 威胁刷新 —— //
		ParamSpec{Name: "threat.spawn_floor", Namespace: "threat", Type: TypeFloat, Default: "0.05",
			Min: Ptr(0), Max: Ptr(0.3), HotReload: true,
			Description: "野外威胁出没概率下限（世界基础凶险度）。"},
		ParamSpec{Name: "threat.spawn_cap", Namespace: "threat", Type: TypeFloat, Default: "0.55",
			Min: Ptr(0.1), Max: Ptr(1.0), HotReload: true,
			Description: "野外威胁出没概率上限（威胁度拉满时的封顶频率）。"},
		ParamSpec{Name: "threat.weight_level", Namespace: "threat", Type: TypeFloat, Default: "0.5",
			Min: Ptr(0), Max: Ptr(1), HotReload: true,
			Description: "威胁选址权重：region 威胁度项（越高越看地方凶险度）。"},
		ParamSpec{Name: "threat.weight_anchor", Namespace: "threat", Type: TypeFloat, Default: "0.3",
			Min: Ptr(0), Max: Ptr(1), HotReload: true,
			Description: "威胁选址权重：玩家锚密度项（越高越在玩家在乎的人/地刷威胁）。"},
		ParamSpec{Name: "threat.weight_freshness", Namespace: "threat", Type: TypeFloat, Default: "0.2",
			Min: Ptr(0), Max: Ptr(1), HotReload: true,
			Description: "威胁选址权重：新鲜度项（防同目标短期扎堆）。"},
		ParamSpec{Name: "threat.freshness_window_turns", Namespace: "threat", Type: TypeInt, Default: "6",
			Min: Ptr(1), Max: Ptr(50), HotReload: true,
			Description: "同一目标多久内压低再出威胁的窗口（回合）。"},

		// —— 服从 / 抗命 —— //
		ParamSpec{Name: "obedience.forced_defiance_threshold", Namespace: "obedience", Type: TypeFloat, Default: "0.3",
			Min: Ptr(0), Max: Ptr(1), HotReload: true,
			Description: "角色对即时令的抗命敏感度阈值（越低越易抗命）。"},
		ParamSpec{Name: "obedience.offline_caution_ceiling", Namespace: "obedience", Type: TypeFloat, Default: "4.5",
			Min: Ptr(1.0), Max: Ptr(10.0), HotReload: true,
			Description: "离线托管越久越保守的封顶（胆量曲线上界）。"},

		// —— 社交 / 撮合 / 血仇 —— //
		ParamSpec{Name: "social.blood_feud_rivalry_gate", Namespace: "social", Type: TypeFloat, Default: "4.0",
			Min: Ptr(0), Max: Ptr(10), HotReload: true,
			Description: "多高敌意才算世仇（影响血仇沿关系图传播范围）。"},
		ParamSpec{Name: "social.auto_match_npc_score", Namespace: "social", Type: TypeFloat, Default: "0.30",
			Min: Ptr(0), Max: Ptr(1), HotReload: true,
			Description: "NPC 兜底成员在撮合里的分量（防 NPC 抢真人名次）。"},
		ParamSpec{Name: "social.auto_match_daily_bind_cap", Namespace: "social", Type: TypeInt, Default: "2",
			Min: Ptr(0), Max: Ptr(20), HotReload: true,
			Description: "每日自动撮合绑定上限（反大 R 垄断社交）。"},
		ParamSpec{Name: "social.auto_match_every_n_turns", Namespace: "social", Type: TypeInt, Default: "4",
			Min: Ptr(1), Max: Ptr(50), HotReload: true,
			Description: "自动撮合周期（回合）。注意：同时影响 NPC backfill 的确定性分桶，改后撮合节奏即时变、不回溯。"},

		// —— 阵营 / 出生点 —— //
		ParamSpec{Name: "faction.ambient_wander_move_chance", Namespace: "faction", Type: TypeFloat, Default: "0.30",
			Min: Ptr(0), Max: Ptr(1), HotReload: true,
			Description: "出生点公共 NPC 每回合边界游走概率（命运地图舞台活跃度）。"},
		ParamSpec{Name: "faction.moral_jitter", Namespace: "faction", Type: TypeFloat, Default: "6.0",
			Min: Ptr(0), Max: Ptr(30), HotReload: true,
			Description: "同阵营 NPC 道德轴离散度。改此值会改变确定性生成结果（同 seed 不同 jitter→不同道德轴）。"},

		// —— 相关性锚 —— //
		ParamSpec{Name: "anchor.geo_half_life_days", Namespace: "anchor", Type: TypeFloat, Default: "3",
			Min: Ptr(0.5), Max: Ptr(30), HotReload: true,
			Description: "地理锚（进新 region）半衰期（天）。越短越快淡忘他乡。"},
		ParamSpec{Name: "anchor.density_saturation", Namespace: "anchor", Type: TypeFloat, Default: "1.5",
			Min: Ptr(0.5), Max: Ptr(5), HotReload: true,
			Description: "锚密度饱和系数（控锚密度对相关性/威胁选址的边际递减）。"},
		ParamSpec{Name: "anchor.default_half_life_days", Namespace: "anchor", Type: TypeFloat, Default: "14",
			Min: Ptr(1), Max: Ptr(365), HotReload: true,
			Description: "默认锚半衰期（天）。"},

		// —— LLM 全局热切（反 P2W：全局单值，绝不进 tier-routing 付费分档） —— //
		ParamSpec{Name: "llm.model", Namespace: "llm", Type: TypeString, Default: "", HotReload: true,
			Description: "全局 LLM 模型热切覆盖（空=用各端点配置默认模型，不覆盖；仅对未硬编码端点模型的 provider 生效）。"},
		ParamSpec{Name: "llm.reasoning_effort", Namespace: "llm", Type: TypeEnum, Default: "",
			Values: []string{"", "minimal", "low", "medium", "high"}, HotReload: true,
			Description: "全局 reasoning effort 热切覆盖（空=不覆盖端点配置）。"},
		ParamSpec{Name: "llm.timeout_seconds", Namespace: "llm", Type: TypeInt, Default: "0",
			Min: Ptr(0), Max: Ptr(180), HotReload: true,
			Description: "全局 LLM 超时秒热切覆盖（0=不覆盖，沿用 profile；>0 时夹 [60,180]）。"},
	)
}
