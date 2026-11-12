# 本轮验证记录与合并门槛

## 已执行

隔离标准库工作区使用环境自带 Go1.23.2，测试本轮新写的标准库包，没有修改真实仓库 go.mod/go.sum：

```bash
go test -race ./gateway ./channels/internal/wire ./channels/feishu \
  ./channels/dingtalk ./channels/wecom ./channels/wecomapp ./channels/qqbot \
  ./examples/multi-channel
go vet ./gateway ./channels/internal/wire ./channels/feishu \
  ./channels/dingtalk ./channels/wecom ./channels/wecomapp ./channels/qqbot \
  ./examples/multi-channel
```

72 项顶层测试通过：Gateway 10、wire 11、飞书 9（包含最终回复路由加固）、钉钉 8、企微 WS 11、企微应用 9、QQ 11、示例授权策略 3。包含真实本地 HTTP Upgrade/WS 收发与并发回执测试、固定协议数据、畸形帧、签名/加解密、授权/去重/隔离、失败回执和主动发送 mock。

## 未执行，不能算通过

当前环境没有 Go1.25 工具链和完整原 SDK 依赖，不能执行真实仓库 `go test ./...`、原 iLink 回归或 `channels/weixin` 的 8 项新增测试，也没有 Windows/macOS 完整编译验证。桥接接口逐项对照原源码并有静态接口断言，但不等于完成编译/运行验证。

没有使用任何真实飞书、钉钉、企业微信、QQ 机器人账号，也未执行生产部署、实际主动消息发送或权限变更。不要把 mock 通过写成平台认证或生产可用承诺。

## Codex / 审核环境合并前检查

1. 按 #11 → #12 → #13 → #14 → #15 → #16 → #17 → 最后一组 PR 顺序审核；保留 stacked bases，必要时 retarget/rebase。
2. 完整 checkout 最后一分支，用 Go1.25+ 对新增 Go 文件执行 gofmt，再跑 `go vet ./...`、`go test ./...`、`go test -race ./...`。上传稿有部分紧凑格式，不能把隔离工作区的 gofmt 当作远端全树已格式化。
3. 原 SDK 的登录/reauth、cursor/replay、并发 Handler、引用缓存、媒体与大消息 ID 回归必须保留；确认根包旧文件与 go.mod/go.sum 没有被意外替换。
4. 各平台真实账号验收：正常文本、群/私聊权限、错账号/错租户拒绝、重复回调、Sink 故障、断线重连、取消退出、发送超时/负回执、平台限流。
5. 飞书验证真实 protobuf 分包/ACK 与 webhook；钉钉验证 SYSTEM data/ACK、sessionWebhook 期限；企微验证接管事件、stream 收尾与 req_id 回执；QQ 验证当前控制台回调/令牌域名及被动回复窗口。
6. 上线前接入真正的持久 inbox/outbox、Bot 单 owner 约束与监控，示例探针不能替代生产业务接入。

本轮所有 PR 保持 Draft，提交带 [skip ci]；未自动合并、发布或部署。CI 跳过不代表检查通过。
