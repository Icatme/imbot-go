# 企业微信自建应用

`channels/wecomapp` 的平台名为 wecom_app；配置 AccountID、CorpID、CorpSecret、AgentID、Token、EncodingAESKey。它不是 wecom 智能机器人，也不是单向群机器人 Webhook。一个 Adapter 对应一个确定的企业应用，不在验证失败时猜测另一个应用。

宿主把 Handler(gateway.Bind(adapter.Account())) 挂载到受 HTTPS 保护的地址；Run 仅等待生命周期取消。支持 GET 加密 echostr 验证、POST 加密 XML、SHA1 签名、5 分钟时间窗、AES-256-CBC/32 字节 PKCS#7、CorpID 尾部校验与解密后 AgentID/ToUserName 检查。内外 XML 均要求单个 `xml` 根元素，限制体积/深度，拒绝根元素外的正文、额外根元素与 DTD 指令，不解析外部实体。媒体回调必须包含 MediaId。

仅在 Sink 完成或持久接受后返回 success，失败返回 503。未知消息类型认证后通过 OnError 或默认 slog 报告再消费；事件不会擅自转换成控制指令。

文本、Markdown 与已有 media_id 的 image/file/voice/video 可发送。文本/Markdown 按 UTF-8 字节限制 2048；不悄悄截断。REST message/send 是向员工主动发送，ReplyTo 只是业务回复关联，不生成原生引用气泡。只允许当前 CorpID 的 direct 路由，拒绝 @all、多收件人分隔符、群聊及跨租户。即使 errcode=0，也检查 invaliduser/unlicenseduser。

已提供 media/get 下载、视频 URL 下载与 multipart 上传，资源绑定完整 Route；详见[媒体契约](media.md)。暂未实现群会话 API、模板卡片和被动 XML 回复。原生回复接口不是异步 Agent 的执行队列。平台 token、应用可见范围与 API 权限须在目标企业验证。

入站附件返回本地不透明 Ref，下载与转发均通过私有完整路由缓存解析，不能通过修改 Scope 变更收件人。Parts 保留正文、媒体和已有语音识别文字。认证 API 返回 401 时清除应用 token，当前调用直接失败；下一次显式调用才刷新，视频 CDN 错误不清除无关的应用 token。

参考资料（只读对照，不依赖）：
- 企业微信官方文档：https://developer.work.weixin.qq.com/document/path/90236
- Hermes 加解密兼容实现：https://github.com/NousResearch/hermes-agent/blob/main/plugins/platforms/wecom/wecom_crypto.py （核对 blob 5e09fa419f08f1f3ad5cd69ab51945970b92e908）
- Hermes 自建应用适配器：https://github.com/NousResearch/hermes-agent/blob/main/plugins/platforms/wecom/callback_adapter.py
- wechatpy 自建应用消息接口文档：https://docs.wechatpy.org/zh-cn/latest/work/client.html （仅对照字段/限制，不将版本年龄作为兼容证明）

12 项加解密/验签/身份/回执/出站 mock 测试已在实际仓库 Go 1.26.4/Windows 上通过 `go test -race ./channels/wecomapp`，包含 XML 文档完整性、媒体标识和收件人边界回归；未完成真实企业账号验收。
