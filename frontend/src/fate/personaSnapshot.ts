/* 文件说明：即时人格快照——「15 秒人生快进微选择」的纯逻辑层（GDD O2「最高 ROI：压缩快进微选择」）。
   在角色降生前（preview 阶段），据 persona 八轴 + 出身确定性地拣选 2-3 个情境微抉择；
   玩家点选后，把「这一选择折射出的人格侧写」用既有 persona 轴映射成即时反馈，
   收尾再合成一句「这就是她」的人格速写。全程纯前端、确定性、零持久化——只为第一分钟的「她是个什么样的人」即时感知。

   设计要点：
   - 不引入任何随机：拣题用 (unitId 哈希 + 轴值) 排序，可复现。
   - 每个选项预绑一条「侧写」文案 + 一组它最契合的轴（用于和该角色实际 persona 比对：契合即「她正是这样」，相左即「但这一次，她出乎你意料」）。
   - 不写后端、不改 persona——纯体验层。若日后要持久化/反哺 persona，见 FateApp.tsx 顶部 crossFileNeeds 注释。 */

// PersonaTraits 是 preview 阶段从 bootstrap 单位 personality 块取到的八轴（[0.05,0.95] 浮点）。
export type PersonaTraits = {
  courage: number;
  loyalty: number;
  aggression: number;
  prudence: number;
  sociability: number;
  integrity: number;
  stability: number;
  ambition: number;
};

// TraitKey 是八轴键名。
export type TraitKey = keyof PersonaTraits;

const TRAIT_KEYS: TraitKey[] = [
  "courage",
  "loyalty",
  "aggression",
  "prudence",
  "sociability",
  "integrity",
  "stability",
  "ambition",
];

const TRAIT_LABEL: Record<TraitKey, string> = {
  courage: "胆识",
  loyalty: "忠义",
  aggression: "锋芒",
  prudence: "审慎",
  sociability: "亲和",
  integrity: "节操",
  stability: "沉稳",
  ambition: "野心",
};

// MicroOption 是一个微抉择选项：玩家点它，即折射出 traits 里这几轴的倾向。
export type MicroOption = {
  id: string;
  // 玩家看到的行动文案。
  label: string;
  // 这个选择契合哪几轴的「高」（leansHigh）或「低」（leansLow）。
  leansHigh: TraitKey[];
  leansLow: TraitKey[];
  // 选后即时折射的人格侧写（一句话）。
  reflection: string;
};

// MicroChoice 是一道情境微抉择：一段情境 + 2 个对立选项。
export type MicroChoice = {
  id: string;
  // 情境一句话（「路遇乞儿…」）。
  scene: string;
  options: [MicroOption, MicroOption];
};

// SnapshotResult 是整段快照的产物：玩家每题选了什么，外加收尾人格速写。
export type SnapshotResult = {
  // 命中度（0..1）：玩家所选与该角色 persona 倾向的契合比例，用于收尾措辞。
  alignment: number;
  // 一句「这就是她」的人格速写。
  verdict: string;
  // 这段速写里最突出的两轴标签（如「胆识 · 节操」），供 UI 高亮。
  signature: string;
};

// FNV-1a 32 位哈希（与仓库确定性随机风格一致，不用全局 rand）。
function hash32(s: string): number {
  let h = 0x811c9dc5;
  for (let i = 0; i < s.length; i += 1) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return h >>> 0;
}

// CHOICE_POOL 是题库：每题预绑两条对立选项及其折射轴与侧写。
// 拣题策略——优先选「该角色在这道题的两轴上分歧最大」的题，让选择更能逼出人格。
const CHOICE_POOL: MicroChoice[] = [
  {
    id: "beggar",
    scene: "城门口，一个褴褛乞儿伸手向你乞食。你怀里只剩半块干粮。",
    options: [
      {
        id: "give",
        label: "掰一半给他",
        leansHigh: ["integrity", "sociability"],
        leansLow: ["aggression"],
        reflection: "她下意识掰开了干粮——心软，藏不住。",
      },
      {
        id: "keep",
        label: "攥紧干粮快步走开",
        leansHigh: ["prudence", "ambition"],
        leansLow: ["sociability"],
        reflection: "她没停步——活下去，先得顾自己。",
      },
    ],
  },
  {
    id: "brawl",
    scene: "酒肆里，邻桌醉汉掀翻了你的酒碗，还出言挑衅。",
    options: [
      {
        id: "fight",
        label: "拍案而起，硬碰硬",
        leansHigh: ["courage", "aggression"],
        leansLow: ["prudence"],
        reflection: "她的拳头比脑子快——惹不得。",
      },
      {
        id: "swallow",
        label: "默默换张桌子",
        leansHigh: ["prudence", "stability"],
        leansLow: ["aggression"],
        reflection: "她咽下了这口气——忍，是她的本事。",
      },
    ],
  },
  {
    id: "purse",
    scene: "巷口，你看见地上掉了一只鼓囊囊的钱袋，四下无人。",
    options: [
      {
        id: "take",
        label: "捡起揣进怀里",
        leansHigh: ["ambition", "aggression"],
        leansLow: ["integrity"],
        reflection: "她不会跟到手的好处过不去。",
      },
      {
        id: "wait",
        label: "守在原地等失主",
        leansHigh: ["integrity", "loyalty"],
        leansLow: ["ambition"],
        reflection: "不是她的，她一文都不要。",
      },
    ],
  },
  {
    id: "stranger",
    scene: "雨夜，一个素不相识的旅人敲门，求借一宿。",
    options: [
      {
        id: "open",
        label: "请进，添一副碗筷",
        leansHigh: ["sociability", "courage"],
        leansLow: ["prudence"],
        reflection: "她对陌生人也敞着门——天真，或是良善。",
      },
      {
        id: "refuse",
        label: "隔门道句歉，不开",
        leansHigh: ["prudence", "stability"],
        leansLow: ["sociability"],
        reflection: "她把门栓得很紧——防人之心，她有。",
      },
    ],
  },
  {
    id: "oath",
    scene: "同伴起事在即，要你歃血为盟、生死与共。可此事九死一生。",
    options: [
      {
        id: "swear",
        label: "割掌为誓，绝不回头",
        leansHigh: ["loyalty", "courage"],
        leansLow: ["prudence"],
        reflection: "答应了的事，她拿命也认。",
      },
      {
        id: "hedge",
        label: "嘴上应着，心里留一手",
        leansHigh: ["prudence", "ambition"],
        leansLow: ["loyalty"],
        reflection: "她从不把后路堵死——精明得很。",
      },
    ],
  },
  {
    id: "spotlight",
    scene: "众人推举你做这趟差事的头领，担子重，风头也最盛。",
    options: [
      {
        id: "lead",
        label: "当仁不让，接过来",
        leansHigh: ["ambition", "courage"],
        leansLow: ["stability"],
        reflection: "她想往高处走——压不住的那股劲。",
      },
      {
        id: "yield",
        label: "退后半步，让旁人来",
        leansHigh: ["stability", "prudence"],
        leansLow: ["ambition"],
        reflection: "她不爱站在最亮的地方——稳当就好。",
      },
    ],
  },
];

// optionFit 衡量某选项契合该角色 persona 的程度（[-1,1]）：
// 选项偏「高」的轴若角色确实偏高、偏「低」的轴若角色确实偏低，则分高。
// 导出供超时自动拣选用（拣「更契合她的那个」，兑现「她会自己活」）。
export function optionFit(opt: MicroOption, traits: PersonaTraits): number {
  let sum = 0;
  let n = 0;
  for (const k of opt.leansHigh) {
    sum += traits[k] - 0.5;
    n += 1;
  }
  for (const k of opt.leansLow) {
    sum += 0.5 - traits[k];
    n += 1;
  }
  if (n === 0) return 0;
  // /0.45 把 [0.05,0.95] 偏离 0.5 的 ±0.45 拉到约 ±1。
  return Math.max(-1, Math.min(1, sum / n / 0.45));
}

// choiceDivergence 是一道题对该角色的「区分度」：两选项契合度之差的绝对值。
// 差越大，说明该角色在这道题上倾向越鲜明，越值得问。
function choiceDivergence(choice: MicroChoice, traits: PersonaTraits): number {
  const a = optionFit(choice.options[0], traits);
  const b = optionFit(choice.options[1], traits);
  return Math.abs(a - b);
}

// pickChoices 据角色 persona + 出身 + unitId 确定性地拣选 count（默认 3）道最具区分度的微抉择。
// 同区分度时用 (unitId+题 id) 哈希打破平手，保证「同一个她」每次拣到的题一致。
export function pickChoices(traits: PersonaTraits, seed: string, count = 3): MicroChoice[] {
  const ranked = [...CHOICE_POOL]
    .map((c) => ({
      choice: c,
      div: choiceDivergence(c, traits),
      tie: hash32(`${seed}:${c.id}`),
    }))
    .sort((x, y) => {
      if (y.div !== x.div) return y.div - x.div;
      return x.tie - y.tie;
    });
  return ranked.slice(0, Math.max(1, Math.min(count, CHOICE_POOL.length))).map((r) => r.choice);
}

// dominantTraits 返回该角色最突出的 n 轴（离 0.5 最远者），用于收尾速写的签名标签。
function dominantTraits(traits: PersonaTraits, n: number): TraitKey[] {
  return [...TRAIT_KEYS]
    .sort((a, b) => Math.abs(traits[b] - 0.5) - Math.abs(traits[a] - 0.5))
    .slice(0, n);
}

// VERDICT_HIGH / VERDICT_LOW 是各轴「偏高/偏低」时的速写碎句，用于合成「这就是她」。
const VERDICT_HIGH: Record<TraitKey, string> = {
  courage: "敢闯",
  loyalty: "重情",
  aggression: "好强",
  prudence: "审慎",
  sociability: "热络",
  integrity: "守正",
  stability: "沉得住气",
  ambition: "有野心",
};
const VERDICT_LOW: Record<TraitKey, string> = {
  courage: "怕事",
  loyalty: "凉薄",
  aggression: "温吞",
  prudence: "莽撞",
  sociability: "孤僻",
  integrity: "不拘小节",
  stability: "易动摇",
  ambition: "随遇而安",
};

// traitPhrase 据某轴高低取速写碎句。
function traitPhrase(k: TraitKey, traits: PersonaTraits): string {
  return traits[k] >= 0.5 ? VERDICT_HIGH[k] : VERDICT_LOW[k];
}

// summarize 据玩家在各题的实际所选与该角色 persona 的契合度，合成收尾人格速写。
// picks: 每题玩家选的 option（按 pickChoices 的题序）。
export function summarize(
  traits: PersonaTraits,
  name: string,
  picks: MicroOption[],
): SnapshotResult {
  // alignment：玩家所选平均契合度（[-1,1]）归一到 [0,1]。
  let fitSum = 0;
  for (const opt of picks) {
    fitSum += optionFit(opt, traits);
  }
  const avgFit = picks.length > 0 ? fitSum / picks.length : 0;
  const alignment = Math.max(0, Math.min(1, (avgFit + 1) / 2));

  const top = dominantTraits(traits, 2);
  const signature = top.map((k) => TRAIT_LABEL[k]).join(" · ");
  const phrases = top.map((k) => traitPhrase(k, traits));

  // 据契合度选不同口吻：选得很贴她 → 「你看得很准」；选得相左 → 「她出乎你意料」。
  let lead: string;
  if (alignment >= 0.62) {
    lead = `你看得很准——${name}`;
  } else if (alignment <= 0.38) {
    lead = `她比你想的更难捉摸——${name}骨子里`;
  } else {
    lead = `${name}是这样一个人`;
  }
  const verdict = `${lead}：${phrases.join("、")}。这就是她。`;

  return { alignment, verdict, signature };
}

// fromPersonalityBlock 把 bootstrap 单位的 personality 任意对象安全地夹成 PersonaTraits（缺轴默认 0.5）。
export function fromPersonalityBlock(raw: unknown): PersonaTraits {
  const obj = (raw && typeof raw === "object" ? (raw as Record<string, unknown>) : {}) as Record<
    string,
    unknown
  >;
  const read = (k: TraitKey): number => {
    const v = obj[k];
    const n = typeof v === "number" && Number.isFinite(v) ? v : 0.5;
    return Math.max(0, Math.min(1, n));
  };
  return {
    courage: read("courage"),
    loyalty: read("loyalty"),
    aggression: read("aggression"),
    prudence: read("prudence"),
    sociability: read("sociability"),
    integrity: read("integrity"),
    stability: read("stability"),
    ambition: read("ambition"),
  };
}
