# 实际能力矩阵

此表描述提交的代码，不代表所有平台功能已完成或已通过真实账号验收。

| Adapter | 接收传输 | 文本回复 | 主动发送 | 媒体 | 卡片 / 流式 |
|---|---|---|---|---|---|
| feishu | 二进制 WS；验签加密 Webhook | 文本、线程回复 | 文本 | 入站引用；已有 image_key/file_key 发送，无上传下载 | 原生 interactive JSON；无高层流式 |
| dingtalk | Stream WS | sessionWebhook 文本/Markdown | 配置 RobotCode 后员工/群 | 入站 downloadCode 描述，无上传下载/发送 | 未实现 AI Cards |
| wecom | 智能机器人 WS | 完整快照 stream | Markdown / template_card | 入站描述，秘密仅 Native；无上传下载 | 原生模板卡片、流式；无交互/审批回调 |
| wecom_app | 加密 XML HTTP 回调 | 业务关联后通过 message/send 发员工消息，非原生引用 | 当前企业 direct 员工 | 入站 media_id；已有 media_id 发送，无上传下载 | 未实现模板卡片 |
| qqbot | Ed25519 Webhook | C2C/群文本；msg_id + msg_seq | 默认关闭，显式 opt-in | 入站描述；已有 file_info 发送，无上传下载 | Markdown 需 opt-in；无模板/按钮 |
| weixin | 委托原 Bot.Run 长轮询 | 原 ReplyMessage，保留会话代次 | 默认关闭；开启仍受原 SDK context 约束 | 桥保留描述；原 SDK 媒体 API 不变，未纳入统一发送 | 桥仅文本，无流式/卡片 |

ReceiveMedia 表示能识别并描述消息，不表示 SDK 自动下载、解密或转码。SendMedia 在当前实现只接受对应平台已经上传的引用，不接受任意路径/URL；不要把平台 A 的 Ref 传给平台 B。

Card 是平台原生 JSON，不是跨平台卡片 DSL。统一层不把不支持的卡片/流式悄悄降级成内容缺失的消息；由上层检查 Capabilities 再选择文本替代。

## 明确未完成的横向能力

持久 inbox/outbox、跨副本去重与账号租约、自动限流队列、统一媒体下载上传和转码、跨平台交互卡片/审批、撤回编辑、平台 readiness/指标看板、JZBeacon 身份绑定和权限界面，都不由本轮代码冒充实现。必要的错误边界和可选接口已经保留。

Gateway 只串行化同会话的接入回调，不保证应用在落库后异步启动的 Agent 任务串行；后者由 JZBeacon/pi-go 调度负责。Run 当前采用 fail-fast：任一 Adapter 终止会取消其余 Adapter；需要故障隔离的宿主可分组运行多个 Gateway。不要误以为某通道永久认证失败会自动自愈。

## 回复上下文寿命

飞书已接收消息的完整 Route+MessageID 绑定默认缓存 30 分钟；钉钉按 sessionWebhook 期限（上限 1 小时）；企微智能机器人 5 分钟并绑定当前连接；微信桥默认 5 分钟（可配但不超过 1 小时，原 SDK 仍可更早拒绝旧会话）。缓存重启后消失，不能作为数据库持久授权凭据。

飞书原生回复接口只携带 message_id，故额外要求它来自当前 Adapter 已观察到的同一 Route；未知/跨会话/过期目标返回 ErrReplyExpired，不会悄悄忽略 ChatID。多进程出站必须回到持有该上下文的连接 owner。
