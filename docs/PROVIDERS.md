# 网页 Provider 配置与兼容说明

## DeepSeek

类型为 `deepseek`，推荐实例名 `deepseek-web`，避免与 Bifrost 自带的官方
DeepSeek Provider 混淆。Bifrost Key 值为网页 refresh token。插件会调用
`/api/v0/users/current` 刷新 access token，缓存键为凭证摘要。

默认模型为 `deepseek`；支持 `_think`、`_search`、`_expert` 的组合。
附件仅提取最后一条消息，支持 data URL 与公共 HTTP(S)，最多 32 个，每账号最多
3 个并发上传。上传使用真实 PoW；解析查询按 access token 合并批次，支持终态错误、
独立超时、429/Retry-After 退避及取消。请求、流、PoW 和轮询都响应取消。

可选配置（均不含凭证）：

```json
{
  "type": "deepseek",
  "options": {
    "base_url": "https://chat.deepseek.com",
    "token_ttl_seconds": 3600,
    "pow_max_iterations": 2000000,
    "file_poll_interval_ms": 3000,
    "file_parse_timeout_ms": 120000,
    "file_poll_max_backoff_ms": 30000,
    "max_concurrent_uploads": 3
  }
}
```

也可配置 `user_agent`、`app_version`、`client_version`、`sec_ch_ua`、
`device_id`、`intercom_device_id`、`intercom_session`，以及
`models: [{"id": "...", "owned_by": "..."}]`。未知字段启动失败。
不自动重试 completion/upload POST；completion 的 401/403 会清掉缓存供后续请求刷新。

官方 `api.deepseek.com` 使用 Bifrost 自带 `deepseek` Provider，本项目不再代理官方 API。

## 元宝

类型为 `yuanbao`，推荐实例名 `yuanbao-web`。Bifrost Provider Key 的值为：

```text
token:<网页 token> agentId:<agent id> hy_user:<用户 id>
```

这三个值均必需，不能把 Bifrost 虚拟 Key 当作网页 token。
默认模型为 `hunyuan_gpt_175B_0404`，保留模型名中的下划线。
支持旧项目的十个模型及 search/deepsearch flags；
`hunyuan_t1` 使用 agentModeModelSetting，`deep_seek_v3` 保留完整名称。

```json
{
  "type": "yuanbao",
  "options": {
    "browserless_ws": "ws://browserless-chromium:3000",
    "web_version": "2.83.11",
    "commit_tag": "0d2b8477",
    "timezone_offset_minutes": 480,
    "signer_timeout_seconds": 90,
    "max_signer_sessions": 4
  }
}
```

Browserless 必须能访问元宝网页并提供 Chrome 的 `/json/new`、`/json/close/{id}`
和 CDP WebSocket 接口。连接 URL 上的认证 query 会保留。Browserless URL 自身属于
部署敏感配置；不要提交真实 token。

签名代码在 Go Provider 内管理 CDP：创建 tab、设置 cookie、打开元宝、定位页面中的
Qimei SDK，调用真实 `getUSKeySync`，取得安全头。没有外部 Deno signer。
会话按 token、agentId、hy_user 三者的摘要隔离，最大数量受限，空闲实例可被淘汰；
同账号签名串行执行，失败重建一次，取消或卸载会关闭 WebSocket 和 tab。
可覆盖 `qimei_app_key`、`qimei_app_id`、`qimei_module_ids`；
网页 SDK 变动可能需要更新模块定位逻辑。

如果在客户端提供已生成的签名，可省略 Browserless，必须同时提供匹配的 uskey、
timestamp、hy92。Bifrost 默认可通过 `x-bf-eh-*` 转发：

```text
x-bf-eh-x-yuanbao-uskey: <签名>
x-bf-eh-x-yuanbao-timestamp: <对应毫秒时间戳>
x-bf-eh-x-yuanbao-hy92: <对应 h38>
```

如果配置了 `client.header_filter_config.allowlist`，加入
`x-yuanbao-uskey`、`x-yuanbao-timestamp`、`x-yuanbao-hy92` 及实际使用的其他头。
插件也接受直传的 `x-yuanbao-*` 和原始安全头别名；是否直接转发由 Bifrost 决定。
支持 device-id、hy93、bus-params-md5、exp-params、trid-channel、web-ch-id 覆盖。
缺省 MD5 根据同一 timestamp/h38 计算。自动签名使用完整 SDK 签名元组，避免与传入的
部分字段混配。没有签名能力时明确报错。

## 兼容边界

- 元宝旧服务的附件列表始终为空，没有上传实现。新 Provider 遇到附件明确返回
  unsupported_operation；不会静默丢弃图片。待有经过验证的元宝上传协议后再实现。
- 元宝旧版 JSON 无效时额外请求一次“整理为 JSON”；新实现使用共享 core 的 schema
  校验并明确失败，不隐式追加模型调用。普通与流式请求保持同一套验证。
- 工具调用来自提示词协议，DeepSeek 为随机 delimiter，元宝为 bracket；
  不宣称网页原生支持 OpenAI tools。参数、tool_choice 和 parallel_tool_calls 由 core 校验。
- 两者均不伪造 usage；上游未提供 token 数时不输出估算用量。
- 元宝 text/think/deepSearch/searchGuid、研究大纲、分隔标题和人物表格映射到共享事件；
  无文本等价物的组件/图谱元数据忽略。搜索 URL 放在响应根级 citations。
- 流收到明确结束标记才成功结束。异常 EOF、错误事件、非 JSON 响应、非 2xx SSE 均失败。
- 每次请求创建临时网页会话，结束、错误和取消时清理。无状态 Responses 走同一条逻辑。
- 默认测试使用合成凭证与本地模拟上游；尚未进行真实账号联网验收，不替换生产服务。
