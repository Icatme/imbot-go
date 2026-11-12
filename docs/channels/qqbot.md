# QQ Bot API v2

核对日期：2026-11-12。使用原生 Go Ed25519 + HTTPS Webhook/REST，不依赖 BotGo/BotPy/Node SDK，不假定旧 WebSocket 链路对新账号可用。

## 接入

`qqbot.New(Config{AccountID, AppID, ClientSecret})`，宿主挂载 `Handler(g.Bind(adapter.Account()))` 到 HTTPS 地址。默认 APIBase 为 https://api.bot.qq.com；TokenURL 为同域 /app/getAppAccessToken。凭证是 AppID/AppSecret，不是旧 Bot Token。

每次回调先校验 X-Signature-Timestamp + 原始请求体的 Ed25519 签名、5 分钟时间窗；不能把 JSON 重新序列化后验签。secret 按协议重复填满 32 字节 seed。op=13 挑战也先验签，再签 event_ts + plain_token；op=1 返回 op=11 心跳；op=0 成功接入后返回 op=12,d=0，接入失败返回 op=12,d=1 请求重投。不能只在 HTTP 头上返回成功而忽略协议级结果。

## 扫码注册 / 绑定

`qqbot.BeginRegistration(ctx, qqbot.RegistrationOptions{})` → `Challenge()` → `Wait(ctx)` → `Credentials.Config(accountID)` 获取 AppID/ClientSecret；可选 `UserOpenID` 是扫码用户身份，不自动授予发送权限。完整示例见 [扫码接入](onboarding.md)。

协议来自腾讯官方 [QQBot Connector 1.2.0](https://www.npmjs.com/package/@tencent-connect/qqbot-connector)：`POST https://q.qq.com/lite/create_bind_task` 提交本次随机生成的 32 字节密钥，返回 `task_id`；宿主将官方 connect 页面链接本地绘制成二维码。每 2 秒 `POST /lite/poll_bind_result`，状态 0/1 等待、2 完成、3 过期。成功结果中的 `bot_encrypt_secret` 是 Base64 编码的 `12 字节 IV | 密文 | 16 字节 GCM tag`，使用本次 AES-256-GCM 密钥校验解密。官方成功 HTTP 响应返回单个机器人；Node SDK 的数组返回值是封装层，不在此额外扩展多机器人协议。

默认 `Source` 为空，对外显示“第三方机器人”，可显式提供平台认可的来源。平台未返回 TTL，`Timeout` 默认 10 分钟，仅为本地等待上限；二维码仍可提前过期。私钥不出现在 Challenge 或日志里，但授权 URL 含任务标识，必须只给发起绑定的用户。取消、网络错误、未知状态和解密失败会返回错误；不会自动换码或跳过完整性校验。

扫码只取得凭据，不自动配置回调、IP 白名单、权限或主动发送。当前 `channels/qqbot` 仍是 HTTPS Webhook 入站，须按前文完成平台回调配置。本地测试覆盖绑定请求、状态、跨语言加密向量、篡改拒绝、超时、取消和错误脱敏；未完成真实 QQ 扫码或消息验收。仅用 Go 标准库实现协议，没有安装、复制或依赖 Node SDK。

## 当前功能

接收 C2C_MESSAGE_CREATE、GROUP_AT_MESSAGE_CREATE 和支持的 INTERACTION_CREATE，区分 user_openid/member_openid/group_openid，按本地 AppID 隔离租户。引用内容、当前附件和有序 Parts 分离；原始媒体 URL 留在私有 Route 缓存与 Native，Download 显式鉴权下载实际字节。

平台提供的 `asr_refer_text` 进入对应的当前文本或引用文本；`voice_wav_url` 仅在私有缓存保存。默认 Download 获取原音频，`DownloadVoiceWAV` 显式下载平台提供的 WAV，缺少 WAV 时返回错误，不转码或执行 STT。按钮事件只接受外层 `type=2` 和 `data.type=11`，交互 ID 不能用于普通消息的 ReplyTo。

发送 C2C/群文本与已有 file_info 的富媒体；ReplyTo 映射 msg_id，Sequence 映射 msg_seq。纯附件消息按[官方发送契约](https://github.com/tencent-connect/bot-docs/blob/main/docs/develop/api-v2/server-inter/message/send-receive/send.md)为 msg_type=7 提供单个空格的 content，已有正文保持原样。默认 Sequence=1；同一入站消息多次回复时，应用 outbox 必须分配稳定递增序号。进程重试不能随意换序号绕过平台去重。

Markdown、主动发送和原生 keyboard 需要先确认目标机器人权限再显式 EnableMarkdown/EnableProactive/EnableKeyboard。配置开启不意味着平台已授权，额度、被动回复窗口与内容审核以控制台和 API 返回为准。按钮点击进入同一个 Gateway Authorize/Sink，业务交互回应使用显式 AcknowledgeInteraction。当前没有频道/guild 或旧 WebSocket。

Upload 使用官方 prepare → COS PUT → part_finish → files 协议，上传真实字节并返回本地不透明 Ref；file_info、原 Route、类型与服务端 TTL 保存在私有缓存，不能通过修改公开 Scope、Kind 或 ExpiresAt 绕过。UploadURL 同样绑定结果，明确由 QQ 服务端拉取指定公有 URL。上传不等于发送，仍需 Send 并处理回执。不重试非幂等失败，不把图片失败降级为文字链接。MD5/SHA-1 只用于官方上传协议要求的内容校验。细节见[消息与媒体契约](media.md)。

发送、媒体 API、下载及交互回执遇到 HTTP 401 时，仅清除与该请求使用值相同的缓存 token，避免迟到错误使新 token 失效。当前请求直接返回错误；只有后续显式调用才获取新 token，不自动重放请求。入站附件 Ref 与出站资源有独立命名空间，不能通过清空 MessageID 将其用于发送。

## 协议参考与测试来源（非依赖）

- 当前官方 API 使用指南：https://bot.q.qq.com/wiki/develop/api-v2/dev-prepare/interface-framework/api-use.html （页面更新 2026-07-30，API/令牌域名以该页为准）。
- 官方签名规范：https://bot.q.qq.com/wiki/develop/api-v2/dev-prepare/interface-framework/sign.html
- 官方 BotGo Webhook：https://github.com/tencent-connect/botgo/blob/master/interaction/webhook/webhook.go （核对 blob 7b94344942700871532450b3cb0a61203e818e07）。
- 官方固定原始字节验签 fixture：https://github.com/tencent-connect/botgo/blob/master/interaction/signature/interaction_test.go （核对 blob 8a8f234b774cf4e38367fb511f976b25585e0968）。仅引用公开测试输入/预期输出用于互操作校验，没有导入或复制 SDK 实现。
- 跨语言行为对照入口：https://github.com/tencent-connect/botpy 与 https://github.com/tencent-connect/bot-node-sdk 。旧 SDK 方法不作为当前可用权限的证明。

11 项测试在隔离 Go1.23 工作区通过，包括官方 BotGo 固定签名向量、官方公钥向量、改变单字节失败、挑战签名、未签名/过期拒绝、协议 ACK、群路由与 REST 契约。

研究中，签名文档网页展示的另一条签名示例未能用其显示出来的报文字节复现（Go/Python 得到相同但不同于页面展示的结果）；可能与展示空白或示例数据不一致有关，未据此断言官方协议错误，也未更改标准算法来匹配展示。验收使用上述可复核的官方仓库固定字节 fixture。真实 QQ 回调、权限、IP 白名单及全仓 Go1.25 回归尚未验收。
