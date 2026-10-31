// Package core is the provider-independent SDK for web-backed Bifrost providers.
package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
)

type Content json.RawMessage

func (c Content) MarshalJSON() ([]byte, error) {
	if len(c) == 0 {
		return []byte("null"), nil
	}
	return json.RawMessage(c).MarshalJSON()
}
func (c *Content) UnmarshalJSON(b []byte) error {
	var raw json.RawMessage
	if err := raw.UnmarshalJSON(b); err != nil {
		return err
	}
	*c = Content(raw)
	return nil
}
func TextContent(text string) Content { b, _ := json.Marshal(text); return b }
func (c Content) Text() string {
	var text string
	if json.Unmarshal(c, &text) == nil {
		return text
	}
	var parts []ContentPart
	if json.Unmarshal(c, &parts) != nil {
		return ""
	}
	for _, p := range parts {
		if p.Type == "text" || p.Type == "input_text" || p.Type == "output_text" {
			text += p.Text
		}
	}
	return text
}

type ContentPart struct {
	Type     string     `json:"type"`
	Text     string     `json:"text,omitempty"`
	ImageURL *URLValue  `json:"image_url,omitempty"`
	FileURL  *URLValue  `json:"file_url,omitempty"`
	File     *FileValue `json:"file,omitempty"`
}
type URLValue struct {
	URL string `json:"url"`
}
type FileValue struct {
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data,omitempty"`
	FileID   string `json:"file_id,omitempty"`
}
type Message struct {
	Role             string     `json:"role"`
	Content          Content    `json:"content,omitempty"`
	Name             string     `json:"name,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
}
type Function struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}
type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
}
type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}
type ToolCallDelta struct {
	Index    int          `json:"index"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function ToolFunction `json:"function"`
}
type ResponseFormat struct {
	Type       string          `json:"type"`
	JSONSchema json.RawMessage `json:"json_schema,omitempty"`
}
type ChatRequest struct {
	Model             string          `json:"model"`
	Messages          []Message       `json:"messages"`
	Stream            bool            `json:"stream,omitempty"`
	Tools             []Tool          `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	ResponseFormat    *ResponseFormat `json:"response_format,omitempty"`
	StreamOptions     *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
	Extra map[string]json.RawMessage `json:"-"`
}

func (r *ChatRequest) UnmarshalJSON(b []byte) error {
	type plain ChatRequest
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*r = ChatRequest(p)
	if err := json.Unmarshal(b, &r.Extra); err != nil {
		return err
	}
	for _, k := range []string{"model", "messages", "stream", "tools", "tool_choice", "parallel_tool_calls", "response_format", "stream_options"} {
		delete(r.Extra, k)
	}
	return nil
}

type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object,omitempty"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by,omitempty"`
}
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
type Credential struct {
	Value  string
	Fields map[string]string
}
type RequestContext struct {
	Credential Credential
	Headers    http.Header
	RequestID  string
}
type RunInput struct {
	Request ChatRequest
	Config  ChatConfig
	Context RequestContext
}

// Providers implement only their upstream protocol. One stream is consumed for
// both streaming and non-streaming calls. Close MUST interrupt blocked Next.
type Provider interface {
	Models(context.Context, RequestContext) ([]Model, error)
	Configure(ChatRequest) (ChatConfig, error)
	Open(context.Context, RunInput) (EventStream, error)
	Close(context.Context) error
}
type EventKind string

const (
	EventContent   EventKind = "content"
	EventReasoning EventKind = "reasoning"
	EventToolCall  EventKind = "tool_call"
	EventCitations EventKind = "citations"
	EventUsage     EventKind = "usage"
	EventFinish    EventKind = "finish"
)

type Event struct {
	Kind         EventKind
	Text         string
	ToolCall     *ToolCallDelta
	Citations    []json.RawMessage
	Usage        *Usage
	FinishReason string
}
type EventStream interface {
	Next(context.Context) (Event, error)
	Close() error
}

// StreamFuncs makes callback adapters small; use ManagedStream to attach cleanup.
type StreamFuncs struct {
	NextFunc  func(context.Context) (Event, error)
	CloseFunc func() error
}

func (s StreamFuncs) Next(ctx context.Context) (Event, error) {
	if s.NextFunc == nil {
		return Event{}, io.EOF
	}
	return s.NextFunc(ctx)
}
func (s StreamFuncs) Close() error {
	if s.CloseFunc != nil {
		return s.CloseFunc()
	}
	return nil
}
