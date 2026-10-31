// A deterministic test-only provider. Never registered in the production plugin.
package main

import (
	"context"
	"encoding/json"
	"github.com/WebLLmTech/bifrost-web-providers/core"
	"github.com/WebLLmTech/bifrost-web-providers/pluginruntime"
	"io"
	"time"
)

type provider struct{}

func (provider) Models(context.Context, core.RequestContext) ([]core.Model, error) {
	return []core.Model{{ID: "fixture"}}, nil
}
func (provider) Configure(r core.ChatRequest) (core.ChatConfig, error) {
	return (core.ModelStrategy{DefaultModel: "fixture"}).Build(r)
}
func (provider) Open(ctx context.Context, in core.RunInput) (core.EventStream, error) {
	events := []core.Event{{Kind: core.EventReasoning, Text: "test reasoning"}, {Kind: core.EventContent, Text: "hello plugin"}, {Kind: core.EventUsage, Usage: &core.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}}, {Kind: core.EventFinish, FinishReason: "stop"}}
	index := 0
	return core.StreamFuncs{NextFunc: func(context.Context) (core.Event, error) {
		if index == len(events) {
			return core.Event{}, io.EOF
		}
		e := events[index]
		index++
		return e, nil
	}}, nil
}
func (provider) Close(context.Context) error { return nil }

var runtime *pluginruntime.Runtime

func GetName() string { return "fixture-web-providers" }
func Init(input any) error {
	cfg, err := pluginruntime.ParseConfig(input)
	if err != nil {
		return err
	}
	registry := pluginruntime.NewRegistry()
	_ = registry.Register("fixture", func(context.Context, json.RawMessage, *core.Services) (core.Provider, error) { return provider{}, nil })
	runtime, err = pluginruntime.New(context.Background(), cfg, registry)
	if err != nil {
		return err
	}
	return runtime.Start()
}
func Cleanup() error {
	if runtime == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := runtime.Close(ctx)
	runtime = nil
	return err
}
func main() {}
