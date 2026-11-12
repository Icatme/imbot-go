# JZBeacon / pi-go 的接入边界

本轮只改 wechatbot-go，不改 JZBeacon 或 pi-go。建议依赖方向如下：

```
平台协议 / SDK 兼容桥
   ↓ 认证后的 Event
Gateway：账号/租户隔离、Authorize、接入去重、能力检查
   ↓ Sink 快速持久接受
JZBeacon inbox / 身份绑定 / CRM 权限 / 任务调度
   ↓ Agent 输入、Steer、取消、工具授权
pi-go Agent Runtime
   ↓ 回复意图
JZBeacon outbox / 目标授权 / 消息状态
   ↓ gateway.Send 或 UpdateStream
持有对应 Bot 连接/回复上下文的 Adapter
```

## 入站事务

Sink 的 nil 意味着消息已完成处理或已经可靠存入 inbox，不是“启动了一个 goroutine”。建议数据库唯一键至少包含 platform、account_id、tenant_id、kind、chat_id、thread_id、event_id；并保存原 MessageID 和完整 Route。重复键可直接视为已接受，数据库故障返回错误让适配器给出失败回执。QQ 要返回协议 d=1，不能统一成“HTTP200就成功”。

Gateway 内存 TTL 去重只优化单进程重复请求，不承担崩溃恢复。原微信 SDK 还有自身 cursor/replay：只在应用持久接受之后才让 Ack 推进。

## 身份与权限

同一个平台 sender_id 在不同组织/账号下不能自动视为同一销售人员；不同平台同名、同邮箱也不能直接合并客户。建立显式 external_identity→JZBeacon user/customer 映射，绑定、解绑、重新授权单独审计。

Authorize 是通道入站准入，不等于允许访问所有 CRM 数据。每次 CRM 工具执行仍检查当前用户角色、租户、客户可见范围；主动发送前也应校验目标授权。gateway.Send 本身是可信应用接口，没有替代业务级出站 ACL。

## 会话与多任务

SessionKey 使用 platform/account/tenant/kind/chat/thread，不仅是 user_id。Gateway 仅保证同键接入回调串行；落库后的 Agent 执行顺序、并行子任务、Steer、cancel、长任务续接由 JZBeacon/pi-go 决定，不能因为更换聊天渠道就复制一套 Agent Runtime。

长连接必须有单 owner。跨副本部署用数据库/分布式租约约束同一 BotID 的活跃实例，出站 RPC 路由到 owner。尤其企微 reply req_id 属于当前连接，不能由另一实例拿一个字符串直接发送。

## 出站与流式

outbox 先保存稳定 ClientID、Route、ReplyTo、消息类型与 QQ Sequence，再发送。只把 Receipt.Accepted 记为“平台接受”；没有回执不代表一定没送达。APIError.Uncertain 要进入核对/人工决策路径，不能无条件重试并更换 ClientID。

流式使用完整快照，稳定 StreamID，控制刷新频率，最后 Finished=true。平台不支持 Stream 时，由上层合并成最终文本；不要塞进传输层模拟工具调用。Raw Card 由平台专用渲染器生成，业务审批必须把按钮与用户、请求 ID 和权限绑定；本轮未实现交互审批。

## Native 与恢复

Native 可能包含 context_token、sessionWebhook、媒体签名 URL/AES 密钥，默认 JSON 故意排除。持久 inbox 只保存归一化内容；需要媒体时，在 owner 内受控获取并存到业务附件存储，不要交给模型或日志。

回复上下文缓存不跨重启。出站恢复遇到 ErrReplyExpired 时，应明确标记不可继续原生回复，由业务根据平台能力决定是否请求用户重新发消息或另发已授权通知。不得反序列化微信 JSON 后伪造原 SDK 的私有会话代次。

## 推荐后续业务 PR

JZBeacon 可以独立按“连接配置/账号租约 → external identity 绑定 → inbox/outbox → Agent 路由/Steer → 媒体与交互审批”推进。不要把这些业务职责并入本 SDK，也不要因有几个可用通道就宣称跨平台客户身份和权限已打通。
