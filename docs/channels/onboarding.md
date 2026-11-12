# 机器人扫码接入

钉钉、飞书/Lark、企业微信智能机器人和 QQ 现在提供显式的扫码注册流程：`BeginRegistration → Challenge → Wait → Credentials.Config`。该流程申请平台授权链接，在用户确认后取得机器人凭据；已有消息适配器继续负责收发。

## 与原微信扫码流程的关系

两类流程都有“取得授权链接 → 用户扫码确认 → 轮询结果 → 获取凭据”四步，但管理范围不同。微信根包的 `Bot.Login` 管理用户会话；新增 API 为机器人申请或绑定平台凭据，并把候选结果交回宿主。

| 行为 | 原微信 `Bot.Login` | 新增机器人注册接口 |
|---|---|---|
| 已有凭据 | 非强制登录先复用内存/磁盘凭据；损坏文件报错 | `BeginRegistration` 显式新建授权，已有凭据由宿主读取后传入 adapter |
| 二维码和扫码通知 | `OnQRURL`、`OnScanned`；微信要求时调用验证码输入回调 | `Challenge` 提供授权链接；当前协议没有统一“已扫码”事件或 SDK 验证码输入步骤 |
| 过期处理 | 微信协议下最多申请三次二维码 | 返回 `ErrExpired`，由宿主明确决定是否重新申请，不自动创建新机器人 |
| 域切换 | 微信 IDC 重定向 | 飞书按租户品牌切换到固定官方 Lark 账号域名；其他通道没有复制 IDC 逻辑 |
| 凭据与连接 | 根包负责持久化及验证后的会话安装，强制重新认证先使旧会话失效 | `Wait` 返回候选凭据，`Config` 只构造配置；宿主验证身份、保存成功后再创建/替换连接 |
| 取消和失败 | 取消不安装候选会话，重新认证失败后旧凭据仍保持失效 | 取消停止本次等待；不操作宿主现有 adapter。网络错误不自动重试，可在原期限内明确再次 `Wait` |
| 成功后的重复获取 | 后续登录可复用已保存的凭据 | 同一注册对象保留成功结果；即使二维码随后过期，也可再次 `Wait` 取回用于重试保存，不重新请求平台 |

`Credentials.Config(accountID)` 的参数是**本地账号标识**，不是微信 `Options.AccountID` 那样的期望平台机器人身份。宿主若要重新绑定既有账号，需先比对原有 `ClientID/AppID/BotID`，飞书还需比对 `Domain`；确认身份及授权范围后再持久化和安装，避免让另一机器人继承旧账号的权限。注册 API 不代替这一步，也不会自动把扫码用户加入白名单。

因此，“获得凭据”这段链路已经齐全，但不能据此声称四个平台都完成了真实账号接入验收。QQ 的 HTTPS 回调、飞书群聊机器人身份、平台权限等仍遵循各自 adapter 文档；UI 和真实扫码由宿主使用者验收。

## SDK 接入

```go
registration, err := dingtalk.BeginRegistration(ctx, dingtalk.RegistrationOptions{})
if err != nil { return err }

challenge := registration.Challenge()
// 宿主负责将 URL 本地绘制成二维码，并仅向当前绑定用户展示。
showQRCode(challenge.URL)

credentials, err := registration.Wait(ctx)
if err != nil { return err }
// 重新绑定旧账号时先验证平台身份，再安全持久化并创建适配器。
adapter, err := dingtalk.New(credentials.Config("customer-a-dingtalk"))
if err != nil { return err }
```

飞书使用 `feishu.BeginRegistration(ctx, feishu.RegistrationOptions{})`；国际版使用 `Domain: "lark"`。`Credentials.Config(accountID)` 设置正确的 API 域名与 WebSocket 模式。用户 OpenID 与机器人 BotOpenID 不同；扫码用户身份可用于宿主授权策略，不能直接当成群聊 @ 提及的机器人身份。

企业微信使用 `wecom.BeginRegistration(ctx, wecom.RegistrationOptions{})`，得到 `BotID/Secret`；`Credentials.Config(accountID)` 用于现有智能机器人长连接适配器。这个流程不创建企业自建应用，也不返回 CorpID/AgentID/CorpSecret。

QQ 使用 `qqbot.BeginRegistration(ctx, qqbot.RegistrationOptions{})`，得到 `AppID/ClientSecret` 及可选的扫码用户 `UserOpenID`。`Credentials.Config(accountID)` 用于现有 Webhook 适配器；仍须挂载并在平台配置 HTTPS 消息回调，扫码成功不代表消息连接或权限已验证。扫码用户身份仅供宿主决定授权范围，不会自动加入白名单。

SDK 返回授权 URL，不生成二维码图片、不打开浏览器、不调用第三方制码服务。私有设备凭据和 QQ 解密密钥留在注册对象内部，授权 URL 本身可能含平台任务标识，仍须保密。每个注册对象只能有一个 `Wait`；`Challenge.ExpiresAt` 同时限制等待和正在执行的请求。钉钉/飞书使用服务端有效期；企微/QQ 未返回有效期，分别使用 5 分钟和默认 10 分钟的本地等待上限，并非平台承诺的二维码有效期。QQ 可通过 `RegistrationOptions.Timeout` 调整等待上限。context 取消或网络错误直接返回，调用方可在原期限内显式再次 `Wait`；成功、拒绝、过期和无效协议响应会保留结果。不会自动刷新二维码、重新创建机器人或重试失败状态。

## 本地注册示例

```powershell
go build -o channel-login.exe ./examples/channel-login
.\channel-login.exe -platform dingtalk -account support-dingtalk -output "$env:LOCALAPPDATA\support-dingtalk.json"
.\channel-login.exe -platform wecom -account support-wecom -output "$env:LOCALAPPDATA\support-wecom.json"
.\channel-login.exe -platform qqbot -account support-qq -output "$env:LOCALAPPDATA\support-qq.json"
# 飞书或国际版：将 platform 改成 feishu 或 lark，并使用不同的 account/output。
```

示例在终端显示授权链接与到期时间，取得凭据后写入指定的新文件。输出目录必须已存在；目标文件存在时会拒绝执行，授权期间出现同名文件也不会覆盖。凭据 JSON 含 `platform`、`account_id` 和 `credentials`，后者对应平台的 `Credentials` 类型；Lark 使用 `platform: "feishu"` 和 `credentials.domain: "lark"`。

示例在提交凭据文件前再次检查取消状态；提交检查时若已取消，直接返回并保留原有文件。示例不打印密钥、不自动连接或发消息。文件创建使用 `0600`；Windows 上实际访问控制由目录的 ACL 决定，应放在该用户的私有数据目录。库不替宿主选择凭据存储位置，也不会写入 `.env` 或替换已有账号。

## 协议与验证范围

- 钉钉：默认采用当前官方 connector 的 `DING_DWS_CLAW` 来源模板，可通过 `Source` 显式指定平台认可的其他模板。接口与依据见 [钉钉](dingtalk.md)。
- 飞书：采用官方 SDK 的 `expires_in` 字段，支持 HTTP 400 `authorization_pending`、`slow_down` 增加后续轮询间隔，以及固定官方域名间的 Lark 识别切换。接口与依据见 [飞书](feishu.md)。
- 企微：采用官方 `wecom-cli` 的 `ai/qc` 生成和查询流程，接口属于扫码接入端点，不能视作稳定的公开 OpenAPI 契约。接口与依据见 [企微智能机器人](wecom.md)。
- QQ：采用官方 connector 的创建绑定任务、轮询和 AES-256-GCM 解密协议，默认 Source 为空（第三方机器人）。接口与依据见 [QQ](qqbot.md)。
- 回归覆盖 HTTP 请求契约、不透明凭据原值、服务端有效期与时长溢出、降速、取消、拒绝、同会话并发等待、结果保留、错误脱敏、禁重定向、QQ 凭据完整性校验、成功结果在二维码过期后保留、取消后不开始落盘及凭据文件不覆盖。
- 企微仅探测过一次未扫码的生成/查询响应；未执行真实平台扫码创建、授权或 UI 验收；平台权限、模板可用性及群聊配置仍需目标账号验收。

本次没有引入平台 SDK、二维码依赖或兼容旧字段的分支；根包微信登录流程保持原有实现。
