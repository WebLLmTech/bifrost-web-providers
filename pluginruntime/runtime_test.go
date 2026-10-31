package pluginruntime

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/WebLLmTech/bifrost-web-providers/core"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fixture struct {
	events    []core.Event
	openErr   error
	nextErr   error
	block     bool
	panicNext bool
	closes    atomic.Int32
	stopped   chan struct{}
	last      core.RunInput
	mode      string
}

func (f *fixture) Models(context.Context, core.RequestContext) ([]core.Model, error) {
	return []core.Model{{ID: "fixture"}}, nil
}
func (f *fixture) Configure(r core.ChatRequest) (core.ChatConfig, error) {
	return (core.ModelStrategy{DefaultModel: "fixture", ToolMode: f.mode}).Build(r)
}
func (f *fixture) Open(ctx context.Context, input core.RunInput) (core.EventStream, error) {
	f.last = input
	if f.openErr != nil {
		return nil, f.openErr
	}
	index := 0
	return core.StreamFuncs{NextFunc: func(c context.Context) (core.Event, error) {
		if f.panicNext {
			panic("secret must not escape")
		}
		if f.block {
			<-c.Done()
			return core.Event{}, c.Err()
		}
		if index < len(f.events) {
			e := f.events[index]
			index++
			return e, nil
		}
		if f.nextErr != nil {
			return core.Event{}, f.nextErr
		}
		return core.Event{}, io.EOF
	}, CloseFunc: func() error {
		if f.closes.Add(1) == 1 && f.stopped != nil {
			close(f.stopped)
		}
		return nil
	}}, nil
}
func (f *fixture) Close(context.Context) error { return nil }
func testRuntime(t *testing.T, f *fixture) *Runtime {
	t.Helper()
	registry := NewRegistry()
	_ = registry.Register("fixture", func(context.Context, json.RawMessage, *core.Services) (core.Provider, error) { return f, nil })
	cfg := DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.Providers = map[string]ProviderConfig{"fixture": {Type: "fixture"}}
	r, err := New(context.Background(), cfg, registry)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	return r
}
func invoke(r *Runtime, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/providers/fixture/v1/"+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token:private agentId:demo")
	req.Header.Set("x-bf-vk", "must-not-forward")
	req.Header.Set("X-Yuanbao-Uskey", "forwarded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
func TestChatCollectedWireAndCredentials(t *testing.T) {
	f := &fixture{events: []core.Event{{Kind: core.EventReasoning, Text: "think"}, {Kind: core.EventContent, Text: "你好"}, {Kind: core.EventUsage, Usage: &core.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}}, {Kind: core.EventFinish, FinishReason: "stop"}}}
	r := testRuntime(t, f)
	w := invoke(r, "chat/completions", `{"model":"fixture_think","messages":[{"role":"user","content":"hello"}]}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if result["object"] != "chat.completion" || !strings.Contains(w.Body.String(), "你好") || !strings.Contains(w.Body.String(), "think") {
		t.Fatal(w.Body.String())
	}
	if f.last.Context.Credential.Fields["token"] != "private" || f.last.Context.Headers.Get("x-bf-vk") != "" || f.last.Context.Headers.Get("Authorization") != "" || f.last.Context.Headers.Get("X-Yuanbao-Uskey") != "forwarded" {
		t.Fatal("credential/header isolation failed")
	}
	if f.closes.Load() != 1 {
		t.Fatalf("raw stream closed %d times", f.closes.Load())
	}
}
func TestChatSSEReasoningUsageAndDone(t *testing.T) {
	f := &fixture{events: []core.Event{{Kind: core.EventReasoning, Text: "r"}, {Kind: core.EventContent, Text: "a"}, {Kind: core.EventContent, Text: "b"}, {Kind: core.EventUsage, Usage: &core.Usage{TotalTokens: 3}}, {Kind: core.EventFinish, FinishReason: "stop"}}}
	r := testRuntime(t, f)
	w := invoke(r, "chat/completions", `{"model":"fixture","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
	if w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatal(w.Header())
	}
	body := w.Body.String()
	if strings.Count(body, "[DONE]") != 1 || !strings.Contains(body, `"reasoning_content":"r"`) || !strings.Contains(body, `"choices":[],"created"`) || !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatal(body)
	}
	if f.closes.Load() != 1 {
		t.Fatal(f.closes.Load())
	}
}
func TestStartupErrorsAndMalformedRequests(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int
	}{{`{"messages":[]}`, 400}, {`{"messages":[{"role":"bad","content":"x"}]}`, 400}, {`null`, 400}, {`[]`, 400}, {`{} {}`, 400}, {`{"messages":[{"role":"user","content":"x"}],"response_format":{"type":"bad"}}`, 400}} {
		r := testRuntime(t, &fixture{})
		w := invoke(r, "chat/completions", tc.body)
		if w.Code != tc.want {
			t.Fatal(tc.body, w.Code, w.Body.String())
		}
	}
	f := &fixture{openErr: core.Unauthorized("invalid token:private agentId:demo")}
	r := testRuntime(t, f)
	w := invoke(r, "chat/completions", `{"messages":[{"role":"user","content":"x"}]}`)
	if w.Code != 401 || strings.Contains(w.Body.String(), "private") {
		t.Fatal(w.Code, w.Body.String())
	}
	req := httptest.NewRequest("POST", "/providers/fixture/v1/chat/completions", strings.NewReader(`{}`))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
}
func TestStreamFailureDoesNotEmitSuccessfulFinish(t *testing.T) {
	for _, panicNext := range []bool{false, true} {
		f := &fixture{events: []core.Event{{Kind: core.EventContent, Text: "partial"}}, nextErr: errors.New("private upstream failure"), panicNext: panicNext}
		r := testRuntime(t, f)
		w := invoke(r, "chat/completions", `{"stream":true,"messages":[{"role":"user","content":"x"}]}`)
		if strings.Contains(w.Body.String(), `"finish_reason":"stop"`) || strings.Contains(w.Body.String(), "private upstream failure") || !strings.Contains(w.Body.String(), `"error"`) {
			t.Fatal(w.Body.String())
		}
	}
}
func TestDisconnectCancelsProviderAndCleansExactlyOnce(t *testing.T) {
	f := &fixture{block: true, stopped: make(chan struct{})}
	r := testRuntime(t, f)
	server := httptest.NewServer(r)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/providers/fixture/v1/chat/completions", strings.NewReader(`{"stream":true,"messages":[{"role":"user","content":"x"}]}`))
	req.Header.Set("Authorization", "Bearer secret")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	response.Body.Close()
	select {
	case <-f.stopped:
	case <-time.After(time.Second):
		t.Fatal("disconnected request left a live provider stream")
	}
	if f.closes.Load() != 1 {
		t.Fatal(f.closes.Load())
	}
}
func TestJSONStreamBufferedUntilSchemaValidated(t *testing.T) {
	f := &fixture{events: []core.Event{{Kind: core.EventContent, Text: `{"value":"bad"}`}}}
	r := testRuntime(t, f)
	w := invoke(r, "chat/completions", `{"stream":true,"messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_schema","json_schema":{"name":"n","schema":{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"]}}}}`)
	if strings.Contains(w.Body.String(), `"content":"{`) || !strings.Contains(w.Body.String(), "json_schema_mismatch") {
		t.Fatal(w.Body.String())
	}
}
func TestResponsesCollectedAndIncremental(t *testing.T) {
	for _, stream := range []bool{false, true} {
		f := &fixture{events: []core.Event{{Kind: core.EventReasoning, Text: "reason"}, {Kind: core.EventContent, Text: "你好"}, {Kind: core.EventContent, Text: " world"}, {Kind: core.EventUsage, Usage: &core.Usage{TotalTokens: 4}}}}
		r := testRuntime(t, f)
		body := `{"model":"fixture","instructions":"system","input":"hi","stream":false}`
		if stream {
			body = strings.Replace(body, `"stream":false`, `"stream":true`, 1)
		}
		w := invoke(r, "responses", body)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		if f.last.Request.Messages[0].Role != "system" || f.last.Request.Messages[1].Content.Text() != "hi" {
			t.Fatal(f.last.Request)
		}
		if stream {
			for _, kind := range []string{"response.created", "response.output_text.delta", "response.reasoning_summary_text.delta", "response.completed"} {
				if !strings.Contains(w.Body.String(), "event: "+kind) {
					t.Fatal(w.Body.String())
				}
			}
		} else {
			if !strings.Contains(w.Body.String(), `"object":"response"`) || !strings.Contains(w.Body.String(), `"text":"你好 world"`) {
				t.Fatal(w.Body.String())
			}
		}
	}
}
func TestResponsesToolsAndStatelessBoundaries(t *testing.T) {
	f := &fixture{events: []core.Event{{Kind: core.EventToolCall, ToolCall: &core.ToolCallDelta{Index: 0, ID: "call_demo", Type: "function", Function: core.ToolFunction{Name: "weather", Arguments: `{"city":"北京"}`}}}}}
	r := testRuntime(t, f)
	w := invoke(r, "responses", `{"input":[{"type":"function_call","call_id":"old","name":"weather","arguments":"{}"},{"type":"function_call_output","call_id":"old","output":"sunny"},{"role":"user","content":[{"type":"input_text","text":"again"},{"type":"input_image","image_url":"https://cdn/image.png"}]}],"tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"weather"},"stream":true}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "response.function_call_arguments.delta") || !strings.Contains(w.Body.String(), "response.completed") {
		t.Fatal(w.Code, w.Body.String())
	}
	if f.last.Request.Messages[1].ToolCallID != "old" {
		t.Fatal(f.last.Request)
	}
	files, err := core.ExtractFiles(f.last.Request.Messages, true)
	if err != nil || len(files) != 1 {
		t.Fatal(files, err)
	}
	for _, body := range []string{`{"input":"x","store":true}`, `{"input":"x","previous_response_id":"resp_old"}`, `{"input":"x","background":true}`} {
		if w := invoke(r, "responses", body); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
func TestRuntimeConfigurationAndStartStop(t *testing.T) {
	for _, input := range []map[string]any{{"listen_addr": "0.0.0.0:8792"}, {"listen_addr": "localhost:8792"}, {"request_timeout_seconds": 0}, {"unknown": true}} {
		if _, err := ParseConfig(input); err == nil {
			t.Fatal("invalid config accepted", input)
		}
	}
	registry := NewRegistry()
	cfg := DefaultConfig()
	cfg.Providers = map[string]ProviderConfig{"missing": {Type: "not-yet-implemented"}}
	if _, err := New(context.Background(), cfg, registry); err == nil {
		t.Fatal("missing provider silently started")
	}
	r := testRuntime(t, &fixture{})
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(); err == nil {
		t.Fatal("double start allowed")
	}
	response, err := http.Get("http://" + r.Addr() + "/health")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestCitationsRemainAtBifrostCompatibleRoot(t *testing.T) {
	for _, stream := range []bool{false, true} {
		f := &fixture{events: []core.Event{{Kind: core.EventContent, Text: "answer"}, {Kind: core.EventCitations, Citations: []json.RawMessage{json.RawMessage(`"https://example.com"`)}}, {Kind: core.EventCitations, Citations: []json.RawMessage{json.RawMessage(`{"url":"https://example.com"}`), json.RawMessage(`"https://example.org"`)}}}}
		r := testRuntime(t, f)
		body := `{"messages":[{"role":"user","content":"x"}]}`
		if stream {
			body = `{"stream":true,"messages":[{"role":"user","content":"x"}]}`
		}
		w := invoke(r, "chat/completions", body)
		if !strings.Contains(w.Body.String(), `"citations":["https://example.com","https://example.org"]`) {
			t.Fatal(w.Body.String())
		}
	}
}
