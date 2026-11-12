# 协议与参考实现登记（仅参考，不作为依赖）

核对日期：2026-11-12。优先采用平台协议，其次对照当前官方跨语言 SDK，再参考 Hermes 的网关分层。以下链接是研究来源，不是运行时依赖。未确认的功能不依据 SDK 年龄臆测支持状态。

| 来源 | 参考点 | 使用边界 |
|---|---|---|
| [Hermes Gateway](https://github.com/NousResearch/hermes-agent/blob/main/gateway/platforms/ADDING_A_PLATFORM.md) 与 [事件模型](https://github.com/NousResearch/hermes-agent/blob/main/gateway/platforms/event.py) | Adapter 注册、统一事件、可选能力、协议与行为分离 | 自行设计 Go API；不移植庞大 Python 基类，不导入 Agent Runtime |
| [飞书 Node SDK](https://github.com/larksuite/node-sdk)，[Channel 文档](https://github.com/larksuite/node-sdk/blob/main/docs/channel.zh.md) | 当前高层消息归一化与传输能力对照 | 不依赖 Node 或旧 Go SDK |
| [飞书 Python WS](https://github.com/larksuite/oapi-sdk-python/blob/v2_main/lark_oapi/ws/client.py) | endpoint discovery、控制帧、分包、数据 ACK | 以 wire 协议重新实现；不复制 SDK 源码 |
| [飞书 Go WS 帧字段](https://github.com/larksuite/oapi-sdk-go/blob/v3_main/ws/pbbp2.pb.go) | Frame/Header 字段编号和 wire type；核对 blob a6416df7f299b3adb1f37df108cbeda623c298b8 | 不引用 gogo/protobuf、不复制生成代码 |
| [钉钉 Python Stream](https://github.com/open-dingtalk/dingtalk-stream-sdk-python/blob/main/dingtalk_stream/stream.py) | connections/open、订阅 topic、消息与 SYSTEM ACK、token | 核对 blob 065eb32d916ba342a715eb920251734d8045adc6；不依赖 SDK |
| [钉钉 Go Stream](https://github.com/open-dingtalk/dingtalk-stream-sdk-go) | Go 生态对照入口 | 候选资料，不作为平台能力的唯一依据 |
| [企业微信官方 Node SDK](https://github.com/WecomTeam/aibot-node-sdk) | subscribe/heartbeat、req_id 回执、流式完整快照、主动发送 | 优先对照官方新协议；媒体上传等未覆盖能力明确列出 |
| [企业微信开发文档](https://developer.work.weixin.qq.com/document/) | 智能机器人长连接和自建应用是不同身份/协议 | 分两个 Adapter，不混淆群机器人 Webhook 与自建应用 |
| [QQ 官方文档](https://bot.q.qq.com/wiki/develop/api-v2/)，[签名算法](https://bot.q.qq.com/wiki/develop/api-v2/dev-prepare/interface-framework/sign.html) | Ed25519、timestamp + 原始 body、挑战签名 | 必须先验签，不能仅验 op=13 或忽略普通事件 |
| [BotGo](https://github.com/tencent-connect/botgo)、[BotPy](https://github.com/tencent-connect/botpy)、[Node SDK](https://github.com/tencent-connect/bot-node-sdk) | 跨语言历史行为对照与兼容差异 | 旧 SDK/WebSocket 可用性不能代表新账号；当前优先 Webhook |
| [RFC 6455](https://www.rfc-editor.org/rfc/rfc6455) | WebSocket 握手、客户端 masking、控制帧/fragment | 最小原生客户端，无压缩扩展；应用不得自行拼帧 |
| [corespeed-io/wechatbot](https://github.com/corespeed-io/wechatbot) 与本仓库 v0.7.0 | iLink 原有实现与生命周期 | 保留现有实现；新增桥接而非重写认证与游标 |

## 已识别的兼容差异

- 飞书长连接是二进制 protobuf Frame，不是收到一段 JSON 就能处理；需正确关联 service、SeqID/LogID、分片与 ACK。
- 钉钉 Stream 的 data 是 JSON 字符串，系统 ping/disconnect 与业务回调不能同一方式处理；sessionWebhook 有时效且含秘密。
- 企业微信智能机器人 WS 和自建应用 XML 回调不能互换配置；流式内容是完整快照，不是 token delta。
- QQ 文档与不同代际 SDK 对 WebSocket、域名及开放范围存在差异；端点可配置，API v2 以平台当前控制台为准。不得把文档未取到的内容写成“已实测”。
- 平台签名只认证传输来源；不能代替 JZBeacon 用户/租户/CRM 权限。
