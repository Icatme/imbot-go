# imbot-go

imbot-go 是 Go 多 IM 机器人 SDK，提供统一 Gateway，支持微信 iLink、企业微信、QQ、飞书/Lark 和钉钉。

代码最初来源于 [corespeed-io/wechatbot](https://github.com/corespeed-io/wechatbot) 的 Go SDK 部分，
协议行为也参考 [Tencent/openclaw-weixin](https://github.com/Tencent/openclaw-weixin)。当前仓库作为
独立 Go module 维护，不与上述仓库自动同步。

项目范围包括登录、消息收发、媒体传输、投递状态和会话状态管理，不包含 Agent 推理或编排运行时。

---

## 安装

```bash
go get github.com/Icatme/imbot-go@v0.9.2
```

要求 Go 1.25+，零 CGO 依赖。

## 最近版本（v0.9.2）：飞书互动卡片使用文档

- 补充互动卡片发送、按钮参数回传及业务 Handler 接入示例。
- 明确默认长连接无需公网回调地址，应用仍需订阅 `card.action.trigger`；可选 Webhook 模式由宿主提供 HTTPS 入口。
- 说明发送与接收共用 Adapter、30 分钟路由缓存、回调处理期限及卡片更新边界。

本次为文档与版本元数据更新，没有新增运行时功能。Go 版本、模块路径和依赖保持不变。

[互动卡片使用指南](docs/channels/feishu-cards.md) · [v0.9.2 Release](https://github.com/Icatme/imbot-go/releases/tag/v0.9.2)

## 历史版本

### v0.9.1：接收策略与引用处理

#### 按 Bot 配置接收内容

原生 `imbot.Options` 和 `gateway.Options` 支持 `ReceivePolicy`，不配置时默认接收平台支持的全部内容。例如关闭语音、视频，并只接收 PDF/Word/Excel 文件：

```go
ReceivePolicy: receive.Policy{
    Disabled:       []receive.Kind{receive.Voice, receive.Video},
    FileExtensions: []string{"pdf", "docx", "xlsx"},
},
```

`receive` 的导入路径为 `github.com/Icatme/imbot-go/receive`。Gateway 可通过 `AccountReceivePolicies` 为每个 Bot 单独配置。策略拒收正常确认消费，不触发业务 Handler；混合内容和引用一起检查，语音转写也受语音开关约束。文件白名单是元数据筛选，实际字节仍须在显式下载后校验。完整示例见[接收策略](docs/channels/receive-policy.md)。

飞书按钮卡片的发送、业务参数回传及长连接/Webhook 配置见[互动卡片使用指南](docs/channels/feishu-cards.md)。默认长连接无需公网回调地址。

修复微信策略拒收误重试、QQ 已消费拒收误报运行错误，以及飞书媒体别名、缓存混合引用和缺失文件元数据绕过限制的问题。Go 版本、模块路径和第三方依赖不变；已有配置默认保持全部接收。

版本说明见 [v0.9.1 Release](https://github.com/Icatme/imbot-go/releases/tag/v0.9.1)。

### v0.9.0：迁移到 imbot-go

项目由 `wechatbot-go` 更名为 `imbot-go`。Go 模块路径改为 `github.com/Icatme/imbot-go`，根包名改为 `imbot`；`gateway`、`channels/*` 和原生微信 API 的行为保持不变。

下游需将 `go.mod` 和 import 中的 `github.com/Icatme/wechatbot-go` 改为 `github.com/Icatme/imbot-go`，根包调用从 `wechatbot.X` 改为 `imbot.X`，然后执行 `go mod tidy`。不要同时依赖新旧模块，它们的 Go 类型身份不同。

历史标签保持原样，v0.8.6 及之前仍使用旧模块路径。原生微信状态继续使用已有 `~/.wechatbot` 目录及显式配置的存储路径，无需因改名重新登录。没有兼容包装包、自动状态搬迁或新的第三方依赖。

版本说明见 [v0.9.0 Release](https://github.com/Icatme/imbot-go/releases/tag/v0.9.0)。

### v0.8.6：修复钉钉扫码后的注册等待

- 钉钉返回 `SCANNED` 时继续等待授权，不再误报 `onboarding: invalid registration response`；仅在 `SUCCESS` 且凭据完整时返回成功。
- 保留服务端轮询间隔、有效期和取消能力；拒绝、过期、未知状态及不完整成功响应仍明确失败。
- 扩展已有注册契约，覆盖扫码后成功、拒绝、过期、取消和显式恢复。Go 1.25+、module path 与第三方依赖不变。

完整版本说明见 [v0.8.6 Release](https://github.com/Icatme/imbot-go/releases/tag/v0.8.6)。真实账号与 UI 验收由使用者完成。

### v0.8.5：消息与媒体协议修复

- 完善多通道富文本、附件上传下载、引用与卡片事件处理，保留消息顺序和长文本/批量发送的部分成功结果。
- 修复媒体资源跨路由复用、过期校验及微信跨 Bot 会话复用；发送 hook 不再污染原消息或缓存资源。
- 修复迟到鉴权错误清除新 token、客户端超时原因丢失、上传成功结果被取消覆盖，以及飞书和 QQ 的发送协议问题。

仍要求 Go 1.25+，module path 和第三方依赖不变。钉钉出站媒体、自动转码、持久 inbox/outbox 与跨副本账号租约不在当前支持范围；真实账号与 UI 验收由使用者完成。

接口与限制见[消息与媒体契约](docs/channels/media.md)、[能力矩阵](docs/channels/capabilities.md)和[多通道接入](docs/channels/README.md)。完整版本说明见 [v0.8.5 Release](https://github.com/Icatme/imbot-go/releases/tag/v0.8.5)。

### v0.8.0：多通道 Gateway 与扫码注册

- 新增 `gateway`：统一账号、租户、会话和线程路由，提供显式授权、接入去重、按会话串行接入和能力查询。
- 新增飞书/Lark、钉钉、企业微信智能机器人、企业微信自建应用、QQ 适配器，以及复用根包的微信 iLink 桥接。
- 钉钉、飞书/Lark、企微智能机器人和 QQ 支持 `BeginRegistration → Challenge → Wait → Credentials.Config` 扫码注册；宿主负责展示授权链接、验证平台身份、保存凭据和安装连接。
- 新增 `examples/channel-login` 凭据获取示例和 `examples/multi-channel` 多通道连接探针，配套接入、能力与集成文档。

原微信根包 API 和 module path 保持不变；新增通道按需导入，要求仍为 Go 1.25+，未增加第三方依赖。
企微自建应用使用已有凭据；QQ 扫码取得凭据后仍需配置 HTTPS 消息回调。
v0.8.0 的统一层尚不提供媒体上传下载、持久 inbox/outbox 或跨副本账号租约；当前版本的媒体支持见上节，真实平台账号与 UI 验收仍需使用者完成。

接入说明见[多通道 Gateway](docs/channels/README.md)、[扫码接入](docs/channels/onboarding.md)和[能力矩阵](docs/channels/capabilities.md)。
完整版本说明见 [v0.8.0 Release](https://github.com/Icatme/imbot-go/releases/tag/v0.8.0)。

### v0.7.0：引用附件恢复

`IncomingMessage.QuotedMessage.Media` 提供引用图片、文件、视频或语音的类型、文件名、
MIME、大小及内嵌 CDN 引用；`Cached` 表示解析时已有可用的本地副本。
图片和视频的 MIME 在取得实际数据后识别，下载前为空；无法识别的二进制格式返回 `application/octet-stream`。
附件与当前消息的 `Images` / `Files` 等字段分开，使用 `DownloadQuoted` 显式读取：

```go
bot.Handle(imbot.MessageHandlerFunc(func(ctx context.Context, msg *imbot.IncomingMessage) imbot.MessageResult {
    if msg.QuotedMessage != nil && msg.QuotedMessage.Media != nil {
        attachment, err := bot.DownloadQuoted(ctx, msg)
        if errors.Is(err, imbot.ErrQuoteMediaUnavailable) {
            // 应用提示原附件未下载、已过期、超出配额或已被删除。
        } else if err != nil {
            return imbot.RetryMessage(err)
        } else if attachment != nil {
            // attachment.Data / FileName / MIMEType 交给应用处理。
        }
    }
    return imbot.AckMessage()
}))
```

轮询不会自动下载附件。`Download`、`DownloadQuoted` 成功取得的数据，以及 `SendMedia` /
`ReplyContent` 成功发送的数据，会按账号、对端用户和服务端消息 ID 保存，供只有 `svr_id`
的后续引用使用。低层 `SendMessage` 只有 CDN 引用时保存元数据，不额外下载。
语音保留原始 SILK，不执行转码；SDK 不执行 PDF/文件解析或构造 Agent 提示词。

```go
bot := imbot.New(imbot.Options{
    QuoteCache: imbot.QuoteCacheOptions{
        MediaRetention:      7 * 24 * time.Hour,
        MaxMediaBytes:       256 << 20, // 每账号
        MaxSingleMediaBytes: 25 << 20,
        // MediaDisabled: true, // 关闭附件持久化，保留文本和附件元数据
    },
})
```

上述数值也是默认值。附件保存在 `<QuoteCache.Path>.media/`，引用索引使用原有 JSON 文件；
备份时应一起保存，目录内不得放置其他 SDK 状态。文件名由 SDK 随机分配，原始文件名仅用于展示。
持久化文件是解密后的正文，缓存不保存 CDN 地址或 AES 密钥。
每次缓存写入会淘汰过期、超量记录并清理失去引用的 SDK 文件；查询不会命中过期文件。
应用需要在空闲时主动释放磁盘空间时，可调用 `bot.PruneQuoteCache()`。
被淘汰的附件仍可保留文件名等元数据，读取时返回 `ErrQuoteMediaUnavailable`。

超出文件大小或空间配额不会将成功下载或发送改判为失败。缓存 IO 故障通过 `OnError`
报告 `*QuoteCacheError`；读取仅有本地副本的引用时，IO 故障直接返回该错误。
缓存损坏保留原文件；本地读取失败不会触发隐藏的远程重试。`QuoteCache.Disabled` 关闭全部缓存。

版本源码见 [v0.7.0](https://github.com/Icatme/imbot-go/tree/v0.7.0)。

### v0.6.0：验证码登录与多账号使用

`Options.OnVerifyCode` 从 `func() (string, error)` 改为
`func(ctx context.Context, isRetry bool) (string, error)`，调用方需要更新回调签名。
回调接收 `Login` / `Reauthenticate` 的 context，等待用户输入时必须响应取消：

```go
verifyCodes := make(chan string) // 应用将当前登录流程的用户输入写入此 channel
bot := imbot.New(imbot.Options{
    OnVerifyCode: func(ctx context.Context, isRetry bool) (string, error) {
        // 应用根据 isRetry 展示首次输入或验证码错误提示。
        select {
        case <-ctx.Done():
            return "", ctx.Err()
        case code := <-verifyCodes:
            return code, nil
        }
    },
})
```

同一二维码的验证码被服务端拒绝时，下次回调的 `isRetry` 为 `true`；服务端返回 `scaned`
或刷新二维码后重置。输入会去除首尾空白，空输入或回调错误会结束当次登录。
未配置回调时，遇到验证码挑战会返回明确错误。验证码封锁会申请新二维码，
与过期刷新共用最多 3 次二维码申请的上限；轮询间隔也响应 context 取消。

普通登录读取本地凭证失败时会返回读取错误，保留原文件。应用可在确认需要新会话后
显式调用 `Reauthenticate(ctx)`。

多账号部署需要隔离状态路径，并确保每个账号只有一个活跃轮询实例，见[多租户与多账号](#多租户与多账号)。
版本源码见 [v0.6.0](https://github.com/Icatme/imbot-go/tree/v0.6.0)。

### v0.5.0：消息 ID 与文本引用

- `message_id`、`msg_id` 和引用中的 `svr_id` 支持 JSON 字符串与完整 `uint64` 数值，避免大 ID 导致解码失败。
- `SendMessage` / `ReplyMessage` 的 `SendResult.MessageID` 与 `AfterSend` 回调保留服务端消息 ID；服务端未返回时为空。
- 新版微信只携带 `svr_id` 时，从本地历史还原文本引用；支持 `partial_text` 的局部引用及 MD5 校验。
- 收取时优先使用顶层 `message_id`，缺失时使用消息项中的首个有效 `msg_id`；引用时优先使用 `svr_id`，缺失时读取 `message_item.msg_id`。只有 ID 的引用占位项会查询缓存，未命中时返回 `missing`。
- 文本引用缓存默认开启，按登录账号和对端用户隔离，重启后仍可使用。缓存错误通过 `*QuoteCacheError` 上报，不将已成功发送的消息改判为发送失败。

**调用方迁移：** `WireMessage.MessageID` 从 `int64` 改为字符串底层类型 `MessageID`，`MessageItem.MsgID` 也使用这个类型。
使用字符串字面量构造/比较 ID；从整数转换时先用 `strconv.FormatUint` / `strconv.FormatInt`，再转为 `imbot.MessageID`，不要直接把整数转换为字符串类型。
缺失和 JSON 零值 ID 统一为空字符串；JSON 序列化输出字符串。现有非零数字消息的 replay 键保持不变。

#### 引用缓存配置

```go
bot := imbot.New(imbot.Options{
    QuoteCache: imbot.QuoteCacheOptions{
        Path:        "./state/quotes.json", // 默认：实际凭证文件路径 + ".quotes.json"
        Retention:   30 * 24 * time.Hour,
        MaxMessages: 10000, // 每账号
    },
})
```

设置 `QuoteCache.Disabled = true` 可关闭缓存。v0.5.0 缓存保存收发正文及语音转写文本，不保存认证信息或媒体文件；v0.7.0 的附件能力见上文。
每条正文最多 64 KiB，整个文件内的正文和身份字段合计最多 16 MiB；超出容量优先淘汰旧记录。
默认 30 天后不再命中，后续写入时清理过期磁盘记录。一个缓存文件只能由一个 Bot/进程写入；配置路径不得与其他状态文件重合。

`IncomingMessage.QuotedMessage` 包含原 `MessageID`、`PartialText` 和 `Resolution`：

| Resolution | 含义 |
|---|---|
| `embedded` / `cached` | 来自消息内嵌内容 / 本地缓存；局部引用校验成功时 `Text` 为选中片段 |
| `missing` | 历史记录缺失或已过期，未还原正文 |
| `disabled` / `unavailable` | 缓存关闭 / 不可用 |
| `invalid_partial` | 局部引用索引或 MD5 无法验证，`Text` 为空 |

缓存损坏会保留原文件并上报错误，当前 Bot 不会改用内存缓存或自动覆盖；修复文件后重新创建 Bot。
缓存启用前的历史消息和仅凭 ID 引用的媒体附件无法还原。缓存不参与 Ack / Retry / Drop 和 cursor 提交判定。

完整说明见 [v0.5.0 Release](https://github.com/Icatme/imbot-go/releases/tag/v0.5.0)。

### v0.4.2

`v0.4.2` 为微信消息长轮询的客户端超时保留网络传输余量：请求至少等待 65 秒，且比服务端建议的轮询时间多 15 秒，避免正常长轮询在边界处误报超时。SDK 的 HTTP 总超时为 2 分钟。

完整说明见 [v0.4.2 Release](https://github.com/Icatme/imbot-go/releases/tag/v0.4.2)。

### v0.4.1

`v0.4.1` 是兼容性补丁版本：

- API、CDN、二维码、远程媒体和日志错误统一脱敏，覆盖签名 URL、认证信息、嵌套或畸形载荷，并限制上游错误消息长度。
- 出站 wire 消息及引用文本中的不安全裸 `<` 会归一化为全角字符；明确允许的少量 HTML 标签和行级 fenced code 保持不变。
- 出站归一化基于请求私有副本执行，避免修改调用方模板，并支持安全的并发复用。
- 将根包的大型实现按登录、会话、轮询、内容、媒体、消息、Handler 和日志职责拆分为同包文件，公共 API 与行为保持不变。

完整说明见 [v0.4.1 Release](https://github.com/Icatme/imbot-go/releases/tag/v0.4.1)。

### v0.4.0

`v0.4.0` 主要调整投递与会话语义：

- 增加持久化 replay 去重、原子 cursor/context/replay 状态以及 malformed message fail-closed。
- Handler 改为接收 `context.Context`，并通过 Ack / Retry / Drop 明确返回投递结果。
- 同一对端用户串行处理，不同用户在有界并发下处理；cursor 只推进到连续完成的批次前缀。
- 增加 Agent `run_id` / `session_id` / `group_id`、工具调用项类型 11/12，以及稳定的出站 `client_id`。
- API 错误可通过 `errors.As` 获取 HTTP status、`ret` 和 `errcode`。
- 会话过期后不再自动复用旧 token；调用方显式调用 `Reauthenticate(ctx)`，持久化 marker 保证进程重启后仍保持 fail-closed。

从 `v0.3.x` 迁移时需要注意：

- `Bot.OnMessage(func(*IncomingMessage))` 改为 `Bot.Handle(MessageHandler)`；Handler 需要返回 `MessageResult`。
- `LifecycleHooks.BeforeSend` 的 payload 从 `*SendContent` 改为 `*SendRequest`，并增加 `AfterSend`。
- 收到 `ErrReauthRequired` 后，由应用决定何时发起二维码交互并调用 `Reauthenticate(ctx)`。

完整说明见 [v0.4.0 Release](https://github.com/Icatme/imbot-go/releases/tag/v0.4.0)。

## 快速开始

```go
package main

import (
    "context"
    "errors"
    "fmt"
    "os"
    "os/signal"
    "syscall"

    "github.com/Icatme/imbot-go"
)

func main() {
    ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer cancel()

    bot := imbot.New(imbot.Options{
        OnQRURL: func(url string) { fmt.Println("请扫码:", url) },
    })

    if _, err := bot.Login(ctx, false); err != nil {
        fmt.Fprintln(os.Stderr, "登录失败:", err)
        os.Exit(1)
    }

    bot.Handle(imbot.MessageHandlerFunc(func(ctx context.Context, msg *imbot.IncomingMessage) imbot.MessageResult {
        if err := bot.Reply(ctx, msg, fmt.Sprintf("Echo: %s", msg.Text)); err != nil {
            return imbot.RetryMessage(err)
        }
        return imbot.AckMessage()
    }))

    if err := bot.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
        fmt.Fprintln(os.Stderr, "运行结束:", err)
    }
}
```

更多示例见 [`examples/`](examples/)。

## 多租户与多账号

每个微信账号创建一个 Bot，使用独立的凭证、context token、cursor、replay 和引用缓存路径。
首次扫码尚不知道微信账号 ID 时，可留空 `AccountID`，由应用分配独立目录：

```go
stateDir := "./state/wechat/tenant-001"
bot := imbot.New(imbot.Options{
    CredPath:         filepath.Join(stateDir, "credentials.json"),
    ContextTokenPath: filepath.Join(stateDir, "context_tokens.json"),
    CursorPath:       filepath.Join(stateDir, "cursor.json"),
    ReplayPath:       filepath.Join(stateDir, "replay_dedupe.json"),
    QuoteCache: imbot.QuoteCacheOptions{
        Path: filepath.Join(stateDir, "quotes.json"),
    },
    // OnQRURL / OnScanned / OnExpired 与 OnVerifyCode 接入该租户的登录交互。
})
```

`Options.AccountID` 表示期望登录的真实微信 bot ID，不是业务租户 ID。登录结果会与它校验；
设置后，未显式指定的凭证及 context/cursor/replay 路径默认位于 `~/.wechatbot/accounts/<AccountID>/`。
引用缓存默认为 `<CredPath>.quotes.json`，重认证 marker 为 `<CredPath>.reauth.json`；备份或迁移时一起保存。

同一微信账号在全集群只能有一个活跃的轮询实例，每个状态文件也只能由一个 Bot/进程写入。
`Run()` 的防重复检查仅覆盖同一 Bot 对象；多进程或多机部署由应用使用租约或数据库锁选主，
在租约失效时取消 `Run()`，并等待旧实例完全退出后再启动接替实例。

有效本地凭证由 `Login(ctx, false)` 直接恢复。公共登录流程不向 QR 接口发送历史 `local_token_list`；
`Reauthenticate(ctx)` / `Login(ctx, true)` 要求新会话，不会通过 `binded_redirect` 复用旧 token。

## 功能范围

- 扫码登录 + 凭证持久化
- 长轮询接收消息
- 轮询与消息处理解耦；同一用户严格有序，不同用户有界并发
- Handler 接收运行上下文，并以 Ack / Retry / Drop 明确控制投递结果
- Agent `run_id` / `session_id` / `group_id` 解析与工具调用项类型（11 / 12）
- 单项出站消息支持调用方复用 `client_id`、返回发送身份并发送工具调用进度
- 按会话持久化 `message_id` / `client_id` / `seq` 全部身份别名，避免跨用户误判重放
- 消息解码失败时默认停止消费并保留 cursor，避免静默丢消息
- 文本 / 图片 / 文件 / 视频 / 语音 收发
- CDN 上传下载与 AES-128-ECB 解密
- context_token 自动管理
- 输入状态指示器
- 会话过期（`-14`）进入终止态，并由调用方显式重新认证

Handler 返回 `RetryMessage(err)` 时，当前消息的重放键与所在批次游标不会提交，`Run` 会携带该错误退出；
返回 `DropMessage(reason)` 会明确消费该消息。`Stop` 和传给 `Run` 的 context 会传播到 Handler，Handler 应及时响应取消。

默认最多同时处理 4 个用户，可通过 `Options.MaxConcurrentHandlers` 调整（最大 256，设为 `1` 可全局串行）。
同一个 Handler、Middleware 与 `AfterReceive` Hook 可能被不同用户并发调用，因此实现必须并发安全。全局轮询游标只会
推进到连续完成的批次前缀；缺少 `message_id`、`client_id` 和 `seq` 的消息仍属于 at-least-once 投递。
重放键按对端用户隔离，不同会话可安全复用相同的 `message_id`、`client_id` 或 `seq`。

任一已认证接口返回 `ret: -14` 或 `errcode: -14` 时，SDK 会清除失效凭证、配置缓存与
`context_token`，取消正在运行的轮询，并返回可由 `errors.Is(err, ErrReauthRequired)` 识别的错误。
它不会自动弹出二维码或再次使用旧 token；应用确认交互时机后调用 `Reauthenticate(ctx)`。显式重认证也会先
封锁当前健康会话，且旧 Run/Handler 不能使用新凭证发送或复活旧 `context_token`；已经明确 Ack/Drop 的
投递仍按原语义提交。需要启动新认证流程的并发或回调内重入请求会返回
`ErrLoginInProgress`；属于旧会话的后续操作返回 `ErrSessionChanged`，不表示当前会话仍需扫码。
失效转换会先持久化 `<CredPath>.reauth.json` marker；进程重启后，普通 `Login(false)` 仍会零网络请求地返回
`ErrReauthRequired`。只有 fresh credentials 已持久化且所有记录的 `context_token` 已清空后，marker 才会被移除。
迁移自定义状态目录时应将 credentials 与该 sidecar 作为同一状态集；marker 活跃期间不应直接降级到不识别它的 SDK 版本。

## 文档

- [API 协议参考](docs/protocol.md)
- [架构说明](docs/architecture.md)

## 来源与同步范围

- 初始 Go 实现来源：`github.com/corespeed-io/wechatbot`
- 协议参考：`github.com/Tencent/openclaw-weixin`
- 本仓库独立维护，变更不会自动与上述仓库双向同步
- 许可证：MIT（保留原项目版权声明）

## 贡献

本仓库相关的 Issues 和 PR 可直接提交到当前仓库。

## License

[MIT](LICENSE)
