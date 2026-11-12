# 国内多通道 Gateway

这是一组可选的多通道接入实现，不替换根包微信 SDK，不修改 module path、go.mod/go.sum、现有登录/游标/重放/媒体 API，也不引入平台 SDK 或 Agent Runtime。

## 阅读入口

- [架构与八组 PR](architecture.md)
- [参考库与协议来源](references.md)；[本轮核对补充](research-notes.md)
- [能力矩阵与非目标](capabilities.md)
- [消息内容、附件传输与部分成功](media.md)
- [按 Bot 配置接收类型与文件白名单](receive-policy.md)
- [飞书互动卡片：发送按钮、回传参数与回调配置](feishu-cards.md)
- [钉钉 / 飞书 / 企微 / QQ 扫码接入](onboarding.md)
- [JZBeacon / pi-go 集成边界](integration.md)
- [验收与限制](verification.md)
- 通道：[飞书](feishu.md)、[钉钉](dingtalk.md)、[企微智能机器人](wecom.md)、[企微自建应用](wecomapp.md)、[QQ](qqbot.md)、[微信兼容桥](weixin.md)

## 评审顺序

| 顺序 | 分支 / PR | 范围 |
|---|---|---|
| 1 | feat/domestic-gateway-01-core / #11 | Gateway 契约、路由、授权、接入去重 |
| 2 | feat/domestic-gateway-02-transport / #12 | 原生 HTTP/WS、限额、缓存与重连 |
| 3 | feat/domestic-gateway-03-feishu / #13 | 飞书 WS / Webhook |
| 4 | feat/domestic-gateway-04-dingtalk / #14 | 钉钉 Stream |
| 5 | feat/domestic-gateway-05-wecom / #15 | 企微智能机器人与流式回复 |
| 6 | feat/domestic-gateway-06-wecomapp / #16 | 企微自建应用回调 |
| 7 | feat/domestic-gateway-07-qqbot / #17 | QQ API v2 |
| 8 | feat/domestic-gateway-08-integration / #18 | iLink 兼容桥、示例、跨层文档与验证记录 |

提交时采用 stacked PR：后一个 base 是前一个 feature 分支。审核按表中顺序将前序修复带入后续分支，再逐一 retarget 到 main、运行检查并合并。验证边界见 [验收记录](verification.md)，真实平台账号验收和发布仍是独立步骤。

## 本地连接探针

`examples/multi-channel` 同时装配国内新通道，环境变量决定启用哪些平台。需要完整仓库与 Go1.25+。

```bash
export FEISHU_APP_ID='your-app-id'
export FEISHU_APP_SECRET='your-app-secret'
export FEISHU_ACCOUNT_ID='support-bot'
export CHANNEL_ALLOW_JSON='[{"platform":"feishu","account":"support-bot","tenant":"your-tenant-key","user":"your-open-id"}]'
export CHANNEL_DEMO_ACK=1
go build -o multi-channel ./examples/multi-channel
./multi-channel
```

**仅用于测试机器人。探针只打印消息元数据并 ACK，不存储消息、不调用 Agent、不回复。** 因而要求显式 CHANNEL_DEMO_ACK=1。生产必须替换 Handler 为可幂等持久接入的 inbox。不要把探针接到客户生产消息。

默认 HTTP 只绑定 127.0.0.1:8640，公网回调用 HTTPS 反向代理；支持 /callbacks/feishu、/callbacks/wecom-app、/callbacks/qq。healthz 仅表示进程存活，不是通道 readiness。长连接平台无需公网入口。

示例使用的凭证变量见 main.go；白名单按 platform/account/tenant/user 精确匹配，空 tenant 不代表通配符。支持多账号的 SDK 通过多次 Register 实现，示例仅装配每平台一个账号。微信需要显式登录和 handler 所有权，见 weixin.md。
