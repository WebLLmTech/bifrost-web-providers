# Implementation boundaries

- `core/` and `pluginruntime/` are the shared SDK and plugin infrastructure.
- Provider implementations belong in `providers/<name>/`; register factories in
  `providers/registry.go`. Never introduce a separate HTTP server per provider.
- Use the shared stream, lifecycle, HTTP, cache, attachment and tool helpers.
- Existing sibling Deno projects are migration references. Do not modify them,
  import credentials from production, or replace running containers in this phase.
- No placeholder success responses. Unsupported operations fail explicitly.
- Keep tokens/cookies out of logs, fixtures, committed configuration and errors.
- Run `go test -race ./...`, `go vet ./...`, and `scripts/test-plugin.sh`.
- DeepSeek and Yuanbao migrations require deterministic recorded/synthetic event
  fixtures. Real network checks are opt-in and must never print credentials.
