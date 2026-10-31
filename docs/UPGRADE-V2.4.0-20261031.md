# 2026-10-31 Bifrost v2.4.0 升级与 Docker 清理

按用户要求升级至核对时最新的官方稳定版
[transports/v2.4.0](https://github.com/maximhq/bifrost/releases/tag/transports/v2.4.0)，
该版本发布于 2026-10-29。上线镜像为 `bifrost-web-providers:v2.4.0-20261031`，
线上 `/api/version` 返回 `v2.4.0`，容器健康检查正常。

## 为什么从官方源码构建

当前实现通过 Go `plugin.Open` 加载 `web-providers.so`。
[官方镜像构建文件](https://github.com/maximhq/bifrost/blob/transports/v2.4.0/transports/Dockerfile)
使用静态链接；此前加载该插件实际报过 `Dynamic loading not supported`。
仅以官方镜像为基础复制 `.so`，不能改变既有 Bifrost 二进制的链接方式。

本项目固定官方提交 `e869c3f9adc96ea53c38694a21bb04a8e1085487`，未修改其业务源码。
构建时取消静态 host 链接，使用同一 Go 1.27.3、musl、CGO、trimpath、sqlite_static
和 Go workspace 编译 Bifrost 与插件，统一共享依赖与包版本。
这也意味着后续升级应成对构建 host 和插件，不能独立替换其中任意一方。

## 备份与迁移检查

上线前保留私有 Compose、Provider 配置、完整 SQLite 配置和日志快照，以及旧版插件、
旧镜像压缩归档；归档经过完整性检查。备份位于部署目录下
`bifrost/backups/upgrade-v2.4.0-20261031/`，不入库，含凭证的文件限制访问。

先在无外网的临时容器中用数据库副本启动新版本，检查迁移后配置及日志。
日志记录数升级前后均为 7,995，最近 20 条 ID 保持一致。
全部已有 Key 的凭证、模型限制、别名、权重、启用状态均通过语义比较；
deepseek-web、yuanbao-web、modelscope、qwen 的目标仍为原进程内插件入口。
正式升级后再次验证配置，保持 DeepSeek 6 枚启用、9 枚禁用的状态。
预演容器和预演副本已删除，正式备份保留。

数据库升级可能包含 schema 迁移，回滚时应停止网关后恢复成套的旧数据库、插件和镜像，
不可直接用旧程序打开已迁移的数据库，也不可覆盖运行中的 SQLite 文件。
旧镜像已按清理要求从 Docker 删除，可从私有归档重新载入。

## 验证结果

- Go 1.27.3 下的 `go test -race ./...`、`go vet ./...` 通过。
- `scripts/test-plugin.sh` 的实际 `.so` 加载、请求、卸载通过。
- 完整 UI、动态 Bifrost 与插件构建通过。
- `scripts/test-gateway.sh` 使用实际新版本 Bifrost、合成上游和无外网容器通过，
  并校验 `/api/version`。覆盖三个 Provider 的 Chat、SSE、Responses、JSON、tools，
  以及鉴权、选 Key、模型权限、别名、签名头、会话清理和 ModelScope 异步图像协议。
- 示例和网关 fixture 的 Provider Key 使用正确的 `enabled` 字段；虚拟 Key 的
  `is_active` 保持不变。修正后完整网关回归再次通过。

上线后经已有虚拟 Key 调用、不指定 Provider Key，提示词为“只回复 OK”：

| Provider / 别名 | 普通 Chat | SSE | 结果 |
| --- | --- | --- | --- |
| deepseek-web / base | HTTP 200，1.53 秒 | HTTP 200，1.43 秒 | OK，正常 finish |
| yuanbao-web / pro | HTTP 200，5.03 秒 | HTTP 200，1.90 秒 | OK，正常 finish |

ModelScope 完成离线图像回归，本次未额外发起真实计量图像生成。

## Docker 清理

按用户扩大后的范围，删除全部 6 个停止的容器：chat-miromind、chat-longcat、
chat-yuanbao、chat-deepseek、chat-modelscope、rebook-postgres。
旧服务的 Compose 块保持注释，Browserless 和其他运行服务继续运行。

删除无容器引用的镜像和未使用的构建缓存，未删除任何数据卷。
Docker 报告镜像两次回收分别为 13.04 GB、10.32 MB，构建缓存回收 17.08 GB。
这些是 Docker 的回收统计，不等于宿主机虚拟磁盘文件立即缩小的空间。

最终共 24 个运行中容器、24 个被引用镜像，没有停止容器或无引用镜像。
15 个数据卷全部保留；剩余构建缓存为 788.7 MB，均被使用。
回滚镜像归档和数据库备份保留在 Docker 之外。
