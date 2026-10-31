# 2026-10-31 部署与真实调用验证

用户明确要求将现有 deepseek-web、yuanbao-web 改为新 Provider，并测试真实调用；
此前也明确要求停止 chat-modelscope。本次部署保留原有 Provider name、Key、别名、
模型限制、权重与虚拟 Key 授权。

## 部署结果

- Bifrost chat 容器改用动态构建镜像
  `bifrost-web-providers:20261031-6fca4a1`，对应实现提交 6fca4a1。
- 单个 `web-providers.so` 在 Bifrost 进程内运行，
  loopback 健康检查包含 deepseek-web、yuanbao-web、modelscope 三个实例。
- 现有 deepseek-web → `http://127.0.0.1:8792/providers/deepseek-web`。
- 现有 yuanbao-web → `http://127.0.0.1:8792/providers/yuanbao-web`。
- 现有 modelscope、qwen → `http://127.0.0.1:8792/providers/modelscope`。
- chat-modelscope 已停止，Compose 中作为 legacy-modelscope profile 保留，
  chat 不再依赖它。旧 DeepSeek/元宝 Deno 容器保留作回滚备用，当前 Provider 已不走它们。
- 元宝仍使用现有外部 Browserless；没有为新 Provider 启动独立 API 服务。

切换前保留了私有 Compose、SQLite 和 Provider 配置快照；快照不在本仓库。
更新后对全部原有 Key 做语义比较，凭证、别名映射、模型限制、权重和启用状态均未变。
Bifrost 启动时重新序列化了部分 aliases_json，仅 JSON 表达形式改变，映射未变。

## 实际调用

经 Bifrost 现有入口、现有虚拟 Key，提示词为“只回复 OK”：

| Provider / 现有别名 | 普通 Chat | SSE | 实际结果 |
| --- | --- | --- | --- |
| deepseek-web / base | HTTP 200，1.25 秒 | HTTP 200，1.04 秒 | OK，正常 finish |
| yuanbao-web / pro | HTTP 200，5.05 秒 | HTTP 200，1.85 秒 | OK，正常 finish |

DeepSeek 测试显式选择池内已验证有效的现有 Key。随机轮换测试暴露出池内旧 Token
被上游拒绝（HTTP 401），包括主账号的一枚和另一枚旧 Token；本次没有擅自禁用、
替换这些 Key。未处理它们前，随机选 Key 仍可能失败。这不是插件路径不通：
相同入口选择有效 Key 时，普通与流式调用均成功。

ModelScope 的新实现通过了完整网关的合成图像生成、multipart 编辑、异步轮询、
url/b64_json、旧 qwen/modelscope 别名和不可用模型不重提等回归。
此次未额外发起付费/计量的真实图像生成，因此不宣称已对新实现完成真实图片验收。

## 真实协议中修复的问题

元宝实际 SSE 混有 speech_type 事件（status/text）与
`[plugin: …]`、`[MSGINDEX:…]`、`[TRACEID:…]` 控制帧。
这些内容现在作为元数据跳过，未知错误仍失败；
只有控制帧而无答案的流不能被判为成功。新增了合成 fixture 与显式 opt-in
live_protocol_test.go 诊断；诊断不打印 JSON 内容或凭证。

离线回归、全仓 race/vet、原生插件加载、动态镜像构建均通过。
完整网关回归还覆盖三个 Provider 的 Chat/Responses/JSON/tools 与 Bifrost
鉴权、选 Key、别名、模型权限和会话清理。

## 回滚顺序

先用 scripts/manage-bifrost.py rollback 恢复旧 Provider 路由/插件配置；
再恢复私有快照中的原 Compose 镜像配置并重建 chat。
恢复旧 ModelScope 路由时，必须同时恢复旧适配服务。
不要在运行中的 SQLite 文件上直接覆盖数据库快照。
