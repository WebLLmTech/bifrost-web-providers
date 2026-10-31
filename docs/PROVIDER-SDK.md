# Provider SDK v0.1

## 工厂与注册

```go
func New(ctx context.Context, options json.RawMessage, services *core.Services) (core.Provider, error)
```

在 `providers/registry.go` 调用 `registry.Register("deepseek", deepseek.New)`
和 `registry.Register("yuanbao", yuanbao.New)`。实例名由配置决定，可与工厂类型不同，
例如 `deepseek-web: {type: deepseek}`。

工厂解析自己的 options、创建自己的 token/signer 缓存。失败时必须返回错误。
配置默认值不得包含线上密钥；上游 base URL、Browserless URL 等应可注入用于测试。

## 必须实现

```go
type Provider interface {
    Models(context.Context, core.RequestContext) ([]core.Model, error)
    Configure(core.ChatRequest) (core.ChatConfig, error)
    Open(context.Context, core.RunInput) (core.EventStream, error)
    Close(context.Context) error
}
type EventStream interface {
    Next(context.Context) (core.Event, error)
    Close() error
}
```

`Configure` 通常调用 `core.ModelStrategy.Build`。元宝可使用
`ModelName: core.StripFeatureFlags` 保留原始模型名中的下划线。
DeepSeek 使用 `ToolMode: "delimited"`，元宝使用 `"bracket"`；模型的 `expert`
等服务专用参数放在 Provider 里解释。`Request.Extra` 保留额外请求字段，包含会话 id
等旧参数，不要把这些字段当成可信的鉴权信息。

`RunInput.Context.Credential.Value` 是 Bifrost 选中的 Key 原值；`.Fields`
支持 `token:... agentId:... hy_user:...`。不要再次读取客户端虚拟 Key。
`Context.Headers` 已去除 Authorization、Cookie、Proxy-Authorization、`x-bf-*`，
其他显式转发的服务头仍可读取。Bifrost 是否转发这些头需要在完整集成时验证。

## 会话与取消

```go
return core.Conversation(ctx,
    func(ctx context.Context) (Session, error) { return createSession(ctx) },
    func(ctx context.Context, s Session) (core.EventStream, error) {
        return openUpstream(ctx, s)
    },
    func(ctx context.Context, s Session) error { return deleteSession(ctx, s) },
)
```

创建失败不清理不存在的会话；打开流失败、读取失败、客户端取消和成功结束都会清理，
且只执行一次。清理使用独立的 5 秒 context，清理错误不覆盖主要响应。
不要在 goroutine 中继续使用已取消请求的凭证/会话资源。

`EventStream.Close` 必须中断被阻塞的 `Next`。网络读取使用
`services.HTTP.SSE(ctx, req)`，它会在取消时关闭响应体。
上游已完成时返回 `EventFinish`，随后可返回 usage/citations 并以 `io.EOF` 结束；
上游异常 EOF 由 Provider 判定为错误。core 不会凭空知道服务是否完成。

## 事件

- `EventContent` / `EventReasoning`：`Text` 为增量，不是累计全文。
- `EventToolCall`：`ToolCallDelta.Index` 是稳定的 0-based call 序号，首个 delta
  应提供稳定 ID、函数名，后续 `Arguments` 为增量。提示词协议通常只需发文本，
  core 会解析工具块，不要 Provider 再解析一次。
- `EventCitations`：原始 JSON 引用列表；不要把检索状态文本混入答案。
- `EventUsage`：最终完整 usage 快照，不是 token 增量。
- `EventFinish`：`FinishReason` 为 `stop`、`length` 等标准原因。
- 错误通过 `Next` 的 error 返回，不作为普通 content。

core 的流通道只有一个缓冲槽，提供背压。事件在返回后不应由 Provider 并发修改。
JSON 格式的文本会缓冲到验证通过，普通文本不需要全量缓冲。

## 共用工具

- `core.MergeMessages` / `HistoryText`：合并历史、系统提示、工具调用结果。
- `core.ExtractFiles` / `services.Files.Resolve`：提取最后一条消息或全部历史的附件，
  获取 bytes、MIME、文件名。上传是服务特定逻辑，不能复用服务外部的文件 ID。
- `core.NewRefreshCache[T]`：按 credential digest 合并刷新；单个等待者取消不会影响
  其他等待者。Provider `Close` 必须关闭缓存。`Invalidate` 可处理上游 token 失效。
- `core.NewAccountGate(n)`：按账号限制并发上传，释放函数可安全重复调用。
- `services.HTTP.JSON` / `.SSE`：检查 HTTP status、Content-Type、限制 body。
  不自动重试有副作用的 POST。401/403 不可以不断轮换重试。
- `core.Unauthorized` / `Invalid` / `Unsupported` / `Upstream`：可传给客户端的错误。
  普通 error 对外仅显示通用错误；不要主动把上游完整 HTML、cookie/token 写进错误。

## Responses

Provider 不需要再实现 Responses。runtime 将无状态 input、instructions、function
call/output 和多模态内容转换成相同 ChatRequest，再把同一事件流转换为 Responses
事件。`text.format` 与 Chat 的 response_format 共用校验。

## 验收命令

```sh
scripts/check.sh
scripts/test-plugin.sh
```

完整网关检查使用 deploy/Dockerfile 构建后运行 scripts/test-gateway.sh。
共享历史序列化使用 core.PromptText，保留 assistant 工具调用和工具结果。
