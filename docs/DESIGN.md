# Bifrost web providers

## Goal and scope

Move the shared `chat-base` behavior and the web-provider implementations into
one Go project. This phase implements the shared runtime; the next phase moves
DeepSeek and Yuanbao without changing the existing production services.

## Request path

Client → Bifrost authentication/governance → provider/key selection and aliases
→ OpenAI-compatible provider dispatch → loopback listener owned by the loaded
plugin → provider implementation → normalized events → Bifrost response/logging.

The plugin exports `Init(any) error`, `GetName() string`, and `Cleanup() error`.
It owns one loopback HTTP server and a registry of provider instances. A provider
base URL is `http://127.0.0.1:8792/providers/deepseek-web` (or `yuanbao-web`).
The selected Bifrost key reaches the provider as Authorization; client virtual
keys never substitute for upstream credentials. We intentionally run after
Bifrost's normal key selection rather than executing remote calls in a
pre-hook, which would bypass its provider dispatch/accounting path.

## Packages

- `core`: OpenAI wire types; stream-first Provider contract; model feature
  strategy; message/history and attachments; prompt tool protocols and JSON
  normalization; SSE reader/writer; typed errors; bounded HTTP access; token
  cache/single-flight; per-account semaphore; conversation finalizers.
- `pluginruntime`: config validation, factory registry, lifecycle, routing,
  bounded request execution and OpenAI HTTP serialization.
- `providers`: provider factories. DeepSeek owns token refresh, challenge/PoW,
  uploads and file-parse polling, sessions, event/patch decoding. Yuanbao owns
  cookie/agent identity, browserless/CDP signer, uploads, sessions and events.
- `plugin`: the native shared-object entry point, with no service-specific
  logic and no Bifrost schema dependency.

## Provider contract

`Models(ctx, auth)` returns model metadata. `Open(ctx, RunInput)` creates the
upstream request and returns an `EventStream`; both streaming and non-streaming
clients consume that stream. `EventStream.Next(ctx)` must honor cancellation;
`Close()` must interrupt outstanding reads and release resources. The shared
conversation helper attaches exactly-once cleanup to `Close` and to failures
during startup, with a separate bounded cleanup context.

Core owns SSE and completion assembly. Providers emit content, reasoning,
native tool-call deltas, citations, usage, finish or errors, never preformatted
OpenAI SSE. EOF is accepted as ordinary completion only when the adapter knows
the upstream completed; truncated SSE should be returned as an error by the
provider. No fabricated token counts are used.

Provider methods receive a request-scoped credential, forwarded header copy,
parsed configuration, normalized messages and an HTTP service bundle. Token
and browser-session caches are per provider instance and keyed by a credential
digest. No credentials go into plugin configuration examples.

## Compatibility

Support chat completion and models, streaming/non-streaming, reasoning,
citations, tool histories and tool choices, JSON response normalization,
file/image data URLs and bounded remote downloads. Responses conversion is a
separate transport adapter over the same provider contract; unsupported stored
response retrieval/history must be explicit rather than pretend persistence.
`parallel_tool_calls=false` cannot silently drop extra tool calls.

Legacy model flags (`_think`, `_search`, `_deepsearch`) remain available through
a configurable strategy. Native provider-specific model selection stays in the
provider. Bifrost custom provider names `deepseek-web` and `yuanbao-web` avoid
collision with its built-in official DeepSeek provider.

## Runtime constraint

The installed Bifrost 2.2.6 image is musl/static and reports `Dynamic loading
not supported` for `.so`. Production native loading requires a dynamically
linked build. Pin Go 1.27.0 and upstream commit
`8b4fce4f1709d66f9208d02f50552da522535f9e`; build the host and plugin together
with the same Go/CGO/libc/trimpath settings. The project includes a build recipe
and a dynamic native-plugin smoke host. Development may run the same runtime
as a standalone process; production architecture is the in-process plugin.

## Rollout and acceptance

1. Implement core, race-tested deterministic fixtures and native plugin loading.
2. Freeze the Provider contract and document migration tasks for QwenDev.
3. QwenDev migrates both services and tests against synthetic upstream servers.
4. Review implementations, build the dynamic Bifrost image, run full gateway
   integration, then separately plan production switching and rollback.

The shared runtime is complete only when cancellation, backpressure, repeated
cleanup, malformed SSE/JSON, HTTP errors, tool marker splits and concurrent
token refresh have tests. No real provider is advertised as migrated in phase 1.
