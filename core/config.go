package core

import (
	"encoding/json"
	"strings"
)

type Features struct {
	Thinking      bool
	Searching     bool
	DeepSearching bool
}
type ChatConfig struct {
	Model             string
	ModelName         string
	Features          Features
	Stream            bool
	ResponseFormat    ResponseFormat
	Tools             []Tool
	ToolChoice        json.RawMessage
	ParallelToolCalls bool
	ToolCallingDone   bool
	ToolProtocol      *ToolProtocol
	Extra             map[string]json.RawMessage
}
type ModelStrategy struct {
	DefaultModel string
	Separator    string
	ModelName    func(string, []string) string
	ToolMode     string
}

func (s ModelStrategy) Build(r ChatRequest) (ChatConfig, error) {
	model := r.Model
	if model == "" {
		model = s.DefaultModel
	}
	if model == "" {
		return ChatConfig{}, Invalid("model is required")
	}
	sep := s.Separator
	if sep == "" {
		sep = "_"
	}
	parts := strings.Split(model, sep)
	features := Features{}
	for _, p := range parts {
		switch p {
		case "think":
			features.Thinking = true
		case "search":
			features.Searching = true
		case "deepsearch":
			features.Searching = true
			features.DeepSearching = true
		}
	}
	name := parts[0]
	if s.ModelName != nil {
		name = s.ModelName(model, parts)
	}
	c := ChatConfig{Model: model, ModelName: name, Features: features, Stream: r.Stream, ResponseFormat: ResponseFormat{Type: "text"}, Tools: r.Tools, ToolChoice: r.ToolChoice, ParallelToolCalls: true, Extra: r.Extra}
	if r.ParallelToolCalls != nil {
		c.ParallelToolCalls = *r.ParallelToolCalls
	}
	if r.ResponseFormat != nil {
		c.ResponseFormat = *r.ResponseFormat
	}
	switch c.ResponseFormat.Type {
	case "text", "json_object", "json_schema":
	default:
		return ChatConfig{}, Unsupported("Unsupported response_format")
	}
	for _, m := range r.Messages {
		if m.Role == "tool" {
			c.ToolCallingDone = true
		}
	}
	if len(c.Tools) > 0 && string(c.ToolChoice) != "\"none\"" && s.ToolMode != "" && s.ToolMode != "native" {
		p, err := NewToolProtocol(s.ToolMode)
		if err != nil {
			return ChatConfig{}, err
		}
		c.ToolProtocol = p
	}
	return c, nil
}
func StripFeatureFlags(model string, parts []string) string {
	var out []string
	for _, p := range parts {
		if p != "think" && p != "search" && p != "deepsearch" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "_")
}
