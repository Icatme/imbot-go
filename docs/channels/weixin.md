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

入站 MessageID 保持无损字符串；空 ID 或零 ID 依次使用消息项 ID、ClientID、Seq，最后使用排除 context_token 的原始消息摘要作为本地标识，避免零 ID 导致不同消息被去重或回复凭证刷新改变事件身份。SessionID 进入 ThreadID。媒体消息正文只从文本/语音文本项提取，避免原 SDK 聚合 Text 中的签名媒体 URL 进入默认 JSON；完整消息保留在 Native，禁止盲目序列化/打印。

发送通过缓存的原生 IncomingMessage 调用 ReplyMessage，保留未导出的 Bot 实例、会话代次/context_token。原生消息及 handler context 跨 Bot 使用时返回 ErrSessionChanged，即使两个实例的会话代次数值相同也不例外。缓存保存完整对象副本，不用纯 JSON 重建授权上下文。ClientID 与原 RunID 传给 OutboundMessage；原 SDK 继续做会话校验和错误处理。缓存过期/重启后返回 ErrReplyExpired，不改发其他聊天。

统一桥提供 direct 文本与已上传图片/文件/视频发送，并复用根 SDK 实现显式 Download、Upload 和引用 DownloadQuoted。GroupID 保留为入站路由元数据，发送群消息明确 Unsupported。原生会话检查仍由根 SDK 执行；跨 Route、过期资源和原生错误不会触发降级或重试。协议与持久消费限制见[消息与媒体契约](media.md)。

回复上传使用 `UploadForReply` 保留入站会话代次；后续发送通过 `OutboundMessage.Upload` 绑定原 Bot、用户与资源，重新登录后旧资源明确失败，不借用新会话上传或发送。当前图片通过 `DownloadImage` 区分加密 CDN 与 URL 形式，其他附件通过 `DownloadRaw` 下载，避免多附件污染原生单附件引用缓存；宿主自行持久保存这些字节。引用恢复仍使用 `DownloadQuoted`。发送前复制缓存资源，隔离原生 hook 修改；Parts 保留语音识别文字，无音频资源时不制造附件 Ref。

桥接契约测试覆盖显式所有权、上下文/ClientID/大 ID、零 ID/重投身份、Native 标量变更隔离、跨 Route 拒绝、Ack/Retry/Drop、秘密 JSON、媒体 URL、群发送拒绝。审核时已在真实仓库执行这些测试；真实微信账号验收仍由应用完成。
