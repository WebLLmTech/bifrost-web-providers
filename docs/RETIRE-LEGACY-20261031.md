# 2026-10-31 失效 Token 与旧容器清理

用户明确要求禁用失效 Token，停止全部旧 chat-* 容器并在 Compose 中注释；
随后授权临时重启这些容器测量内存。

## Token 检查与禁用

对 deepseek-web 当时启用的 13 枚 Token，仅调用 /api/v0/users/current 验证授权，
未为检查 Token 发起图像或对话生成。结果为 6 枚有效、7 枚明确返回
40003 / Authorization Failed (invalid token)，无网络/限流等不确定项。
7 枚失效 Token 已通过 Bifrost 管理 API 禁用，未删除凭证或改变权重。

原先的 8 个 base/pro 等别名存在于其中一枚失效 Key 上，因此同步到了 6 枚有效 Key，
避免禁用旧 Key 后别名失效或 pro 被当成普通模型。已比较全部原有 Key，凭证值、
模型限制、名称和权重未变；只有已授权的 enabled 状态与别名同步发生变化。
已有禁用项保持禁用。

禁用后经 Bifrost、不指定 Key 随机选取的真实复测：

| Provider | 普通 Chat | SSE | 结果 |
| --- | --- | --- | --- |
| deepseek-web / base | 200，1.57 秒 | 200，1.13 秒 | OK、正常 finish |
| yuanbao-web / pro | 200，3.11 秒 | 200，1.87 秒 | OK、正常 finish |

检查入口为 opt-in 的 providers/deepseek/live_tokens_test.go，输入只读的私有临时
凭证文件；输出不含 Token 或返回的 access token。scripts/manage-bifrost.py 的
`disable-invalid` 只禁用明确的 40003 invalid token，不把网络失败或限流当作失效。
审计结果与操作前记录保存在私有备份目录，不入库。

## 停止与内存测量

下列旧容器均已停止，对应 Compose 服务块已注释。chat 网关和 Browserless 保持运行。
按用户授权临时启动、稳定后采样两次，并在 finally 中再次停止全部五个容器。

| 容器 | 最近一次空闲内存 |
| --- | ---: |
| chat-longcat | 32.50 MiB |
| chat-yuanbao | 26.63 MiB |
| chat-deepseek | 20.96 MiB |
| chat-miromind | 18.90 MiB |
| chat-modelscope | 6.94 MiB |
| 合计 | 105.93 MiB |

第一次合计 98.48 MiB，第二次 105.93 MiB。这是重启后的空闲占用，不含 Browserless，
也不能还原之前的请求缓存、历史峰值或计算新旧整体内存净差。
LongCat、Miromind 尚未迁到插件，对应旧路由在停止后暂时不可用。
Compose 校验通过，当前启用服务仅 chat。
