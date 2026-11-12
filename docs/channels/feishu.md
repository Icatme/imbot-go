# 飞书 / Lark

`channels/feishu` 不依赖平台 SDK。默认 `Transport: "websocket"`，配置 AccountID、AppID、AppSecret。Lark 可设置 BaseURL=https://open.larksuite.com。群聊默认要求 @当前 Bot：配置 BotOpenID，或明确打开 RespondWithoutMention；Gateway 的 Authorize 仍需单独配置。

## 当前能力

- endpoint discovery、二进制 protobuf Frame、Ping/Pong、分片合并、成功/失败 ACK、取消与退避重连。
- 文本/图片/文件等接收，保留 tenant/chat/thread/reply 关系。
- 文本、原生 interactive card JSON、已上传 image_key/file_key 发送；回复与 thread reply。
- ClientID 转换成稳定 uuid。平台 API 的幂等窗口仍由飞书决定。
- 可选 Webhook：Handler(sink) 由宿主挂载 HTTPS；要求 EncryptKey + VerificationToken；检查时间窗、SHA256 签名、AES-CBC、token/app_id 后再接入。挑战请求也必须认证。
- 原生回复 API 仅携带 message_id，所以额外缓存已接收消息的完整 Route+MessageID 绑定（30 分钟、4096 条上限）。跨聊天、未知或过期回复返回 ErrReplyExpired，不能通过传错 ChatID 绕过路由检查。该缓存不是业务出站 ACL，应用仍需做权限验证。

## 尚未覆盖

不提供媒体文件上传/下载、音视频转码、审批/卡片点击处理、高层流式卡片 API；原生 Card 的结构与权限由调用方负责。当前接收解析器尚不支持 post 富文本，未知事件不触发 Agent。Markdown 字符串不会自动变成飞书卡片，显式返回 Unsupported。

参考 Python/Node 最新实现及 Go Frame 字段，来源登记在 references.md。已做本地 mock/fixture/race 测试，未使用真实飞书账号做权限、限流和长时间断线验收。上线前必须在目标应用控制台订阅 im.message.receive_v1、授予所需消息权限并完成真实联调。

长连接 discovery 将官方 `SYSTEM_BUSY`（1）及 `INTERNAL_ERROR`（1000040343）识别为可重连错误，凭证/权限错误终止；不把这个分类用于消息发送重试。协议依据：[官方 Python WS 常量](https://github.com/larksuite/oapi-sdk-python/blob/v2_main/lark_oapi/ws/const.py)与[连接实现](https://github.com/larksuite/oapi-sdk-python/blob/v2_main/lark_oapi/ws/client.py)。
