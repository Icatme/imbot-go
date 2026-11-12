# 按 Bot 配置接收内容

`receive.Policy{}` 默认允许平台已经支持的全部入站内容。不配置即可保持原行为；策略只能缩小接收范围，不能启用平台不支持的能力。`Capabilities` 继续描述适配器能力，接收策略不修改它。

```go
import "github.com/Icatme/imbot-go/receive"

policy := receive.Policy{
    Disabled:       []receive.Kind{receive.Voice, receive.Video},
    FileExtensions: []string{"pdf", ".docx", "xlsx"},
}
```

此配置接收文字、图片和上述文件，拒收语音、视频。关闭所有文件可设置 `Disabled: []receive.Kind{receive.File}`；只关闭图片可使用 `receive.Image`。

| 类型 | 范围 |
|---|---|
| `receive.Text` | 原始文字；语音识别结果仍归语音 |
| `receive.Image` | 图片，包括飞书 sticker |
| `receive.Voice` | 语音、audio 附件、只有转写而没有音频资源的语音 |
| `receive.Video` | 视频，包括飞书 media |
| `receive.File` | 平台归类为文件的附件 |
| `receive.Interaction` | 已支持的卡片、按钮交互事件 |
| `receive.ToolCall` | 原生微信工具调用事件 |

## 原生微信 Bot

```go
bot := imbot.New(imbot.Options{
    ReceivePolicy: policy,
    OnReceiveRejected: func(ctx context.Context, msg *imbot.IncomingMessage, why *receive.Rejection) {
        // 可记录 why.Reason / why.Kind，或使用 msg 回复明确提示。
        // SDK 不自动发提示；回调应及时返回并遵守 ctx 取消。
    },
})
```

策略在引用恢复后、当前消息写入引用缓存以及 `AfterReceive` / middleware / Handler 之前执行。被拒消息正常提交重放记录及批次游标；不触发业务 Handler 或 `OnError`。`OnReceiveRejected` 保留回复所需的 Bot/会话绑定。已有上下文凭据仍正常更新。

新的引用缓存保存原始类型集合，区分混合消息中的真实文字和语音转写。已有缓存仍可读取；旧记录未保存原始类型时，将其中非空文字按 `Text` 检查，并单独检查已知媒体类型，不猜测这些旧文字是否来自转写。

`imbot.New` 保持原返回类型；非法策略会由 `Run` 在开始轮询前报 `receive.ErrInvalidPolicy`。需要在登录前检查动态配置时，可显式调用 `policy.Validate()`。

## 多通道 Gateway

```go
g, err := gateway.New(gateway.Options{
    Handler:   handleMessage,
    Authorize: authorizeMessage,
    ReceivePolicy: policy, // 所有未单独配置的账号使用此策略
    AccountReceivePolicies: map[gateway.Account]receive.Policy{
        {Platform: "feishu", ID: "text-bot"}: {
            Disabled: []receive.Kind{
                receive.Image, receive.Voice, receive.Video,
                receive.File, receive.Interaction, receive.ToolCall,
            },
        },
        {Platform: "qqbot", ID: "general-bot"}: {}, // 显式恢复全部支持内容
    },
    OnReceiveRejected: func(ctx context.Context, event gateway.Event, why *receive.Rejection) {
        // 按场景记录拒收原因或发送提示。
    },
})
```

账号键同时包含平台和 Bot ID。账号策略**完整替换**默认策略，不叠加；不存在账号条目时才使用默认策略。创建时复制配置及切片，后续修改传入的 map/slice 不会热更新运行中的 Bot。`gateway.New` 直接拒绝非法配置。

先执行业务 `Authorize`，再执行内容策略，因此不会向未授权用户触发内容拒收通知。`Accept` 返回 `*receive.Rejection`，可用 `errors.Is(err, receive.ErrRejected)` 判断；`gateway.Consumed(err)` 对该错误返回 true。内置通道据此确认消费，不要求平台重投。重复投递在 Gateway 去重有效期内不会重复触发拒收回调；这仍是内存去重，不是跨进程的持久保证。

回调 panic 属于宿主错误，会按失败处理，不能当作已经完成拒收。回调若发送提示，仍须自行处理发送失败；SDK 不隐式重试。

微信桥可以使用 Gateway 的账号策略；其原生 Bot 若也配置了策略，两层都会执行，先通过原生策略后才到 Gateway。需要统一管理时，在 Gateway 配置，原生 Bot 保持零值。

## 文件筛选与内容边界

- `FileExtensions` 只作用于文件类型；空列表不限制。忽略大小写，允许带或不带前导点，只匹配最终扩展名，因此 `report.pdf.exe` 不会匹配 `pdf`。不接受通配符或复合扩展名配置。
- `FileMIMETypes` 是可选的精确 MIME 白名单，如 `[]string{"application/pdf"}`。不接受通配符；平台 MIME 的参数在比较时忽略。扩展名和 MIME 同时配置时，两项都必须通过。
- 启用白名单后，缺少对应文件名或 MIME 的附件会拒收。许多平台不提供 MIME，原生微信当前文件也没有入站 MIME；不要为这些 Bot 配置无法满足的 MIME 白名单。
- 图片/语音/视频按平台原始类型识别；作为普通文件发送的内容受文件规则约束。扩展名及 MIME 都只是元数据，**不证明实际字节格式**。显式下载后，宿主仍应校验字节大小、真实格式和解析安全性；SDK 不为判型自动下载或执行文件。
- 混合消息采用**整条拒收**：任何部分或引用内容触发禁用规则，正文和其余附件也不会进入 Handler，不裁剪消息。引用检查只针对平台已提供或本地已经恢复的内容；仅有引用 ID 时，不自动联网补取，也不能推断被引用内容的类型。
- 策略限制业务接收，不阻止平台推送回调，也不移除适配器为解析回调建立的短期资源缓存。现有发送、上传和可信宿主显式下载 API 不受接收策略控制。

自定义适配器若把语音转写等内容投影成 `Text`，应填充 `Event.ContentKinds` / `Quote.ContentKinds`，保留所有原始类型。非空列表用于区分真实文字和转写文字，附件仍会独立检查；留空则从 Text、Parts、Attachments 和 Interaction 推断。不要将 `MessageType` 的平台枚举直接写入该列表。
