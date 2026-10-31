# QwenDev 迁移任务

用户要求：本项目 core 由 Codex 实现，完成后交给 QwenDev Agent 迁移 DeepSeek、元宝。
本文件是固定 core 接口和验收边界。阅读 AGENTS.md、DESIGN.md、PROVIDER-SDK.md 后开发。

## 工作范围

1. 实现 `providers/deepseek/` 和 `providers/yuanbao/`。
2. 在 `providers/registry.go` 注册 `deepseek`、`yuanbao` 两个工厂。
3. 增补 options 示例、单元/模拟上游测试、必要的依赖与构建资源。
4. 本阶段不修改正在运行的 Bifrost、Deno 容器、数据库、生产 Key 或系统设置。
5. 旧项目只有参考用途，不得改动；core 如需修复可做小范围兼容修改并写明原因，
   不得绕过 core 新建 Hono/Deno 服务或为两个 Provider 分别启动 HTTP server。

## 原项目位置

- `/Volumes/DevData/Projects/chat/chat-base`，仓库 `WebLLmTech/chat-base`。
- `/Volumes/DevData/Projects/chat/deepseek-deno-api`，仓库 `WebLLmTech/deepseek-deno-api`。
- `/Volumes/DevData/Projects/chat/yuanbao-deno-api`，仓库 `WebLLmTech/yuanbao-deno-api`。

源码中已有固定演示 cookie 等内容不要拷贝成测试凭证，使用合成值。不要从生产
Docker volume 或浏览器读取秘密。真实上游测试只能明确 opt-in，默认测试离线运行。

## DeepSeek 必须迁移

- `main.ts`：模型 flags、expert/default、会话创建/清理、附件与请求编排。
- `api.ts`：刷新 token、挑战、创建/删除 session、completion、上传。
  使用 core.RefreshCache 和 AccountGate，保留按账号上传并发限制。
- `file-parse-poller.ts` 及测试：批量等待附件解析、终态错误、超时、429/backoff、取消。
  这是 DeepSeek 专用逻辑，不应塞进通用 core。
- `lib/challenge.ts` 与 `lib/sha3_wasm_bg.7b9ca65ddd.wasm`：真实 PoW 能力，不可返回固定
  答案或跳过挑战。可用 Go WASM runtime 或等价经测试的实现，记录资源来源。
- `chunk-transformer.ts`：patch/fragment、thinking/text、搜索引用、结束/错误语义。
  输出 core.Event，不要输出 OpenAI SSE 字符串。覆盖分块 patch 与异常 EOF。
- 官方 `api.deepseek.com` 转发能力属于 Bifrost 自带 DeepSeek Provider，不在这次网页
  Provider 重做；文档说明如何同时配置即可。

## 元宝必须迁移

- `main.ts`：token/agentId/hy_user、模型名完整性、模型 flags、历史消息、工具、附件。
- `api.ts`：完整请求体、会话创建/删除、对话流、模型列表和必要的服务参数。
- `signer.ts`：Browserless/CDP 会话、uskey 与安全头获取、过期重建、超时与关闭。
  浏览器保持外部 Browserless 服务，Provider 本身在 Bifrost 进程内；不能留下 Deno
  signer 进程当成完成迁移。不得伪造签名。缓存必须账号隔离且可关闭。
- 保留显式 `x-yuanbao-*`/签名头覆盖方式；同时文档说明 Bifrost 的头转发配置。
- `chunk-transformer.ts`：真实分段/思考/搜索/答案/结束/错误事件，保留引用。

## 测试和完成条件

- 普通 Chat、真实逐块 SSE、reasoning、tools、JSON、无状态 Responses 经同一 core。
- token/浏览器签名失败给出适当错误，不允许 placeholder success。
- 客户端取消、服务出错、文件轮询超时都不能泄漏会话、后台任务或 WebSocket。
- 两服务必须有 synthetic/recorded upstream fixtures，覆盖主要协议路径与失败路径。
- `scripts/check.sh` 和 `scripts/test-plugin.sh` 全部通过。
- 构建 `deploy/Dockerfile` 的完整动态 Bifrost 镜像，补真实网关的 mocked-provider
  集成回归；实际账号的线上验证若缺凭证，明确记录未验证，不得声称已验证。
- core 使用 Go 1.27.0，jsonschema v6.0.2 与 Bifrost 匹配；完整镜像在 Go workspace
  中统一依赖。新增 Provider 依赖也必须通过这个 ABI 构建。
- 交付提交、测试命令/结果、兼容差异和仍未验证的项目，并回报父 Agent。
