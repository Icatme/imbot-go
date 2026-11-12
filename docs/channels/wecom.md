# 企业微信智能机器人（长连接）

`channels/wecom` 对接 openws 智能机器人，与企业自建应用不同。配置 AccountID、BotID、Secret，可加本地 TenantID；不接受入站 payload 覆盖本地账号。默认连接 wss://openws.work.weixin.qq.com。

## 扫码注册 / 绑定

`wecom.BeginRegistration(ctx, wecom.RegistrationOptions{})` → `Challenge()` → `Wait(ctx)` → `Credentials.Config(accountID)` 获取智能机器人的 `BotID/Secret`。宿主仅向当前绑定用户展示授权链接或本地生成的二维码；凭据持久化由宿主负责。完整示例见 [扫码接入](onboarding.md)。`Config` 不推断企业 TenantID，需租户隔离的应用仍应显式配置。

协议以企微官方 [wecom-cli 的 qrcode.rs](https://github.com/WecomTeam/wecom-cli/blob/main/crates/wecom-cli/src/auth/qrcode.rs) 为依据：`GET https://work.weixin.qq.com/ai/qc/generate` 获取 `data.scode/auth_url`，再每 3 秒 `GET /ai/qc/query_result?scode=...`；完成时读取 `data.bot_info.botid/secret`。默认来源为官方外部接入模板 `wecom_cli_external`，`plat` 随当前操作系统传递。服务端未返回有效期，SDK 沿用官方 CLI 的 300 秒本地等待上限。

这些 `ai/qc` 是扫码接入端点，不应视作稳定公开 OpenAPI。官方 CLI 仅定义 `success` 为完成，其他非空字符串状态继续等待，受上述本地期限限制；SDK 不推测额外的拒绝/过期状态。HTTP/业务错误及缺失或无效字段直接返回错误，不自动改用手填凭据或生成新二维码。当前仅验证了未扫码生成/查询响应及本地模拟回归，没有扫码创建机器人或进行 UI/真实消息验收。

此接口只适用于 `channels/wecom` 智能机器人，不返回自建应用所需的 CorpID/AgentID/CorpSecret，也不改变 `channels/wecomapp` 的接入方式。

## 已实现

认证 aibot_subscribe、应用层 ping 与回执、原生文本/语音转写/图文混排/媒体描述接收、主动 Markdown/原生模板卡片、回复模板卡片、完整快照流式回复。

`UpdateStream(Route, StreamUpdate{ReplyTo, StreamID, Content, Finished})` 的 Content 是完整文本快照，StreamID 在一次回复内保持不变，最后 Finished=true。上限 20480 UTF-8 字节，不把模型的每个 token 直接当成增量协议发送。

## 回执与并发

发送成功必须收到对应 req_id 且 errcode=0 的回执。单连接最多 128 个请求，同一 req_id 串行，待回执超时关闭连接，以免晚到 ACK 错配后续流式更新；Uncertain=true 表示平台可能已收到。

收包循环独立于接入 Sink，防止在 Sink 中 Send 等回执时死锁。接入队列有界，Sink 应快速持久接受。原生回调没有在此实现可控的消息重投承诺：接入失败会报错并断开，但不能声称平台一定重投，生产必须监控并采用可靠 inbox。

回复上下文按完整 Route+MessageID 缓存，并绑定当前连接实例；重连后旧上下文返回 ErrReplyExpired。收到 disconnected_event 表示另一实例接管，停止抢连并返回终止错误。不要让同一个 BotID 在多副本同时建立连接。

主动发送与流式回复都要求 Route.TenantID 与配置一致，仅接受 direct/group 路由。已经取消、尚未发送的请求返回上下文错误，不标记为发送结果不确定，也不关闭仍可用的连接。

## 安全与范围

媒体 URL/密钥只保留在 Event.Native（json:"-"），默认不入日志或 JSON 持久化；需要媒体处理的应用应在受控路径下载或加密保存，不能默认抓取任意 URL。Native 不适合直接打印。

未实现分片媒体上传、加密文件下载、流式图文附加、欢迎语、卡片交互回调/审批/更新。Card 仅接受平台原生 JSON，不提供高级卡片模型。参照官方 WecomTeam/aibot-node-sdk 的 src/ws.ts、src/client.ts、src/types/api.ts 与 message.ts，读取日期 2026-11-12；没有引用其代码或依赖。

13 项测试覆盖回执关联、串行、超时、负回执、旧连接隔离、流式快照、秘密序列化、出站租户隔离、取消请求以及真实本地 WebSocket 握手/回调中等待发送回执；已在实际仓库 Go 1.26.4/Windows 上通过 `go test -race ./channels/wecom`。仍未做真实企业微信账号验收。
