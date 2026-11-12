# 现有微信 iLink SDK 的兼容桥

`channels/weixin` 是原有根包的适配器，而不是 iLink 重新实现。根包的登录、验证码交互、reauth marker、cursor、replay、引用缓存、媒体能力及错误类型保持不变。

```go
native := wechatbot.New(wechatbot.Options{AccountID: actualWeChatAccountID})
// 应用使用原来的 Login / Reauthenticate 和扫码/验证码回调。
if _, err := native.Login(ctx, false); err != nil { return err }
adapter, err := weixin.New(weixin.Config{
    AccountID: "customer-a-wechat",
    Bot: native,
    TakeHandlerOwnership: true,
})
if err != nil { return err }
if err := g.Register(adapter); err != nil { return err }
return g.Run(ctx)
```

Gateway AccountID 是本地连接标识，原 Options.AccountID 是期望的实际微信账号，两者用途不同。应用必须把它们绑定到同一安装/租户，并隔离原 SDK 所有磁盘路径。每个 Bot 只能属于一个 Adapter，不要同时调用 native.Run，也不要之后再次覆盖 native.Handle。

显式 TakeHandlerOwnership 防止无意覆盖已有 Handler。成功接入映射 AckMessage，明确拒绝/不支持映射 DropMessage，暂时失败映射 RetryMessage；不吞掉原 SDK 的 ErrReauthRequired。桥不会自动重新扫码登录。

入站 MessageID 保持无损字符串，缺失时用原始消息的稳定摘要作为本地标识。SessionID 进入 ThreadID。媒体消息正文只从文本/语音文本项提取，避免原 SDK 聚合 Text 中的签名媒体 URL 进入默认 JSON；完整消息保留在 Native，禁止盲目序列化/打印。

发送通过缓存的原生 IncomingMessage 调用 ReplyMessage，保留未导出的会话代次/context_token。缓存保存完整对象副本，不用纯 JSON 重建授权上下文。ClientID 与原 RunID 传给 OutboundMessage；原 SDK 继续做会话校验和错误处理。缓存过期/重启后返回 ErrReplyExpired，不改发其他聊天。

当前统一桥只提供 direct 文本回复；GroupID 可以保留为入站路由元数据，但发送群消息明确 Unsupported，不能把群回复意外发送给某个个人。媒体发送、下载和引用附件仍可通过原 SDK 使用，尚未归一化到此桥。

本轮已编写 8 项桥接契约测试：显式所有权、上下文/ClientID/大 ID、Native 标量变更隔离、跨 Route 拒绝、Ack/Retry/Drop、秘密 JSON、媒体 URL、群发送拒绝。当前环境无 Go1.25 与完整原包依赖，因此这些测试尚未执行，不能与新标准库通道的已通过测试混算。
