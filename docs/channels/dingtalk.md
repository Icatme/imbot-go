# 钉钉 Stream

`channels/dingtalk.New(Config{AccountID, ClientID, ClientSecret, RobotCode})`。Stream 由本机主动连接，不需要公开回调端口。主动发送需要 RobotCode；群消息默认必须 isInAtList，放开需显式 RespondWithoutMention，仍经过 Gateway Authorize。

实现 connections/open、JSON 字符串 data、CALLBACK 与 SYSTEM ACK、ping/disconnect、连接取消与重连。文本/图片/文件/音视频描述可接收；当前图片等仅保留 downloadCode，不自动下载。文本、Markdown 可通过会话 Webhook 回复；主动发送走群 groupMessages/send 或员工 oToMessages/batchSend。

回复 URL 不进入默认 Event JSON，按完整 Route + MessageID 缓存，限制目的地主机与 /robot/sendBySession 路径，要求单个非空 session 参数，禁重定向。以平台 sessionWebhookExpiredTime 为期限，并保留最多一小时的本地缓存；缺少期限、已过期或进程重启丢失上下文返回 ErrReplyExpired，不自动切换主动发送。Direct 路由优先 senderStaffId；仅有外部 senderId 的会话不能假定拥有主动发送权限。

Reply Receipt.Accepted 要求响应中明确 errcode=0；缺失回执字段返回发送结果不确定的 APIError。Accepted 只表示 webhook API 接收；该接口可能没有消息 ID。主动发送 processQueryKey 是平台处理键，不等于已读。发送不自动重试，避免重复通知。

未实现 AI Cards、richText 接收、媒体上传/下载、审批按钮、撤回/编辑。不要把旧 Go SDK 的方法列表当作当前应用权限。参考官方 Python stream.py/frames.py/handlers.py（2026-11-12 阅读）与当前 SDK 文档，未直接依赖任何 SDK。已有本地 mock/协议与 race 测试，真实控制台权限与长期稳定性仍待验收。

会话 Webhook 的字段和路径按[钉钉官方接收消息示例](https://opensource.dingtalk.com/developerpedia/docs/learn/bot/message/)核对（2026-11-12）。该接口与固定群机器人 `/robot/send` 不同，不接受后者作为会话回复地址。
