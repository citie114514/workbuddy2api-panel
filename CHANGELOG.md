# 更新日志（继任维护）

本文件是 GitHub Release 正文的数据源：打 `v*` tag 发版时，CI 自动提取对应章节写入发行说明。
格式：`## vX.Y.Z（日期）` 起头，下一个 `## ` 结尾。新版本发版前在文件顶部追加章节。

## v1.16.2（2026-11-01）

### 修复：不带 max_tokens 的客户端被上游输出预留顶成 11115（昔涟 / ai-sdk 长会话 400）

上游的上下文校验口径是 **`prompt_tokens` + 输出预留 > 模型窗口**，不是只看输入；预留取值随请求体是否显式带 `max_tokens` 而变：

- 显式带（且不超过模型上限）→ 预留就是该值；
- 没带（或带了个被上游静默换掉的大值）→ 预留取模型默认值。**实测 `global:deepseek-v4.1-flash` 384000、`cn:deepseek-v4.1-flash` 128000**（`prompt is too long: 868040 tokens > 1048576 maximum` 里的 868040 就是「真实输入 + 384000 预留」）。

于是同一份长篇会话：pi / Claude Code / dsh 都显式发 `max_tokens`，预留被压到几万，正常通过；昔涟（Cyrene，走 ai-sdk 的 OpenAI 兼容路径）不发 `max_tokens`，凭空背上 384k 预留，明明还有几十万 token 可用也被硬顶成 400 `prompt_too_long`。

修复（只在已经 400 的路径上动作，正常请求零影响）：

- 解析 11115 报文里的 `used`（= prompt + 预留）与 `limit`，**不估算、不猜**：客户端显式声明 ≤32768 的输出上限时直接反推 `prompt = used - max_tokens` 一次算准；否则先补一个明确的 32768 预留重发，下一跳精确收敛
- 最多重发 2 次且**不吃 `MaxRotate` 预算**；只收窄输出上限、不改任何输入内容、不罚号（11115 本来就是请求侧问题）
- 收窄后仍越界 = 输入本身贴顶，**原样透传上游原文**（真实 token 数 + 窗口上限），客户端据此压缩上下文或换更大窗口的模型
- 日志留痕：`prompt_too_long repair #N model=… -> max_tokens=… (prompt+reserve=… limit=…)`

实测对照（同一 868040 token 的请求）：修复前 global 域稳定 400，修复后自动按剩余窗口作答并成功返回；1.2M 的真超限请求仍如实透传 400。

## v1.16.1（2026-10-27）

### 修复：工具结果里的图片被字符串化 / 丢弃（Codex 看图成本修复）

- **/v1/responses**：`function_call_output` 的 `output` 为数组时，此前整体 `json.Marshal` 成文本 —— 图片 data URL 变成百万 token 级的文本垃圾、模型根本看不到图。现在逐块分流：`input_image` / `input_image_url` 片段按图片区块透传（data URL 入 `image_url`，`image_url` 的 string / `{url}` 对象 / 顶层 `url` 键三种形态都收），`input_text` / `output_text` 转 text，未知片段按 JSON 文本兜底；字符串形态零回归
- **/v1/messages**：`tool_result` 里的图片块（base64 source）此前被静默丢弃（Claude Code 截图类工具结果模型看不到），现在同样转 `image_url` data URL 透传；单一文本块保持裸字符串折叠

效果：Codex / Claude Code 看图成本从约百万 token/张降到正常图片计量。

## v1.16.0（2026-10-25）

### 修复：/v1/responses 与 /v1/messages 透出思维链与缓存计量（计量失真修复）

三路对照实测（同一网关同一模型）：chat 路径明明白白报 `reasoning_content`、`completion_thinking_tokens`、`prompt_cache_hit_tokens`，而 Responses / Anthropic 两条适配路径把 reasoning 与缓存字段全部吞掉 —— `reasoning_tokens` 恒 0、`cached_tokens` 恒 0，Codex / Claude Code 里看到的用量与成本偏小，DeepSeek 前缀缓存省下的钱完全看不见。上游把一切都给了，是适配层翻译 schema 时漏字段。

**/v1/messages（Anthropic）**：

- 思维链以 thinking 块透出：`content_block_start {type:"thinking"}` + `thinking_delta` 增量，关块补 `signature_delta`（空串占位 —— 入站侧丢弃 thinking，无可验签名场景）；text / thinking / tool_use 三类块互斥自动切换
- `usage.cache_read_input_tokens` ← `prompt_cache_hit_tokens`、`usage.cache_creation_input_tokens` ← `prompt_cache_write_tokens`（流式随 `message_delta` 下发，非流式随顶层 usage）
- `output_tokens` 透传 `completion_tokens`（实测口径 total = prompt + completion，thinking 已计入，无需另加）

**/v1/responses（OpenAI Responses）**：

- `output[]` 补 `{"type":"reasoning","summary":[{"type":"summary_text",...}]}`（思维链原样作为 summary 透出）；流式对应 `reasoning_summary_part.added → reasoning_summary_text.delta → …done` 事件序列；**是否输出由请求的 `reasoning.summary` 决定**（`none` / 缺省不发，对齐官方语义）
- `usage.output_tokens_details.reasoning_tokens` ← `completion_tokens_details.reasoning_tokens`
- `usage.input_tokens_details.cached_tokens` ← `prompt_cache_hit_tokens`

**/v1/chat/completions** 不动（已经是全的）。适配层新增请求级状态（`forRequest`）：stream 翻译器与非流式翻译共享请求的 summary 设置。

## v1.15.0（2026-10-25）

### 模型付费回退：免费额度用尽自动切付费兄弟型号（默认开启）

WorkBuddy 国际版的免费模型（如 `global:deepseek-v4.1-flash`，x0.00）额度用尽后，上游报 6004 模型级限流；官方客户端此时会自动切到同名付费兄弟型号（`deepseek-v4.1-flash-sg`，x0.03）继续可用。此前网关没有这个回退：免费额度一耗尽，所有全球号被模型级锁住（6004），请求直接失败到上游重置。

本次对齐官方行为：

- 选号时若整个池子对某模型模型级阻塞（6004 这类），且上游目录存在 `<model>-sg` 付费兄弟型号，自动改用回退型号重选一次；响应的 `model` 字段如实回显实际服务的型号，计费按回退型号的真实倍率
- 只回退一次；回退型号也被锁时按既有 `400 model_unavailable` 口径收场，不做 `-sg-sg` 链式递归
- 请求流水行的模型列、用量与成本账本均按实际服务的回退型号记录

配置（面板保存后热生效）：

- `pool.model_fallback_enabled`：默认 `true`
- `pool.model_fallback`：自定义映射，键 `"realm:model"`、值同域目标裸模型名；值空串或指向自身 = 禁用该模型的回退；未命中的模型仍走内置 `-sg` 规则

已在真实环境验证：免费版被 6004 全池锁住时请求自动落到 `-sg`、日志留痕（`model fallback model=X -> Y`）、余额按 `-sg` 倍率扣减。

**升级说明**：无破坏性变更，默认开启；不想要回退行为时把 `pool.model_fallback_enabled` 设为 `false`。

## v1.14.2（2026-10-25）

### 修复：SSE 负载缺 `data:` 前缀（Codex 全事件被丢弃的根因）

`/v1/responses` 与 `/v1/messages` 的 SSE 事件此前把 JSON 裸写在 `event:` 行后（无 `data:` 前缀）。按 SSE 规范（WHATWG），裸行会被 eventsource 解析器当未知字段丢弃、事件以**空 data** 派发 —— curl 与子串断言都测不出，只有真实 eventsource 客户端暴露：codex 0.164 实测 `stream disconnected before completion: stream closed before response.completed`。经 socat 抓包 + 对照 codex-rs 源码（`ResponsesStreamEvent` / `see_event_completed_failed`）定位。

- 出站事件负载统一写入 `data:` 字段；流式端到端测试补 `event: …\ndata: ` 断言防回归
- Anthropic 适配层容忍 `messages` 里 `role:"system"`（Claude Code 实测形态，尽管规范要求 system 走顶层参数）

修复后容器内真实 `claude`（2.1.309）与 `codex`（0.164.0）CLI 双双端到端跑通。

## v1.14.1（2026-10-25）

- `/v1/models` 接受 `x-api-key` 鉴权（Anthropic SDK 标准形态），与 Bearer 等效
- 鉴权失败的错误形状语义固定：`/v1/messages` 系恒 Anthropic 形状（Claude Code 不总发 `anthropic-version` 头，不能拿头的有无判协议）；形状分流仅用于 `/v1/models` 这一双协议共享端点

## v1.14.0（2026-10-25）

### 多协议端点：Codex / Claude Code 直连（无需外层协议转换器）

- **`POST /v1/responses`**（OpenAI Responses）：instructions / input 项 / function 工具 / `reasoning.effort` / `max_output_tokens`；流式事件 `response.created → output_text.delta → function_call_arguments.delta → response.completed`（含 usage），error 帧 → `response.failed`
- **`POST /v1/messages`**（Anthropic Messages）：system 块 / 文本与图片 / `tool_use`↔`tool_result` / `tool_choice`（`any`→`required`）/ `stop_sequences`；流式 `message_start → content_block_delta(text/input_json_delta) → message_stop`；`x-api-key` 与 Bearer 双鉴权形态
- `POST /v1/messages/count_tokens`：本地粗估（chars/4），不打上游
- `/v1/models` 带 `anthropic-version` 头时返回 Anthropic 列表形状

架构：请求翻译成 chat 形态走既有 `chatCompletions` 管道，`ResponseWriter` 包装器把出站实时翻译回目标协议 —— 选号 / 会话粘性 / 熔断冷却 / 用量统计 / 原生工具标记修复零改动全继承。模型名不映射，客户端需使用网关模型名（`cn:` / `global:` 前缀）。

## v1.13.1（2026-10-25）

### 修复：账号池表格整表空白（面板）

`app.js` 的 `rateLimitMeta` 双时钟分支（`until < reset_at`，issue #171 问题二引入）对未声明变量 `detail` 赋值；文件顶部 `'use strict'` 下抛 `ReferenceError`，`renderAccounts` 在 tbody 赋值前中断，异常又被 `loadOverview` 的静默 catch 吞掉 —— 表现为统计卡正常、账号表格空白、无任何报错。任一账号挂 6004 模型级限流且命中双时钟条件即复现。

- 补 `let detail;` 声明
- `TestAppJSRateLimitMeta` 测试沙箱补 `'use strict'` 前缀对齐浏览器真实执行环境（此前切片求值不带它，裸赋值只隐式建全局，测试对这类回归全绿）
