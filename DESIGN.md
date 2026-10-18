# workbuddy2api-prime 设计文档

> 集四家之长的最优 WorkBuddy2API 网关。分析依据：`~/workspace/workbuddy2api-analysis/docs/` 下 4 份深度分析。
> 仅供学习研究。

## 1. 技术选型

- **语言：Go**（3/4 家用 Go；单二进制部署；生态标准）
- **入站**：标准 `net/http`（零重型框架依赖，hub 的"零外部依赖"思路）
- **出站**：标准 `net/http` + SSE 流式透传
- **存储**：本地 JSON 文件（`auths/`、`data/`），0600 权限；用量账本 JSONL

## 2. 四家优点采纳矩阵

| 能力 | 来源 | 说明 |
|---|---|---|
| Go 单二进制部署 | linguo / panel-plus / HanawaBanana | 无外部依赖，Windows 双击即跑 |
| 三层选号（会话粘性→成本分层→加权随机）+ EMA 成本账本 | HanawaBanana | 调度核心，WAF 友好（零新增上游请求） |
| 积分"最早到期优先"路由 | linguo | 快过期积分优先烧完（温和权重实现，不做硬排序） |
| 四层熔断/冷却（指数退避/连败降级/模型级冷却/全冷却兜底） | linguo / HanawaBanana | |
| `cooldown.credit_exhausted` 分钟级软冷却 | HanawaBanana | 积分耗尽不硬冷却到次日 04:00 |
| 每账号固定虚拟设备指纹 + 用量归属伪装 | linguo / hub | 防关联风控 |
| 签到/巡检账号间 2–5s 随机错峰 | hub #394 | 防出口 IP 频率风控 |
| `disabledModels` 账号级模型黑名单 | hub #405 | 精确匹配，与自动节流分离 |
| 后台模型透明重定向（防烧钱） | hub v1.7.6 | 后台任务硬编码走付费模型时拦截到免费模型 |
| `wbk_` 子钥匙分发（只存 sha256） | panel-plus | 多租户/分发场景 |
| 昼夜模型编排 `auto`/`cn:auto` | panel-plus | 按北京时间自动选主模型 + 降级链 |
| 多协议兼容层（`/v1/messages` + `/v1/responses`） | panel-plus | `compatRun` 复用整条 chat 链路，零复制 |
| 兼容层工具调用修复（issue#2） | panel-plus | 入站全量翻译工具字段 + stopReason 兜底 |
| 签到结果落盘 + `GET /v1/checkin/history` | HanawaBanana | |
| 成长任务每日 01:00 自动执行 | linguo | |
| 逐请求用量账本 `usage.jsonl` | hub | 三维度统一"消耗积分"列 |
| 每 Key 独立并发限制 | hub | 超限 429 |
| Web 管理面板 | linguo | Phase 3 |

## 3. 架构

```
cmd/server/main.go          入口，组装各模块
internal/
  types/      共享类型：Account, ModelPolicy, ChatRequest, UsageRecord...
  config/     config.json 加载（0600），环境变量覆盖
  auth/       登录态管理：auths/*.json 存取，token 刷新，
              出站头注入（X-Device-Token/设备指纹/UA/用量归属头）
  pool/       账号池：Pick 三层选号 + EMA 账本 + 熔断冷却 + disabledModels
  upstream/   上游请求：协议转换（OpenAI↔腾讯），SSE 流式，出错分类
  protocol/   入站兼容层：/v1/chat/completions 主路径，
              /v1/messages、/v1/responses 经 compatRun 复用
  apikeys/    wbk_ 子钥匙：签发/校验/配额/IP 白名单/模型白名单
  router/     模型编排：auto/cn:auto 昼夜路由 + 降级链
  scheduler/  定时：签到（错峰）、巡检、成长任务 01:00
  billing/    用量记账：usage.jsonl 写入 + 聚合查询
  panel/      Web 面板（Phase 3）
```

请求链路：

```
client → apikeys.Verify → router.Resolve(model) → pool.Pick(account)
       → protocol.Translate(in) → upstream.Do → protocol.Translate(out)
       → billing.Record → client
```

## 4. 关键设计决策

1. **调度是核心**：`pool.Pick` 实现 HanawaBanana 三层选号 + linguo 的最早到期权重，不新增上游探测请求。
2. **风控默认保守**：错峰、退避、指纹隔离全部默认开启；`disabledModels` 人工黑名单与自动节流分离。
3. **防烧钱是底线**：后台模型重定向 + Anthropic 不透传 `cache_control` 时默认拒绝（hub v1.7.6 教训）。
4. **多协议零复制**：兼容层不重建调度/冷却/记账链路，mux 内调主路径（panel-plus `compatRun` 思路）。
5. **密钥安全**：`wbk_` 只存 sha256；`auths/` 0600；日志脱敏（只存元数据）。
6. **无 TLS**：公网部署前置反代（四家一致结论）。

## 5. 分阶段

- **Phase 1**：types/config/auth/pool/upstream + OpenAI chat 主路径（可用网关）
- **Phase 2**：protocol 兼容层 + apikeys + router + scheduler + billing
- **Phase 3**：panel Web 面板

## 账号运维（2026-10-17 补齐，对齐 linguo）

上游端点（以 linguo/hub 实际代码为准）：
- 签到：`POST https://www.codebuddy.cn/v2/billing/meter/daily-checkin`（body `{}`）；
  国际版走 `/billing/meter/daily-checkin`→404 fallback `/v2/...`；国际版无签到体系直接跳过
- 余额：`POST https://www.codebuddy.cn/v2/billing/meter/get-user-resource`
  （body `{PageNumber:1, PageSize:100, ProductCode:"p_tcaca", Status:[0,3], PackageEndTimeRangeBegin/End}`）；
  信封 `{code,msg,data}`，code=10001 或 msg 含"已签到"视为幂等成功
- 请求头：BillingHeaders（Bearer + `X-CodeBuddy-Request: 1` + Accept-Language + X-User-Id + X-Device-Token）

面板接口：
- `POST /panel/api/accounts/{uid}/checkin` 单号签到（签到+余额，结果记历史）
- `POST /panel/api/accounts/{uid}/balance` 单号余额刷新
- `POST /panel/api/checkin_all`（异步）/ `POST /panel/api/balance_all`（同步，2s 错峰）
- `POST /panel/api/accounts/{uid}/disable|enable`、`DELETE /panel/api/accounts/{uid}`
- 登录/导入成功后立即拉余额并推进 pool（免重启）
