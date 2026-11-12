# 飞书 / Lark

`channels/feishu` 不依赖平台 SDK。默认 `Transport: "websocket"`，配置 AccountID、AppID、AppSecret。Lark 可设置 BaseURL=https://open.larksuite.com。群聊默认要求 @当前 Bot：配置 BotOpenID，或明确打开 RespondWithoutMention；Gateway 的 Authorize 仍需单独配置。

## 扫码创建与绑定

通过注册会话获取凭据，无需手动复制 AppID/AppSecret：

```go
registration, err := feishu.BeginRegistration(ctx, feishu.RegistrationOptions{
    Domain: "feishu", // 国际版为 "lark"
})
if err != nil { return err }
challenge := registration.Challenge()
// 把 challenge.URL 交给当前用户的本地二维码组件；不要发送给第三方制码服务。
showQRCode(challenge.URL)
credentials, err := registration.Wait(ctx)
if err != nil { return err }
// 先安全保存 credentials，再建立消息连接。
adapter, err := feishu.New(credentials.Config("customer-a-feishu"))
```

`BeginRegistration` 执行账号服务的 `init` / `begin`，申请 `PersonalAgent` 应用授权；`Wait` 通过私有 device code 轮询并返回凭据。它不写应用配置、不替用户决定消息权限，也不会自动重建二维码或重试网络错误。并发 `Wait` 返回 `onboarding.ErrConcurrentWait`；取消返回 context 错误，可在原二维码仍有效时显式调用 `Wait` 恢复。拒绝、过期、畸形响应分别返回 `ErrDenied`、`ErrExpired`、`ErrInvalidResponse`，终止会话。

生命周期使用官方 `expires_in` 字段；缺省有效期 600 秒、轮询间隔 5 秒，显式零值、负值和超范围值拒绝。服务端 `slow_down` 会使此后每次轮询增加 5 秒；HTTP 400 的 `authorization_pending` 是正常等待状态，其余未知错误不会无限重试。整个会话受服务端期限和调用方 context 共同限制。

支持从飞书识别 Lark 租户后切换到固定的官方 Lark 账号域名，包括第一次仅返回 Lark 租户标记的正常交接；同一响应中的成功凭据直接保留，拒绝/过期结果仍终止。`Credentials.Domain` 决定后续 API 域名；`Credentials.OpenID` 是扫码用户，不是机器人身份，不能填入 `Config.BotOpenID`。群聊仍需单独配置机器人身份或明确选择无 @ 回复策略，Gateway 用户授权仍由宿主配置。

协议对照：[飞书官方 Node SDK 注册实现](https://github.com/larksuite/node-sdk/blob/main/scene/registration/index.ts)、[注册字段定义](https://github.com/larksuite/node-sdk/blob/main/scene/registration/types.ts)、[Hermes 接入流程](https://github.com/NousResearch/hermes-agent/blob/main/plugins/platforms/feishu/adapter.py)。按官方协议使用 `expires_in`，不沿用 Hermes 中的 `expire_in` 字段差异。相关实现仅作为协议参考，没有引入这些 SDK 或二维码依赖。已覆盖本地 HTTP 合约、虚拟时钟下的等待/降速/过期/取消、域名切换及错误回归；尚未进行真实飞书/Lark 扫码验收。

## 当前能力

- endpoint discovery、二进制 protobuf Frame、Ping/Pong、分片合并、成功/失败 ACK、取消与退避重连。
- 文本、post 富文本、图片/文件等接收，Parts 保留图文顺序，Quote 与当前附件分离；保留真实 tenant/chat/thread/reply 关系。
- 文本、Markdown post、原生 interactive card JSON、已上传图片/文件/Opus/MP4 发送；支持显式资源下载与 multipart 上传、图片/视频 caption、回复与 thread reply。
- ClientID 转换成稳定 uuid。平台 API 的幂等窗口仍由飞书决定。
- 可选 Webhook：Handler(sink) 由宿主挂载 HTTPS；要求 EncryptKey + VerificationToken；检查时间窗、SHA256 签名、AES-CBC、token/app_id 后再接入。挑战请求也必须认证。
- 原生回复 API 仅携带 message_id，所以额外缓存已接收消息的完整 Route+MessageID 绑定（30 分钟、4096 条上限）。跨聊天、未知或过期回复返回 ErrReplyExpired，不能通过传错 ChatID 绕过路由检查。该缓存不是业务出站 ACL，应用仍需做权限验证。
- Upload 返回本地不透明 Ref，通过独立的 30 分钟完整路由缓存解析，不能修改公开 Scope 后跨会话复用。原生外部 image_key/file_key 仍可由可信宿主显式提供。图片预检识别 TIFF/HEIC，平台负责最终格式和分辨率校验。
- Interaction.Value 保留完整原生 action 对象，包括表单、输入与多选值；不包含事件级卡片 token。媒体 API 明确报告 token 失效时清除缓存，当前失败不自动重试。
- Send、Upload、Download 都识别 HTTP 401 及非 2xx 中明确的数值认证错误；错误体有界且不写入诊断。失效只作用于该请求实际使用的 token。未知 chat_type 不猜测成群聊，而是通过既有 unsupported 诊断后消费。

## 尚未覆盖

不提供音视频自动转码、业务审批执行、高层流式卡片 API 或消息编辑。原生 Card 的结构与权限由调用方负责；card.action.trigger 经认证和已发送卡片的完整 Route 缓存核验后进入同一 Authorize/Sink。未知类型通过 OnError 或默认 slog 报告再消费。Markdown 明确发送为 post，不隐式改成卡片；媒体、卡片缓存期限与格式限制见[媒体契约](media.md)。

参考 Python/Node 最新实现及 Go Frame 字段，来源登记在 references.md。已做本地 mock/fixture/race 测试，未使用真实飞书账号做权限、限流和长时间断线验收。上线前必须在目标应用控制台订阅 im.message.receive_v1、授予所需消息权限并完成真实联调。

长连接 discovery 将官方 `SYSTEM_BUSY`（1）及 `INTERNAL_ERROR`（1000040343）识别为可重连错误，凭证/权限错误终止；不把这个分类用于消息发送重试。协议依据：[官方 Python WS 常量](https://github.com/larksuite/oapi-sdk-python/blob/v2_main/lark_oapi/ws/const.py)与[连接实现](https://github.com/larksuite/oapi-sdk-python/blob/v2_main/lark_oapi/ws/client.py)。
