# workbuddy2api-prime

集四家之长的 WorkBuddy/CodeBuddy 多账号 OpenAI 兼容网关（Go 单二进制）。

**仅供学习研究，禁止商用倒卖。** 非官方逆向网关，账号有封禁风险。

## 它从各家拿了什么

| 能力 | 来源 |
|---|---|
| 三层选号（会话粘性→成本分层→加权随机）+ EMA 成本账本 | HanawaBanana/workbuddy2api |
| 积分"最早到期优先"路由、成长任务自动执行 | linguo2625469/workbuddy2api-panel |
| 签到错峰、账号级模型黑名单、后台模型防烧钱重定向、用量账本 | ardeyouxipianyi/workbuddy2api-hub |
| `wbk_` 子钥匙分发、昼夜模型编排、多协议兼容层 | JACKY199503/workbuddy2api-panel-plus |

详细设计见 [DESIGN.md](DESIGN.md)。上游 4 个项目的深度分析在 `~/workspace/workbuddy2api-analysis/docs/`。

## 状态

✅ Phase 1 骨架完成（`go build` + `go vet` 全过，pool 10/10 测试通过）：
- `types` 共享类型、`config` 配置加载
- `auth` 登录态管理 + 出站头注入（设备指纹隔离）
- `pool` 三层选号 + 四层熔断冷却（EMA 成本账本）
- `protocol` 三协议入站（`/v1/chat/completions`、`/v1/messages`、`/v1/responses`，含工具调用修复）
- `upstream` + `cmd/server` 接线完成（上游真实调用为 Phase 2）

✅ Phase 2 完成（`go build` + `go vet` 全过，全部模块测试通过，冒烟测试通过）：
- `upstream` 真实上游调用：cn/intl 双端点、会话头族、SSE 聚合、错误码→熔断分类映射
- `apikeys`：`wbk_` 签发/两段式校验/额度扣减/IP 白名单
- `router`：昼夜编排 `auto`/`cn:auto` + 降级链（13 测试）
- `scheduler`：签到（错峰）/保活/成长任务 + 落盘（5 测试）
- `billing`：`usage.jsonl` 账本 + 聚合查询（4 测试）
- `cmd/server` 接线：鉴权中间件 → 编排降级 → 选号上游 → 记账，`/healthz` 公开

✅ Phase 3 完成：`panel` Web 管理面板（单页 embed、账号池可视化、用量、签到历史+手动补跑、子钥匙管理，5 测试）
⏳ 已知 TODO：客户端流式透传、上游 usage 真实积分字段对接、本机断网识别、签到/成长任务真实接口调用

## 运行

```bash
# 首次运行会生成 config.json（或手动创建）
WB2API_ADMIN_KEY=你的管理密钥 go run ./cmd/server
# 面板：http://localhost:8788/panel/ （用管理密钥鉴权）
# API：http://localhost:8788/v1/chat/completions
```

## 构建

```bash
go build ./...
go run ./cmd/server
```
