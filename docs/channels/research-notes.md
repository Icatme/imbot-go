# 本轮协议核对补充

核对日期：2026-11-12。基础参考列表见 references.md，各平台细节见对应文档。以下均为协议/行为参考，不导入 SDK、不 vendor、不复制实现。公开验签 fixture 只作为互操作测试输入与预期输出，来源明确标注。

| 来源 | 核对文件 / 标识 | 实际用途 |
|---|---|---|
| 飞书 Python | lark_oapi/ws/client.py | 二进制帧、控制帧、回执与分包 |
| 飞书 Go | ws/pbbp2.pb.go，blob a6416df7f299b3adb1f37df108cbeda623c298b8 | wire 字段编号；未复制生成文件或引入 protobuf SDK |
| 飞书 Node | larksuite/node-sdk 的 docs/channel.zh.md | 当前高层 Channel 能力对照；未把所有能力标成已实现 |
| 钉钉 Python | stream.py blob 065eb32d916ba342a715eb920251734d8045adc6；frames.py blob 20c28ea8f3175dd9f2552f41b6d9eec28df9c8ec | endpoint/订阅、JSON 字符串 data、ACK |
| 企业微信官方 Node | WecomTeam/aibot-node-sdk/src/ws.ts blob 1e24e29420154b1e460dfa31e65fb8033e5d31e9 | req_id 队列、回执、接管断连 |
| 企业微信官方 Node | src/types/api.ts blob 8c292a9b772fbe0278ec7d41eb26f96240287966；message.ts blob dffcde3abad8562f855244fadb7c06962e4d9cf4 | 完整快照 stream、消息结构与秘密字段 |
| 企业微信官方 Node | src/client.ts blob c3eaa945079f5f6145fc2684f55a5906530fe92f | 主动发送与三阶段媒体上传对照；初版 Gateway 未实现媒体上传，v0.8.5 已补齐 |
| Hermes Python | plugins/platforms/wecom/wecom_crypto.py blob 5e09fa419f08f1f3ad5cd69ab51945970b92e908 | 企业应用 AES-CBC wire 兼容核对 |
| BotGo | interaction/webhook/webhook.go blob 7b94344942700871532450b3cb0a61203e818e07 | challenge 与 op=12,d=0/1 回执 |
| BotGo | interaction/signature/interaction_test.go blob 8a8f234b774cf4e38367fb511f976b25585e0968 | 固定字节签名 fixture |
| QQ 当前官方文档 | api-use.html 页面更新 2026-07-30 | api.bot.qq.com 与同域 access token 端点 |
| 本仓库 v0.7.0 | outbound.go blob 305fc1fe3bce37303c635ab75bcd80531cd2dae4 | 原 ReplyMessage、会话代次、ClientID，兼容桥不重写 |

## 特性不匹配的处理

本次媒体与交互扩展另核对了腾讯官方 [分片上传协议](https://github.com/tencent-connect/qqbot-nodejs/blob/main/src/protocol/api/media-chunked.ts)、[消息和交互 API](https://github.com/tencent-connect/qqbot-nodejs/blob/main/src/protocol/api/messages.ts)、[附件与 ASR 类型](https://github.com/tencent-connect/qqbot-nodejs/blob/main/src/protocol/types.ts)及 [BotGo 交互类型](https://github.com/tencent-connect/botgo/blob/master/dto/interaction.go)。QQ 原生上传使用协议要求的 MD5/SHA-1；按钮事件同时核对外层 command 与内层 inline-keyboard 类型，避免用自造样例循环证明实现。

企微 [官方业务解析器](https://github.com/WecomTeam/wecom-openclaw-plugin/blob/main/src/message-parser.ts)与 [官方 SDK 事件类型](https://github.com/WecomTeam/aibot-node-sdk/blob/main/src/types/event.ts)存在卡片字段层级差异，因此交互 Value 直接保留认证过的原生 event 对象，不做猜测性字段映射，嵌套卡片、表单选项和已存在的顶层字段均不丢失。

不把“SDK 最后提交很久以前”直接等同于不可用，也不把“新 SDK 有方法”当作目标账号已获授权。本轮以 wire 协议和当前文档为准，保留端点配置，测试真实报文形状。QQ 老 WebSocket 与新 Webhook 区别、企微两类身份、钉钉 ACK data 字符串、飞书 protobuf 等均在代码中分开处理。

QQ 网页展示签名的可复现性疑问完整记录在 qqbot.md；当前官方公钥和官方仓库固定报文测试已通过，不隐瞒失败样例。其余平台的 mock 通过仅说明本地协议处理，不证明生产账号权限、灰度范围、重投窗口或限流额度。

参考入口：
- https://github.com/NousResearch/hermes-agent
- https://github.com/larksuite/node-sdk
- https://github.com/larksuite/oapi-sdk-python
- https://github.com/larksuite/oapi-sdk-go
- https://github.com/open-dingtalk/dingtalk-stream-sdk-python
- https://github.com/open-dingtalk/dingtalk-stream-sdk-go
- https://github.com/WecomTeam/aibot-node-sdk
- https://github.com/tencent-connect/botgo
- https://github.com/tencent-connect/botpy
- https://github.com/tencent-connect/bot-node-sdk
- https://bot.q.qq.com/wiki/develop/api-v2/dev-prepare/interface-framework/api-use.html
- https://bot.q.qq.com/wiki/develop/api-v2/dev-prepare/interface-framework/sign.html
