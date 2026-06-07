# 假门测试落地页

对应 [`docs/验证实验设计.md`](../docs/验证实验设计.md) §3 的步骤①（假门测试）。单文件、零构建、可直接部署到任意静态托管（Netlify / Vercel / GitHub Pages / OSS / nginx）。

## 文件
- `index.html` — 三版 A/B 落地页 + 留资表单 + 留资后弹出的卖点问卷（Q1–Q5）+ 埋点，全部内联在一个文件里。

## 三个 A/B 变体（同一文件，按 `?src=` 切换）

把三个广告系列分别投向：

| 变体 | URL | 卖点切入 |
|---|---|---|
| A 版 | `https://你的域名/?src=autonomy` | 角色自治 ·「它会自己活，也会违背你」 |
| B 版 | `https://你的域名/?src=story` | 读故事 + 介入命运 ·「会写自传的角色」 |
| C 版 | `https://你的域名/?src=world` | 共享世界 ·「所有人的角色活在同一世界」 |

无 `src` 参数时默认 A 版。`utm_source/medium/campaign/content` 会被一并采集用于归因。

## 上线前两处配置（`index.html` 顶部 `CONFIG`）
1. `formEndpoint`：留资与问卷的 POST 目标。
   - 留空 = **本地模式**（数据存浏览器 `localStorage`，并打到 `dataLayer`/`console`）——可立即跑通流程、做演示。
   - 接 [Formspree](https://formspree.io)：填 `https://formspree.io/f/xxxxxxx`；或自建 `'/api/leads'`。
2. `gaMeasurementId`：填 GA4 的 `G-XXXX` 即自动加载 gtag；若页面已注入 GTM/gtag 会自动复用，可留空。

## 埋点事件（转化口径）
| 事件 | 含义 |
|---|---|
| `page_view` | 落地页曝光（带 `variant`/`src`/`utm_*`） |
| `cta_click` | 主/次 CTA 点击（次 CTA「看演示」本身是强意向信号） |
| **`lead_submit`** | **留资转化 ← §3.5 北极星指标**（带 `contact_type`：email/phone/wechat） |
| `survey_view` / `survey_submit` | 问卷曝光/提交（`survey_submit` 携带 `q1/q2/q3/q5` 答案） |

每个事件都带稳定的访客 `vid` 与归因字段，方便按 `src` 拆转化率给卖点排序。

## 判读阈值（§3.5）
- 留资转化率：`<8% 砍 / 8–15% 谨慎 / >15% 继续`。
- 问卷 Q1 选「角色离线自己活」相关（选项 A+B）`<30%` → 卖点不抓人（红灯）。
- 问卷 Q3 的 D+E 占比是致命假设 H2 的廉价前测。

## 本地预览
```bash
cd landing
python3 -m http.server 8000
# 浏览器打开 http://localhost:8000/?src=autonomy （或 story / world）
```
本地模式下，留资与问卷数据可在浏览器控制台执行 `JSON.parse(localStorage.qx_lead)` / `localStorage.qx_survey` / `localStorage.qx_events` 查看。
