# QQ Bot API v2

核对日期：2026-11-12。使用原生 Go Ed25519 + HTTPS Webhook/REST，不依赖 BotGo/BotPy/Node SDK，不假定旧 WebSocket 链路对新账号可用。

## 接入

`qqbot.New(Config{AccountID, AppID, ClientSecret})`，宿主挂载 `Handler(g.Bind(adapter.Account()))` 到 HTTPS 地址。默认 APIBase 为 https://api.bot.qq.com；TokenURL 为同域 /app/getAppAccessToken。凭证是 AppID/AppSecret，不是旧 Bot Token。

每次回调先校验 X-Signature-Timestamp + 原始请求体的 Ed25519 签名、5 分钟时间窗；不能把 JSON 重新序列化后验签。secret 按协议重复填满 32 字节 seed。op=13 挑战也先验签，再签 event_ts + plain_token；op=1 返回 op=11 心跳；op=0 成功接入后返回 op=12,d=0，接入失败返回 op=12,d=1 请求重投。不能只在 HTTP 头上返回成功而忽略协议级结果。

## 当前功能

接收 C2C_MESSAGE_CREATE、GROUP_AT_MESSAGE_CREATE，区分 user_openid/member_openid/group_openid，按本地 AppID 隔离租户。文本与媒体描述归一化；原始媒体 URL 留在 Native，不自动抓取。

发送 C2C/群文本与已有 file_info 的富媒体；ReplyTo 映射 msg_id，Sequence 映射 msg_seq。默认 Sequence=1；同一入站消息多次回复时，应用 outbox 必须分配稳定递增序号。进程重试不能随意换序号绕过平台去重。

Markdown 与主动发送默认关闭，需要先确认目标机器人权限再显式 EnableMarkdown/EnableProactive。配置开启不意味着平台已授权，额度、被动回复窗口与内容审核以控制台和 API 返回为准。当前没有频道/guild、卡片按钮、媒体上传下载或旧 WebSocket。

## 协议参考与测试来源（非依赖）

- 当前官方 API 使用指南：https://bot.q.qq.com/wiki/develop/api-v2/dev-prepare/interface-framework/api-use.html （页面更新 2026-07-30，API/令牌域名以该页为准）。
- 官方签名规范：https://bot.q.qq.com/wiki/develop/api-v2/dev-prepare/interface-framework/sign.html
- 官方 BotGo Webhook：https://github.com/tencent-connect/botgo/blob/master/interaction/webhook/webhook.go （核对 blob 7b94344942700871532450b3cb0a61203e818e07）。
- 官方固定原始字节验签 fixture：https://github.com/tencent-connect/botgo/blob/master/interaction/signature/interaction_test.go （核对 blob 8a8f234b774cf4e38367fb511f976b25585e0968）。仅引用公开测试输入/预期输出用于互操作校验，没有导入或复制 SDK 实现。
- 跨语言行为对照入口：https://github.com/tencent-connect/botpy 与 https://github.com/tencent-connect/bot-node-sdk 。旧 SDK 方法不作为当前可用权限的证明。

11 项测试在隔离 Go1.23 工作区通过，包括官方 BotGo 固定签名向量、官方公钥向量、改变单字节失败、挑战签名、未签名/过期拒绝、协议 ACK、群路由与 REST 契约。

研究中，签名文档网页展示的另一条签名示例未能用其显示出来的报文字节复现（Go/Python 得到相同但不同于页面展示的结果）；可能与展示空白或示例数据不一致有关，未据此断言官方协议错误，也未更改标准算法来匹配展示。验收使用上述可复核的官方仓库固定字节 fixture。真实 QQ 回调、权限、IP 白名单及全仓 Go1.25 回归尚未验收。
