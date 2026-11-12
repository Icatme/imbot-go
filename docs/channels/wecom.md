# 企业微信智能机器人（长连接）

`channels/wecom` 对接 openws 智能机器人，与企业自建应用不同。配置 AccountID、BotID、Secret，可加本地 TenantID；不接受入站 payload 覆盖本地账号。默认连接 wss://openws.work.weixin.qq.com。

## 已实现

认证 aibot_subscribe、应用层 ping 与回执、原生文本/语音转写/图文混排/媒体描述接收、主动 Markdown/原生模板卡片、回复模板卡片、完整快照流式回复。

`UpdateStream(Route, StreamUpdate{ReplyTo, StreamID, Content, Finished})` 的 Content 是完整文本快照，StreamID 在一次回复内保持不变，最后 Finished=true。上限 20480 UTF-8 字节，不把模型的每个 token 直接当成增量协议发送。

## 回执与并发

发送成功必须收到对应 req_id 且 errcode=0 的回执。单连接最多 128 个请求，同一 req_id 串行，待回执超时关闭连接，以免晚到 ACK 错配后续流式更新；Uncertain=true 表示平台可能已收到。

收包循环独立于接入 Sink，防止在 Sink 中 Send 等回执时死锁。接入队列有界，Sink 应快速持久接受。原生回调没有在此实现可控的消息重投承诺：接入失败会报错并断开，但不能声称平台一定重投，生产必须监控并采用可靠 inbox。

回复上下文按完整 Route+MessageID 缓存，并绑定当前连接实例；重连后旧上下文返回 ErrReplyExpired。收到 disconnected_event 表示另一实例接管，停止抢连并返回终止错误。不要让同一个 BotID 在多副本同时建立连接。

## 安全与范围

媒体 URL/密钥只保留在 Event.Native（json:"-"），默认不入日志或 JSON 持久化；需要媒体处理的应用应在受控路径下载或加密保存，不能默认抓取任意 URL。Native 不适合直接打印。

未实现分片媒体上传、加密文件下载、流式图文附加、欢迎语、卡片交互回调/审批/更新。Card 仅接受平台原生 JSON，不提供高级卡片模型。参照官方 WecomTeam/aibot-node-sdk 的 src/ws.ts、src/client.ts、src/types/api.ts 与 message.ts，读取日期 2026-11-12；没有引用其代码或依赖。

11 项测试覆盖回执关联、串行、超时、负回执、旧连接隔离、流式快照、秘密序列化以及真实本地 WebSocket 握手/回调中等待发送回执。仍未做真实企业微信账号验收。
