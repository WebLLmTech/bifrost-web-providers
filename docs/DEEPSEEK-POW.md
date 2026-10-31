# DeepSeek PoW 实现与验证

Go 实现在 `providers/deepseek/pow.go`。来源为
[deepseek-deno-api/lib/challenge.ts](https://github.com/WebLLmTech/deepseek-deno-api/blob/main/lib/challenge.ts)
及同目录 `sha3_wasm_bg.7b9ca65ddd.wasm`。不分发该二进制、不缓存答案、不跳过挑战。

DeepSeekHashV1 使用 rate 136、domain 0x06、32 字节摘要的 SHA3 sponge；
Keccak-f1600 只执行标准轮次 1..23，跳过第 0 轮。因此不能用标准 SHA3-256 替代。
求解扫描 `salt + "_" + expire_at + "_" + decimal_nonce`，nonce 从 0 到 difficulty-1。
CPU 工作受 pow_max_iterations 限制，每 256 次检查 context。

`testdata/pow_vectors.json` 的 17 个 hash 向量覆盖空串、UTF-8 和 sponge 块边界；
5 个 solve 向量验证真实 WASM 的 nonce 和 difficulty 范围。正常 Go 测试不需要 WASM。

额外的独立验证直接调用原始 WASM 导出函数，校验同一向量文件，不修改文件：

```sh
node providers/deepseek/testdata/verify.mjs /path/to/sha3_wasm_bg.7b9ca65ddd.wasm
```

也可使用 Deno 运行，授权只读文件访问。接管审核时已经对原始二进制执行交叉验证：
22 个向量全部通过。此验证只证明算法等价，不能替代线上账号/反爬环境验证。

本次校验资源来自 deepseek-deno-api 提交
`1ae763feaa1244f5c580123c2714bcdd9a2a5094`，WASM 的 SHA-256 为
`b3fca8cc072c1defbd60c02266a8e48bd307a1804aaff4314900aea720e72f7d`。
