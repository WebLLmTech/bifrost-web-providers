package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIChatBodyKeepsNativeFields(t *testing.T) {
	var r ChatRequest
	if err := json.Unmarshal([]byte(`{"model":"org/model_with_underscores","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}],"temperature":0.2,"thinking":{"type":"enabled"},"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"tool_choice":"required"}`), &r); err != nil {
		t.Fatal(err)
	}
	raw, err := OpenAIChatBody(r)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	json.Unmarshal(raw, &body)
	if body["temperature"] != 0.2 || body["thinking"] == nil || body["tools"] == nil || body["tool_choice"] != "required" || body["model"] != r.Model {
		t.Fatalf("lost native fields %s", raw)
	}
}
func TestOpenAIJSONNativeToolsAndUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","reasoning_content":"reason","tool_calls":[{"id":"call","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5},"citations":["https://example.com"]}`)
	}))
	defer server.Close()
	client := NewHTTPClient()
	defer client.Close()
	req, _ := http.NewRequest("POST", server.URL, nil)
	s, err := OpenAIChatStream(context.Background(), client, req, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg := ChatConfig{Tools: []Tool{{Type: "function", Function: Function{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}}}
	result, err := Collect(context.Background(), s, cfg, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reasoning != "reason" || len(result.ToolCalls) != 1 || result.ToolCalls[0].Function.Name != "lookup" || result.Usage.TotalTokens != 5 || len(result.Citations) != 1 {
		t.Fatalf("%+v", result)
	}
}
func TestOpenAIStreamTailUsageAndTruncation(t *testing.T) {
	data := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"total_tokens\":7}}\n\n"
	s := &openAIStream{sse: NewSSEReader(context.Background(), io.NopCloser(strings.NewReader(data+"data: [DONE]\n\n")), 4096)}
	result, err := Collect(context.Background(), s, ChatConfig{}, 4096)
	if err != nil || result.Content != "answer" || result.Usage.TotalTokens != 7 {
		t.Fatalf("%+v %v", result, err)
	}
	for _, bad := range []string{data, "event: error\ndata: {}\n\n", "data: invalid\n\n", "data: {\"error\":{\"message\":\"secret\"}}\n\n", "data: [DONE]\n\n"} {
		s := &openAIStream{sse: NewSSEReader(context.Background(), io.NopCloser(strings.NewReader(bad)), 4096)}
		_, err := Collect(context.Background(), s, ChatConfig{}, 4096)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("bad stream accepted: %v", err)
		}
	}
}
