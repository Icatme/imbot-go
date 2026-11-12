# 国内多通道 Gateway：架构与交付计划

核对日期：2026-11-12。基线：wechatbot-go v0.7.0，main `abcc59906486b506dc68da682c7f643322d37244`。

## 兼容与依赖原则

保留根包 `wechatbot` 的所有现有 API、iLink 登录、Ack/Retry/Drop、游标、重放和媒体语义；不改 module path、go.mod、go.sum，也不把旧客户强制迁移到 Gateway。新增 `gateway` 与 `channels/*` 为可选入口。

平台 SDK 只用于阅读协议和行为对照，不 import、不 vendor、不复制生成代码。新通道使用 Go 标准库；参考仓库、实际核对的文件、协议差异见 [references.md](references.md)。SDK 包版本或提交日期不是功能兼容证明。

## 三层职责

```
飞书 / 钉钉 / 企微 / QQ / iLink
  channels/*：认证、连接、事件解码、媒体描述、回执、平台发送
  gateway：统一事件、账号/租户路由、授权、接入去重、能力检查
  应用持久 inbox/outbox → JZBeacon 业务与权限 → pi-go Agent Runtime
```

本仓库不引入 JZBeacon、pi-go、模型 SDK 或数据库。Gateway 的 Sink 是“接入/持久接受”接口，不是长时间执行模型的接口。JZBeacon 在 Sink 中以幂等键落库后返回 nil，再由任务系统处理业务。SDK 的 Run 只维护通道生命周期。

## 身份与安全

Account = platform + configured account_id；SessionKey = platform + account_id + tenant_id + kind + chat_id + thread_id，使用 JSON 数组编码，避免分隔符碰撞。接入去重与回复上下文都使用完整 Route，避免不同线程或会话类型碰撞。不同平台或租户相同 user_id 不是同一人，不能自动合并 CRM 身份。

Gateway 默认拒绝所有访问，生产必须配置 Authorize。允许的机器人来源也不等于允许其用户操作 CRM：通道鉴权、用户授权、业务工具权限分别校验。Bind(account) 拒绝适配器跨账号注入；平台账号由本地配置确定，不信任 payload 声称的账号。

Webhook 先验签/解密，再解析/接入。限制请求体与网络响应大小，失败关闭，禁止自动跟随携带凭证的重定向。不把 context_token、sessionWebhook、secret、媒体密钥放入默认 JSON 事件或日志。

## 回执与可靠性边界

接入成功只表示 Sink 完成处理或持久接收，不表示 Agent 已答完。内存去重只覆盖单进程 TTL 窗口，重启/多副本需要应用数据库唯一键。失败接入不得写入成功去重记录；并发重复事件等待同一接入结果；同会话接入串行、不同会话有界并发。

出站 Receipt.Accepted 只表示平台 API 接受，不表示用户已读。网络超时可能已发送：SDK 不盲目重试非幂等发送，APIError.Uncertain 供 outbox 决策。平台不支持的能力返回 ErrUnsupported，不能假装成功或悄悄改发到另一目标。过期的回复上下文返回 ErrReplyExpired。

## PR 拆分（按顺序）

| 批次 | 范围 | 验收重点 |
|---|---|---|
| 1 | 统一事件、注册、授权、路由与文档 | 兼容旧根包；并发去重与账号隔离 |
| 2 | 原生 HTTP/WebSocket 与公共状态 | 限额、取消、掩码/分片、重定向、Token 缓存 |
| 3 | 飞书 | 长连接 protobuf、Webhook 验签/解密、文本/卡片 |
| 4 | 钉钉 | Stream 订阅与 ACK、会话回复、主动发送 |
| 5 | 企业微信智能机器人 | subscribe、heartbeat、回执关联、流式回复 |
| 6 | 企业微信自建应用 | 加密 XML 回调、URL 验证、主动 API |
| 7 | QQ Bot | Ed25519 Webhook、挑战验证、C2C/群聊发送 |
| 8 | iLink 兼容桥、示例、JZBeacon 接入与验收矩阵 | 旧 SDK 不变，能力边界可查 |

这是一轮可独立评审的基础通道交付，不承诺复制 Hermes 所有卡片、音视频、审批和群管理功能。平台真实账号、权限、配额、发布审核不能由 mock 测试证明。
