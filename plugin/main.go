// Build with -buildmode=plugin using the same Go/CGO/libc flags as Bifrost.
package main

import (
	"context"
	"errors"
	"github.com/WebLLmTech/bifrost-web-providers/pluginruntime"
	"github.com/WebLLmTech/bifrost-web-providers/providers"
	"sync"
	"time"
)

var state struct {
	sync.Mutex
	runtime *pluginruntime.Runtime
}

func GetName() string { return "web-providers" }
func Init(input any) error {
	state.Lock()
	defer state.Unlock()
	if state.runtime != nil {
		return errors.New("web-providers is already running")
	}
	cfg, err := pluginruntime.ParseConfig(input)
	if err != nil {
		return err
	}
	registry := pluginruntime.NewRegistry()
	if err = providers.Register(registry); err != nil {
		return err
	}
	runtime, err := pluginruntime.New(context.Background(), cfg, registry)
	if err != nil {
		return err
	}
	if err = runtime.Start(); err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = runtime.Close(ctx)
		return err
	}
	state.runtime = runtime
	return nil
}
func Cleanup() error {
	state.Lock()
	defer state.Unlock()
	if state.runtime == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := state.runtime.Close(ctx)
	state.runtime = nil
	return err
}
func main() {}
