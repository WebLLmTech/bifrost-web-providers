# ModelScope：迁移已有 Bifrost 适配

本 Provider 来自当前部署使用的 `bifrost/modelscope-plugin`，不是另一个同名 SDK。
该旧适配曾由 `chat-modelscope` 独立容器提供；现在代码位于
`providers/modelscope`，和 DeepSeek、元宝编译进同一个 `web-providers.so`。

## 是否需要独立服务

不需要为这三个 Provider 各部署一个 API 服务。动态 Bifrost 加载插件后，
`Init` 在 Bifrost 进程内启动唯一的 loopback 入口 `127.0.0.1:8792`，
所有实例共享该入口。端口只绑定回环地址；它不是另一个容器或进程。

元宝自动签名仍需要外部 Browserless 浏览器服务。DeepSeek 和 ModelScope 不需要
额外适配服务。旧静态 Bifrost 无法加载 Go .so，必须换用 deploy/Dockerfile 构建的
动态镜像；单独改 provider base URL 不能让静态二进制具备加载能力。

Bifrost 的 provider name 保持 `deepseek-web`、`yuanbao-web`。
ModelScope 现有 `modelscope`、`qwen` 两个 Bifrost Provider 可以共同指向：
`http://127.0.0.1:8792/providers/modelscope`。各自的 Key、别名、模型限制、
虚拟 Key 和日志仍由 Bifrost 管理。

## 配置

```json
{
  "listen_addr": "127.0.0.1:8792",
  "request_timeout_seconds": 600,
  "max_request_mb": 100,
  "providers": {
    "modelscope": {
      "type": "modelscope",
      "options": {
        "base_url": "https://api-inference.modelscope.ai",
        "poll_interval_ms": 2000,
        "timeout_seconds": 540,
        "max_result_mb": 64
      }
    }
  }
}
```

兼容旧名称 `upstream_url`，与 `base_url` 二选一。base URL 可带尾部 /v1。
单一入口由顶层 listen_addr 配置，Provider options 不再接受 listen_addr。
请求大小也在顶层配置。未知配置启动失败。

Bifrost custom provider 使用 `base_provider_type: openai`，
`allow_private_network: true`、`max_retries: 0`，
`default_request_timeout_in_seconds: 600`。
启用 image_generation、image_edit 和 chat/models/responses，关闭图像流能力。
完整示例见 examples/bifrost.json。

## 图像兼容

- 编辑请求接受 multipart 文件、JSON URL/data URL（image、images、image_url）。
  multipart 文件转换为图片 data URL，实际调用 /v1/images/generations。
- 提交携带 X-ModelScope-Async-Mode: true；轮询 /v1/tasks/{id} 时携带
  X-ModelScope-Task-Type: image_generation。
- 提交响应中的 SUCCEED 不等于任务完成，拿到 task_id 后仍查询任务端点。
- 输出转为标准 created/data[].url 或 data[].b64_json。下载图片不携带 Provider
  token/cookie，使用共享附件解析器的地址、重定向和大小检查；默认禁止私网地址，
  多张下载结果也共享 max_result_mb 总预算。
- 请求取消、卸载和超时中止本地等待。已提交远端任务不自动取消或重提；
  失败消息保留 task_id，插件响应头提供 X-ModelScope-Task-ID。
- 只对轮询的临时 HTTP 429/502/503/504 重试；提交 POST 不自动重试。
  非法任务 JSON、FAILED、异常空结果不作为临时 HTTP 错误重试。
- num_inference_steps→steps，guidance_scale→guidance；其他模型参数保留。
  暂支持 stream=false、n=1，mask 编辑明确拒绝。不编造 usage。
- 上游结构化错误只取有界 message 字段并移除 token，保留“Model not exists”等诊断。
  修正 API 路径不会让无权限或不存在的模型变得可用，也不会自动改 image-base 别名。

默认上游保持 .ai，与旧部署一致。.cn 不自动作为 fallback；地区账号和 token
可能不互通。协议参考 [ModelScope API 推理](https://www.modelscope.cn/docs/model-service/API-Inference/intro)
和[官方托管示例](https://modelscope.ai/models/openmoss/MOSS-Image-SFT)。

## Chat、Models 与 Responses

保留原生 OpenAI Chat 的工具、thinking、temperature 等参数，模型名不剥离下划线
或特性后缀。Chat JSON/SSE 转为共享 core.Event，保留 reasoning、原生 tool calls、
citations 和实际 usage。Models 从上游获取。Responses 使用共享无状态转换，
与另两个 Provider 相同。旧适配曾代理任意 /v1/*；新入口只开放明确实现的端点，
不提供通用反向代理。

## 部署与回滚工具

scripts/manage-bifrost.py 是显式执行的运维工具，不在测试或插件启动时自动运行。
它使用只读挂载的现有 config.db 中有效管理员 session 调用管理 API，
不新增/导出 Provider Key，不改虚拟 Key 授权。backup 会生成权限 600 的私有
SQLite 快照与 Provider 配置快照；快照不可提交到仓库。

执行顺序：inspect → backup → 构建并切换动态镜像/挂载匹配的 .so →
configure → verify → test。configure 更新已有 deepseek-web、yuanbao-web、modelscope、
qwen 的目标地址，并核对 Key/别名/权重不变。回滚使用 rollback 恢复 Provider/
插件设置，再恢复原镜像；若要恢复旧 ModelScope 路径，需要重新启动旧适配容器。

测试命令：
```sh
scripts/check.sh
scripts/test-plugin.sh
docker build -f deploy/Dockerfile -t bifrost-web-providers:modelscope .
BIFROST_IMAGE=bifrost-web-providers:modelscope scripts/test-gateway.sh
```

普通测试不使用生产凭证。真实调用仅由明确授权的 manage-bifrost.py test 触发，
复用现有虚拟 Key，输出有界结果和状态，不打印凭证。


后续经用户明确授权，可使用 manage-bifrost.py disable-invalid，读取同一私有 state
目录中的 DeepSeek 审计结果，仅禁用已确认的 40003 invalid token，并将原别名保留到
有效 Key。此操作另存私有回滚记录，不修改凭证值和权重；不属于默认 configure 行为。
