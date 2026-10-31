// Package providers is the registration point for concrete web providers.
package providers

import (
	"github.com/WebLLmTech/bifrost-web-providers/pluginruntime"
	"github.com/WebLLmTech/bifrost-web-providers/providers/deepseek"
	"github.com/WebLLmTech/bifrost-web-providers/providers/modelscope"
	"github.com/WebLLmTech/bifrost-web-providers/providers/yuanbao"
)

func Register(registry *pluginruntime.Registry) error {
	if err := registry.Register("deepseek", deepseek.New); err != nil {
		return err
	}
	if err := registry.Register("yuanbao", yuanbao.New); err != nil {
		return err
	}
	return registry.Register("modelscope", modelscope.New)
}
