# 实际能力矩阵

此表描述当前开发分支的代码，媒体与交互扩展尚未包含在 v0.8.0 中，不代表所有平台功能已完成或已通过真实账号验收。

钉钉、飞书/Lark、企微智能机器人和 QQ 提供独立的扫码注册与凭据获取接口；微信继续使用原 SDK 的扫码登录。统一接入方法、终端示例与存储边界见 [扫码接入](onboarding.md)。企微自建应用仍使用已有凭据；QQ 扫码后仍需配置当前适配器要求的 HTTPS 消息回调。

| Adapter | 接收传输 | 文本回复 | 主动发送 | 媒体 | 卡片 / 流式 |
|---|---|---|---|---|---|
| feishu | 二进制 WS；验签加密 Webhook | 文本、Markdown post、线程回复 | 同左 | 资源下载、multipart 上传、图片/文件/Opus/MP4；图片/视频可带正文 | 原生 interactive JSON、卡片点击事件；无高层流式 |
| dingtalk | Stream WS | sessionWebhook 文本/Markdown；接收 richText | 配置 RobotCode 后员工/群 | downloadCode 解析与下载；无媒体上传/发送 | 未实现模板 AI Cards |
| wecom | 智能机器人 WS | 完整快照 stream | Markdown / template_card | 图片/文件下载解密、分片上传、原生媒体发送 | 原生模板卡片、流式、模板点击事件；无业务审批 |
| wecom_app | 加密 XML HTTP 回调 | 业务关联后通过 message/send 发员工消息，非原生引用 | 当前企业 direct 员工 | media/get 下载、multipart 上传、media_id 发送 | 未实现模板卡片 |
| qqbot | Ed25519 Webhook | C2C/群文本；msg_id + msg_seq；引用内容 | 默认关闭，显式 opt-in | 鉴权下载、本地分片/显式 URL 上传、file_info 发送 | Markdown/原生 keyboard 需 opt-in；点击事件与显式交互回执 |
| weixin | 委托原 Bot.Run 长轮询 | 原 ReplyMessage，保留会话代次 | 默认关闭；开启仍受原 SDK context 约束 | 桥接根 SDK 下载/引用恢复/上传；图片、文件、视频发送 | 无流式/卡片；不自动转码语音 |

`ReceiveMedia` 表示附件识别，`DownloadMedia` 表示实际字节下载，`UploadMedia` 表示实际资源上传，`SendMedia` 表示已上传资源的发送。下载必须显式调用，不自动转码。默认输入为字节而非路径/URL；不要跨账号、Route 或平台复用 Ref。调用方法、期限与持久消费方式见[消息与媒体契约](media.md)。

Card 是平台原生 JSON，不是跨平台卡片 DSL。统一层不把不支持的卡片/流式悄悄降级成内容缺失的消息；由上层检查 Capabilities 再选择文本替代。

## 明确未完成的横向能力

持久 inbox/outbox、跨副本去重与账号租约、自动限流队列、音视频转码、跨平台卡片 DSL/审批执行、撤回编辑、平台 readiness/指标看板、JZBeacon 身份绑定和权限界面均未实现。卡片点击是经过授权的输入数据，不是批准执行某项业务操作。

Gateway 只串行化同会话的接入回调，不保证应用在落库后异步启动的 Agent 任务串行；后者由 JZBeacon/pi-go 调度负责。Run 当前采用 fail-fast：任一 Adapter 终止会取消其余 Adapter；需要故障隔离的宿主可分组运行多个 Gateway。不要误以为某通道永久认证失败会自动自愈。

## 回复上下文寿命

飞书已接收消息的完整 Route+MessageID 绑定默认缓存 30 分钟；钉钉按 sessionWebhook 期限（上限 1 小时）；企微智能机器人 5 分钟并绑定当前连接；微信桥默认 5 分钟（可配但不超过 1 小时，原 SDK 仍可更早拒绝旧会话）。缓存重启后消失，不能作为数据库持久授权凭据。

飞书原生回复接口只携带 message_id，故额外要求它来自当前 Adapter 已观察到的同一 Route；未知/跨会话/过期目标返回 ErrReplyExpired，不会悄悄忽略 ChatID。多进程出站必须回到持有该上下文的连接 owner。
