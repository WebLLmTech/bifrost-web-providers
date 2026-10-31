// Package providers is the only registration point for concrete web providers.
package providers

import "github.com/WebLLmTech/bifrost-web-providers/pluginruntime"

// Register is intentionally empty in the core-only phase. Unknown configured
// provider types fail at startup; there are no placeholder success adapters.
// QwenDev adds deepseek.New and yuanbao.New factories here after migration.
func Register(registry *pluginruntime.Registry) error { return nil }
