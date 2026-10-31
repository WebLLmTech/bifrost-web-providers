# 接管审核记录

QwenDev 在用户要求停止后已取消。接管时仅留下 DeepSeek 初稿；保存在本地 WIP
提交 4526091，未直接合入 main。后续由 Codex 审核、修正并完成元宝迁移。

## 已修复

- DeepSeek PoW 不响应取消：增加 context 检查与取消测试，并用原始 WASM 独立验证
  22 组向量。保留真实算法，删除带开发机绝对路径的探索脚本。
- 解析轮询在 HTTP 卡住时越过文件超时：查询受最近文件 deadline 约束；
  最后一个等待者取消时中断查询。轮询器通过引用计数获取，消除空闲回收与注册竞态；
  索引使用凭证摘要，结束后删除。
- 非 2xx SSE 被当作正常流、错误事件被忽略：检查状态、JSON 错误与异常 EOF，
  completion 的 401/403 清除 token 缓存，不重试有副作用的 POST。
- 上游错误可能带回原始响应和 token：对外只返回固定的状态/错误说明。
- DeepSeek 显式 THINK fragment patch 被错分成 answer：根据 patch 指向的 fragment 类型分流。
- 引用测试错误地要求重复显示搜索结果：按 URL 和结果片段分别去重，修正预期。
- 关闭 Provider 时活动流和清理存在边界：Provider 生命周期取消连接，
  DeepSeek 清理使用已选出的 access token，不依赖已关闭的刷新缓存。
- 两个 Provider 的工具历史曾重复且元宝 bracket 拼法有缺口：统一为 core.PromptText，
  覆盖历史调用、工具结果关联、两种协议及非法参数。
- options 的尾随 JSON 检查不正确：严格校验尾随数据和未知字段；
  base_url 不接受内嵌凭证、query 或 fragment。

## 新增元宝实现

Go 中实现请求体、模型、会话、事件、覆盖头与 Browserless/CDP 签名。
按完整账号身份隔离并限制签名会话，签名失败只重建一次，
取消/卸载关闭 socket 和 browser tab；不依赖旧 Deno signer。

详见 [PROVIDERS.md](PROVIDERS.md) 的兼容差异，尤其是附件明确不支持、
JSON 校验不自动追加整理请求，以及当前未做真实账号联网验收。

## 验证入口

- scripts/check.sh：全仓 race tests、coverage 与 vet。
- scripts/test-plugin.sh：真实动态 Go plugin.Open 生命周期和 HTTP 契约。
- testdata/verify.mjs：独立调用原始 DeepSeek WASM。
- scripts/test-gateway.sh：无外网的完整动态 Bifrost + 合成上游，
  验证两个服务的 Chat、逐块 SSE、Responses、JSON、工具，以及鉴权/选 Key/
  模型权限/别名/签名头转发/会话清理。

验证脚本是可复现入口；真实账号、真实 Browserless 页面与线上反爬环境未经过验证。
此次不修改生产容器或 Deno 服务。


## 本次验证结果（2026-10-31）

- 全仓 race tests、vet 和原生 plugin.Open 检查通过。
- DeepSeek 原始 WASM 的 22 组向量独立校验通过。
- deploy/Dockerfile 的完整动态 Bifrost 镜像构建成功（Go 1.27.0 / musl / CGO）。
- 无外网容器中的真实 Bifrost 集成通过：两服务的普通/流式 Chat、普通/流式
  Responses、JSON schema、跨块工具标记，及鉴权、选 Key、模型权限、别名和签名头。
  17 个合成测试会话全部删除。
- 测试启动时使用本地空 pricing/model-parameters datasheet，关闭 MCP catalog
  背景同步；没有依赖外网下载价格表。SSE 断言重组实际 delta 后校验内容。
- 真实网页账号、线上 Qimei SDK 与 Browserless 页面未联网验证；元宝附件仍明确拒绝。
