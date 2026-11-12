# 消息内容与媒体传输

本文描述 v0.8.5 的消息与媒体协议支持及边界。没有新增第三方依赖、平台 SDK、转码程序或 Agent Runtime。

## 内容与未知格式

`Event.Text` 是便于文本处理的投影，`Event.Parts` 保留文字和附件的原始顺序；`Event.Quote` 保存平台已提供的引用 ID、正文与附件，避免把引用附件混入当前消息。飞书 post、钉钉 richText、企微 mixed/appmsg/quote、QQ 引用及微信引用均沿用平台字段解析。飞书引用只保留回调提供的消息 ID，不自动请求原消息正文。

不支持的入站类型通过 `UnsupportedMessageError` 报告到配置的 `OnError`；未设置回调时写入标准 slog 诊断，然后明确消费。日志只含平台与消息类型，不含正文、URL 或凭据。畸形已支持消息、授权及持久接收失败仍走原失败契约，不因为新增格式而确认成功。

已支持的内容也可通过 `receive.Policy` 按 Bot 关闭类型或限制文件扩展名/MIME，零值默认全部允许。语音转写保留原语音类型；混合消息及已知引用内容一起检查，命中策略时整条确认消费并跳过业务 Handler。配置、回调及元数据限制见[接收策略](receive-policy.md)。

## 四项媒体能力

| 能力 | 含义 |
|---|---|
| ReceiveMedia | 能识别附件描述 |
| DownloadMedia | 可显式下载真实字节 |
| UploadMedia | 可上传真实字节并取得平台资源引用 |
| SendMedia | 可发送已上传资源 |

```go
// 入站：由业务授权后的 Sink 显式下载并持久保存，成功后再确认接收。
for _, attachment := range event.Attachments {
    content, err := g.Download(ctx, event.Route, attachment)
    if err != nil { return err }
    if err := inbox.StoreAttachment(ctx, event.ID, attachment.Ref, content); err != nil {
        return err
    }
}

// 出站：每一步都明确返回错误，不自动重试、转格式或改目标。
uploaded, err := g.Upload(ctx, event.Route, gateway.MediaUpload{
    Kind: "file", Name: "report.pdf", MIME: "application/pdf",
    Data: pdfBytes, ReplyTo: event.MessageID,
})
if err != nil { return err }
receipt, err := g.Send(ctx, event.Route, gateway.Message{
    ReplyTo: event.MessageID, Attachments: []gateway.Attachment{uploaded},
})
```

`inbox.StoreAttachment` 是宿主自己的持久化接口，SDK 不创建数据库。`Download` / `Upload` / `Send` 是可信宿主接口，不代替业务出站和文件读取权限检查。原生 Adapter 也提供相同 Download/Upload 方法。

## 引用、期限与安全

- 每个附件有独立 Ref，带 Scope、MessageID、MIME、ExpiresAt 等元数据。平台未提供的值不会假装已探测。
- URL、AES key、downloadCode 等秘密保留在 Adapter 的有界缓存，不写入普通事件 JSON。新上传资源同样绑定账号与完整 Route；企微智能机器人的资源另绑定连接。
- 飞书新上传资源返回 `feishu-upload:` 不透明 Ref，私有路由缓存保留 30 分钟；企微自建应用入站媒体也使用本地不透明 Ref。修改公开 Scope 或删除过期时间不能绕过私有绑定。显式传入宿主已有的原生平台资源 key 仍是可信宿主操作。
- QQ 新上传资源返回 `qqbot-upload:` 本地不透明 Ref，私有缓存按服务端 ttl 保留 file_info、完整 Route 和媒体类型；更改公开字段不能延长寿命或改变路由/类型。入站 Ref 使用独立命名空间，清空 MessageID 后也不能冒充出站 file_info。
- 企微智能机器人入站 URL 定位缓存和 ExpiresAt 上限为 5 分钟，与官方短期链接期限一致；上传资源使用独立期限。
- **缓存不是持久文件索引。** 过期、容量淘汰或进程重启后返回 `ErrMediaExpired`；需要可靠异步消费时，在 Sink 返回成功前下载并保存到宿主文件存储。不要只保存 Event JSON 后假定 SDK 能跨重启恢复秘密参数。
- 微信当前图片经 `DownloadImage`、其他附件经 `DownloadRaw` 独立下载，不把第 N 个附件的字节写入原生单附件引用缓存。图片区分加密 CDN 对象与原生 URL 两种形式，加密失败不会改试 URL。`DownloadQuoted` 仍调用根 SDK 的持久引用恢复；桥自身保存原生调用上下文的缓存有独立期限，默认沿用 ReplyTTL。
- 微信回复上传调用根 SDK 的 `UploadForReply`，发送携带 `OutboundMessage.Upload`，绑定原 Bot、收件人、会话代次和上传资源。重新登录或 hook 替换资源后明确拒绝；缓存不保留完整上传字节，不能通过 JSON 重建这份会话授权。
- 微信原生入站消息与 handler context 同时绑定 Bot 实例和会话代次；跨 Bot 回复、上传或复用 handler context 返回 `ErrSessionChanged`，即使两个实例的代次数值相同也不例外。
- 微信桥和根包的发送都在 hook 前隔离可变资源对象，包括没有 Upload 绑定的普通消息；失败 hook 不会污染宿主后续显式重试，也不能通过赋值替换私有上传绑定。微信和企微自建应用的 Parts 保留平台已有语音识别文字；微信只有识别文字而没有音频资源时，不创建不可下载的附件。
- 微信缺少实际媒体 locator 时明确拒绝；混排中的空 wire 节点不会错配后续附件。原生下载只接受 2xx 响应，不把未跟随的跳转页面当作文件。取消在副作用或新请求前检查；已有完整有效成功回执不会因同时取消而被降为失败。
- SDK 字节上限为 100 MiB，平台还会采用更低的文件/类型限制；这不是平台官方配额承诺。上传不隐式读取任意本地路径或公网 URL。
- 新增共享媒体 HTTP 路径禁止重定向并限制响应体；CDN 默认网络路径校验公有 IP、固定已校验 DNS 结果拨号且禁用环境代理。显式注入非标准 RoundTripper 的宿主自行负责其网络策略。微信桥委托原生 Bot 的 HTTP 客户端与 CDN 实现，不经过此共享下载路径。
- QQ `UploadURL` 是明确的 QQ 服务端拉取功能，SDK 不把它冒充为本机 DNS 固定的下载。QQ 下载仅向受支持媒体域发送鉴权；本地分片上传使用官方要求的 MD5、SHA-1 和前段 MD5 校验，没有新增版本或文件指纹用途。

## 平台格式边界

- 飞书支持原生图片、文件、Opus 音频、MP4 视频；图片/视频可组成带正文的 post。普通文件或音频不能伪装成视频 post 来携带 caption。不执行 ffmpeg 或自动转码。
- 钉钉实现 richText、downloadCode 解析及下载；出站文件上传和模板 AI Card 未声明支持。
- 企微智能机器人按官方媒体类型与分片协议上传，文本 stream 仍是完整快照，不把文本增量误作 wire 协议。自建应用保留自己的鉴权、media/get 与 multipart 路径。
- 企微上传已取得完整有效成功响应或完成 ACK 后，不因随后到达的取消丢弃资源；智能机器人的资源仍须属于当前连接。
- 企微明文文件保留有效的响应 MIME 类型，通用二进制类型或缺少类型时才嗅探；密文在解密后识别，不将密文响应头当作原文件类型。
- QQ 上传成功不等于发送成功；得到 file_info 后仍须调用 Send，并处理其回执。平台提供的 `asr_refer_text` 保留到当前文本或引用文本，默认 Download 获取原音频；`DownloadVoiceWAV` 显式选择平台已提供的 WAV，不自行转码或执行 STT。频道媒体没有纳入本次 C2C/群接入。
- QQ 纯附件消息按官方 `msg_type=7` 契约将空正文编码为单个空格；已有正文保持原样。
- 微信桥复用根 SDK 的 CDN 实现，原生语音上传/转码不在桥的高层契约内；语音可下载原始数据，需要按文件发送时应明确指定 Kind=file。低层已上传资源发送与原根包低层 SendMessage 一样，不承诺额外保存出站附件字节到引用缓存。

## 长文本、组合发送与部分成功

`Gateway.Send` 仍是一个平台操作。`Gateway.SendText` 显式按 Adapter 的保守 UTF-8 字节预算分片，保持原回复 Route，为每片派生不同 ClientID，QQ 同步递增 msg_seq。它保持字节完整，不声称能跨片保持任意 Markdown 语法结构。

SendText 在发送首片前检查负数序号和整个 QQ 序号区间，避免已发送部分内容后才遇到已知的序号越界。鉴权失败直接返回；适配器仅使该请求实际使用且仍在缓存中的 token 失效，迟到的旧请求错误不清除新 token。等待其他请求刷新时可以独立取消，刷新仍保持每个缓存最多一个请求。宿主下一次显式调用才重新获取凭据，不重放当前媒体或消息操作。

共享 HTTP 与企微 WebSocket 错误保留 `context.Canceled` / `DeadlineExceeded` 的错误链，同时区分是否已经开始写入；HTTP 客户端自身的 `Timeout` 也保留超时原因。在途写入取消可能仍有 `APIError.Uncertain=true`。飞书非 2xx 响应额外解析有界错误体中的数值业务码，正文不进入错误或日志，成功的 JSON 文件仍按文件下载；发送必须收到明确的成功码及消息 ID 才返回 Accepted。

`Gateway.SendBatch` 可显式发送多条消息，例如平台不支持一条消息同时包含 caption 和文件时，宿主明确提交两条操作。二者都返回 `BatchReceipt.Receipts`；失败返回 `PartialSendError`，其 Completed 为已接受前缀长度，Unwrap 保留底层不确定发送错误。**不能在失败后重放整个批次。** SDK 不自动重试、补发、改用主动发送或文字链接。

## 卡片交互

发送按钮卡片及处理回调的完整示例见[飞书互动卡片使用指南](feishu-cards.md)，包括无需公网回调地址的长连接模式和可选 HTTP 模式。

飞书、企微智能机器人和 QQ 的已支持点击事件以 `Event.Interaction` 进入同一个 Gateway Authorize/Sink 流程；Value 是平台原生动作数据，宿主仍须验证业务权限和 schema。飞书 Value 保存完整 action 对象，包括 form_value、input_value、option/options、checked 等控件数据，排除事件级卡片 token。企微 Value 保存完整原生 event JSON，保留嵌套卡片及表单选择。QQ 只接受外层 type=2、内层 data.type=11 的按钮事件；交互 ID 不可当作普通消息的 ReplyTo，业务 ACK 同时检查 HTTP 状态和平台错误字段。

飞书使用本 Adapter 已发送卡片的完整 Route 缓存核对回调，缓存为进程内 30 分钟；未知/过期来源不猜测聊天类型或线程。QQ 原生 keyboard 需要显式开启，点击的接入 ACK 与业务交互回执分开，使用平台专属接口明确回应。SDK 不把按钮点击当成自动审批通过。

## 验证范围

协议测试涵盖本地 HTTP/WebSocket、上传字节、分片顺序、解密、资源作用域、超时/取消及错误传播。这些不能替代真实账号的权限、配额、格式显示和长期连接验收。UI 由使用者人工验收。
