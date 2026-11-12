# 钉钉 Stream

`channels/dingtalk.New(Config{AccountID, ClientID, ClientSecret, RobotCode})`。Stream 由本机主动连接，不需要公开回调端口。主动发送需要 RobotCode；群消息默认必须 isInAtList，放开需显式 RespondWithoutMention，仍经过 Gateway Authorize。

实现 connections/open、JSON 字符串 data、CALLBACK 与 SYSTEM ACK、ping/disconnect、连接取消与重连。文本、richText 和图片/文件/音视频描述可接收；downloadCode 保存在私有 Route 缓存中，Download 显式解析 URL 并下载实际字节。文本、Markdown 可通过会话 Webhook 回复；主动发送走群 groupMessages/send 或员工 oToMessages/batchSend。

回复 URL 不进入默认 Event JSON，按完整 Route + MessageID 缓存，限制目的地主机与 /robot/sendBySession 路径，要求单个非空 session 参数，禁重定向。以平台 sessionWebhookExpiredTime 为期限，并保留最多一小时的本地缓存；缺少期限、已过期或进程重启丢失上下文返回 ErrReplyExpired，不自动切换主动发送。Direct 路由优先 senderStaffId；仅有外部 senderId 的会话不能假定拥有主动发送权限。

Reply Receipt.Accepted 要求响应中明确 errcode=0；缺失回执字段返回发送结果不确定的 APIError。Accepted 只表示 webhook API 接收；该接口可能没有消息 ID。主动发送 processQueryKey 是平台处理键，不等于已读。发送不自动重试，避免重复通知。

未实现模板 AI Cards、媒体上传/发送、审批按钮、撤回/编辑。richText 接收和显式媒体下载已实现，格式、缓存与网络边界见[媒体契约](media.md)。不要把旧 Go SDK 的方法列表当作当前应用权限。参考官方 Python stream.py/frames.py/handlers.py（2026-11-12 阅读）与当前 SDK 文档，未直接依赖任何 SDK。已有本地 mock/协议与 race 测试，真实控制台权限与长期稳定性仍待验收。

会话 Webhook 的字段和路径按[钉钉官方接收消息示例](https://opensource.dingtalk.com/developerpedia/docs/learn/bot/message/)核对（2026-11-12）。该接口与固定群机器人 `/robot/send` 不同，不接受后者作为会话回复地址。

## 扫码创建与授权

`BeginRegistration` 实现 `/app/registration/init → begin → poll`，成功后返回机器人 ClientID / ClientSecret。由用户在钉钉手机端确认创建或授权；无需预先在开发者后台手工复制凭据。

```go
registration, err := dingtalk.BeginRegistration(ctx, dingtalk.RegistrationOptions{})
if err != nil { return err }
challenge := registration.Challenge()
// 宿主将 challenge.URL 本地渲染成二维码，仅展示给发起绑定的用户。
// 此处调用宿主自己的显示函数；不要将链接提交给第三方二维码服务。
showAuthorization(challenge)
credentials, err := registration.Wait(ctx)
if err != nil { return err }
adapter, err := dingtalk.New(credentials.Config("local-account-id"))
if err != nil { return err }
```

注册端点默认 `https://oapi.dingtalk.com`，独立于消息 APIBase。Source 默认采用当前[钉钉官方 connector 配置](https://github.com/DingTalk-Real-AI/dingtalk-openclaw-connector/blob/main/src/device-auth-config.ts)中的 `DING_DWS_CLAW`；[Hermes 的参考实现](https://github.com/NousResearch/hermes-agent/blob/main/hermes_cli/dingtalk_auth.py)使用 `openClaw`。若平台为部署方注册了独立模板，可显式配置 Source；扫码页展示由该模板决定。这里没有自行声明 Hermes 或其他品牌身份。

Challenge 提供授权链接、用户码、到期时间和轮询间隔，隐藏 device_code。宿主负责二维码显示和凭据安全存储；库不会写 `.env`、覆盖已有账号或自动启动适配器。Credentials.Config 将 ClientID 同时用于 RobotCode，与[官方主动发送实现](https://github.com/DingTalk-Real-AI/dingtalk-openclaw-connector/blob/main/src/services/messaging.ts)一致；Gateway 用户授权仍需单独配置。

Wait 遵守服务端间隔与有效期，支持 context 取消，并拒绝同时轮询同一注册会话。仅在服务端省略字段时使用官方客户端的 7200 秒有效期、3 秒间隔默认值；显式 null、非正数、溢出或无效格式直接报错。FAIL、EXPIRED、未知状态或缺失成功凭据立即结束，不照搬参考客户端对这些状态继续等待两分钟的行为。网络/HTTP 错误直接返回；调用方可以在有效期内显式再次 Wait，成功和终止结果不会重复轮询。

协议对照：[官方 device-auth.ts](https://github.com/DingTalk-Real-AI/dingtalk-openclaw-connector/blob/main/src/device-auth.ts)（2026-11-12）。本地回归覆盖 HTTP 请求契约、默认/自定义 Source、待确认到成功、凭据接入适配器、取消/并发轮询/过期、畸形响应、错误脱敏及禁重定向；未执行真实钉钉扫码创建或授权。
