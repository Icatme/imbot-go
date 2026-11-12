# 飞书互动卡片：发送按钮与接收操作

适用于 imbot-go v0.9.1 的 `channels/feishu` + `gateway`。卡片通过 `gateway.Message.Card` 发送，按钮点击以 `Event.Interaction` 进入业务 Handler。普通 `post` 富文本或 `Format: "markdown"` 不会自动生成回传按钮。

## 是否需要回调链接

**默认长连接模式不需要公网回调地址。** Bot 主动连接飞书，用户点击按钮后，飞书通过这条连接推送 `card.action.trigger`，由 Bot 处理。发送卡片的业务系统不必另建一个 HTTP 回调服务，也不必在每个按钮中填写回调 URL。

| 接收方式 | imbot 配置 | 是否需要公网接收地址 | Bot 侧入口 |
|---|---|---|---|
| 长连接 | `Transport: "websocket"`，也是默认值 | 不需要；运行环境需要能出站访问飞书 | `g.Run(ctx)` 建连，操作进入 `Handler` |
| HTTP 回调 | `Transport: "webhook"` | 需要飞书能够访问的 HTTPS 地址 | 挂载 `adapter.Handler(g.Bind(adapter.Account()))` |

`behaviors.type = "callback"` 表示回传业务数据，数据由飞书按**应用的回调配置**投递；`open_url` 表示跳转网页，其 URL 不是 Bot 的回调地址。两种接收方式最终都使用同一个业务 Handler。[飞书回调概述](https://open.feishu.cn/document/event-subscription-guide/callback-subscription/callback-overview)、[接收回调](https://open.feishu.cn/document/event-subscription-guide/callback-subscription/receive-and-handle-callbacks)、[配置卡片交互](https://open.feishu.cn/document/feishu-cards/configuring-card-interactions)

## 飞书应用配置

1. 为应用启用机器人能力，配置发送消息等所需权限及可用范围。
2. 运行对应 AppID/AppSecret 的 Bot 长连接，再在开发者后台的**事件与回调 → 回调配置**选择“使用长连接接收回调”，添加新版**卡片回传交互 `card.action.trigger`**。
3. 需要接收用户普通消息时，在**事件配置**另行配置 `im.message.receive_v1`。仅订阅普通消息不代表已经订阅按钮回调。
4. 按目标应用的发布流程使配置生效，并在真实账号上验证。扫码获取凭据不等于已经配置了卡片回调。

这里接入的是应用机器人，不是仅有一个群自定义机器人 Webhook URL 的发送接口。本文使用新版 `card.action.trigger`，当前适配器不解析旧版 `card.action.trigger_v1`。[长连接回调配置](https://open.feishu.cn/document/event-subscription-guide/callback-subscription/step-1-choose-a-subscription-mode/configure-callback-request-address)

## Go 接入示例

下面的示例包含 Gateway 装配、按钮数据解析和发送函数。宿主提供普通消息处理函数、权限校验及持久化接收函数；示例不执行审批或任务本身。

```go
package cardexample

import (
    "context"
    "encoding/json"
    "errors"

    "github.com/Icatme/imbot-go/channels/feishu"
    "github.com/Icatme/imbot-go/gateway"
)

// SaveDecision 应校验任务权限/当前状态，并幂等地保存操作或写入持久队列。
// 保存成功才返回 nil；存储失败应返回错误，不能先 ACK 再仅启动内存 goroutine。
type SaveDecision func(ctx context.Context, e gateway.Event, action, taskID string) error

func NewCardBot(
    cfg feishu.Config,
    authorize gateway.Authorizer,
    messages gateway.Sink,
    saveDecision SaveDecision,
) (*gateway.Gateway, *feishu.Adapter, error) {
    if authorize == nil || messages == nil || saveDecision == nil {
        return nil, nil, errors.New("authorization, message handler and decision store are required")
    }
    adapter, err := feishu.New(cfg)
    if err != nil {
        return nil, nil, err
    }
    g, err := gateway.New(gateway.Options{
        Authorize: authorize,
        // 零值 ReceivePolicy 默认允许交互；不要禁用 receive.Interaction。
        Handler: func(ctx context.Context, e gateway.Event) error {
            if e.Interaction == nil {
                return messages(ctx, e)
            }
            // Interaction.Value 是完整 action 对象，不只是 action.value。
            var action struct {
                Tag   string `json:"tag"`
                Value struct {
                    Action string `json:"action"`
                    TaskID string `json:"task_id"`
                } `json:"value"`
            }
            if err := json.Unmarshal(e.Interaction.Value, &action); err != nil {
                return gateway.ErrInvalidEvent
            }
            if action.Tag != "button" || action.Value.TaskID == "" ||
                (action.Value.Action != "confirm" && action.Value.Action != "cancel") {
                // 明确拒绝不属于本示例业务协议的操作，按未授权操作消费。
                return gateway.ErrUnauthorized
            }
            return saveDecision(ctx, e, action.Value.Action, action.Value.TaskID)
        },
    })
    if err != nil {
        return nil, nil, err
    }
    if err := g.Register(adapter); err != nil {
        return nil, nil, err
    }
    return g, adapter, nil
}

// SendDecisionCard 回复一条已被同一适配器接收的消息。
// e.Route 保留真实 Account/TenantID/ChatID/ThreadID/Kind，不自行猜测路由。
func SendDecisionCard(ctx context.Context, g *gateway.Gateway, e gateway.Event, taskID string) (gateway.Receipt, error) {
    button := func(label, action, style string) map[string]any {
        return map[string]any{
            "tag": "button",
            "text": map[string]string{"tag": "plain_text", "content": label},
            "type": style,
            "behaviors": []any{map[string]any{
                "type": "callback",
                "value": map[string]string{"action": action, "task_id": taskID},
            }},
        }
    }
    card, err := json.Marshal(map[string]any{
        "schema": "2.0",
        "header": map[string]any{
            "title": map[string]string{"tag": "plain_text", "content": "任务确认"},
        },
        "body": map[string]any{
            "elements": []any{
                map[string]string{"tag": "markdown", "content": "请选择是否执行此任务。"},
                button("确认执行", "confirm", "primary"),
                button("取消", "cancel", "default"),
            },
        },
    })
    if err != nil {
        return gateway.Receipt{}, err
    }
    return g.Send(ctx, e.Route, gateway.Message{
        ReplyTo: e.MessageID,
        Card:    card,
    })
}
```

卡片 JSON 2.0 将按钮直接放在 `body.elements`，不要套用旧版 `tag: "action"` 容器。`Card` 填写完整卡片 JSON，不再手工添加 `msg_type`、`content` 或最外层 `card`；适配器会编码为飞书 `interactive` 消息。不要同时设置 `Message.Text`、附件或 `Format: "markdown"`。[官方按钮结构](https://open.feishu.cn/document/feishu-cards/card-json-v2-components/interactive-components/button)

长连接配置示意：

```go
cfg := feishu.Config{
    AccountID: "support-feishu",
    AppID:     appID,
    AppSecret: appSecret,
    Transport: "websocket",
    BotOpenID: botOpenID, // 群消息默认要求 @此机器人
}
g, _, err := NewCardBot(cfg, authorize, handleMessage, saveDecision)
if err != nil { return err }
return g.Run(ctx) // 阻塞运行，宿主使用 ctx 管理停止
```

由业务流程调用 `SendDecisionCard`，传入已接收消息的原始 Event。回复绑定及卡片路由都有缓存期限，不要把过期 Event 当作永久发送凭据。主动发送卡片时可省略 `ReplyTo`，但必须提供已知完整路由，尤其是后续回调匹配需要的 `TenantID`；不能只猜一个 ChatID。

## 点击后收到的数据

上面“确认执行”按钮的 `Event.Interaction.Value` 示例：

```json
{
  "tag": "button",
  "value": {"action": "confirm", "task_id": "task-123"}
}
```

| 字段 | 含义 |
|---|---|
| `Event.MessageType` | `card.action.trigger` |
| `Event.SenderID` | 点击者的飞书 OpenID |
| `Event.Route` | 发送原卡片时保存的完整路由 |
| `Event.MessageID` / `Interaction.MessageID` | 原卡片消息 ID |
| `Event.ID` / `Interaction.ID` | 本次回调事件 ID，可用于业务去重 |
| `Interaction.Name` | 组件 tag，例如 `button`，不是自定义业务 action |
| `Interaction.Value` | 完整 `action` JSON，业务参数位于其中的 `value` |

表单、输入框、选择项的 `form_value`、`input_value`、`option`、`options`、`checked` 等字段也保留在原始 action 中，按所用组件结构解析。回调不会自动变成一条用户聊天文字，也不会自动执行任务。消息级 `Authorize` 通过后，宿主仍须检查“此用户能否操作这个任务”，并防止同一任务被重复执行。

## 可选 HTTP 回调

若选择 Webhook，设置 `Transport: "webhook"`、`VerificationToken` 和 `EncryptKey`，再由宿主挂载：

```go
mux.Handle("/callbacks/feishu", adapter.Handler(g.Bind(adapter.Account())))
```

在飞书应用的**回调配置**中选择发送至开发者服务器，填写对应公网 HTTPS 地址，例如 `https://bot.example.com/callbacks/feishu`。该 URL 属于接收 Bot 服务，不是每个按钮上的跳转链接。飞书发起的网址校验、验签及消息解析由 Handler 处理；当前 imbot 的 HTTP 模式要求同时配置 token 和 encrypt key。HTTPS 入口及服务器生命周期由宿主管理，`g.Run` 不会自动监听 HTTP 端口。

## 当前实现边界与排查

- **发送与接收需使用同一个 Adapter 实例。** 成功发送卡片后才建立路由缓存；每实例最多保留 4096 条、30 分钟，进程重启或淘汰后不可用。这是 imbot 的本地缓存期限，不是飞书卡片的官方有效期。不同进程分别负责发送和接收、同凭据多副本随机分流、从其他工具发送卡片，都可能因接收实例没有路由而返回 `ErrReplyExpired`。当前没有跨副本共享路由或自动查询补全机制。
- **Handler 要及时返回。** 飞书要求回调在 3 秒内响应，当前适配器给业务接入 context 设置 2 秒期限；宿主必须遵守取消。耗时任务先幂等持久入队，成功后返回，再由后台执行；不要在此等待 LLM 或长任务完成。[飞书接收回调说明](https://open.feishu.cn/document/event-subscription-guide/callback-subscription/receive-and-handle-callbacks)
- **接收 ACK 不代表任务完成。** 成功消费后适配器返回空 action 响应 `{}`，原卡片保持不变。业务失败应如实返回错误；授权或接收策略拒收会确认消费。Gateway 的内存去重不能替代业务持久化幂等。
- **当前不能通过 Handler 返回新卡片或 toast。** SDK 未暴露事件级卡片 token，也没有高层卡片更新接口。处理结果可另发消息或新卡片；原卡片“已处理”、按钮禁用等状态更新需另行实现。
- **卡片可见但点击无事件：** 先核对回调配置中的新版订阅、AppID、应用可用范围和当前连接，再检查 `receive.Interaction` 是否被禁用、`Authorize` 是否放行，以及接收实例是否仍持有该卡片路由。只配置 `im.message.receive_v1` 不足以收到按钮事件。

以上示例与 SDK API 做本地编译核对，卡片显示、应用权限和真实按钮交互仍由使用者在飞书中人工联调。
