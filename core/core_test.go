package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testStream struct {
	events []Event
	i      int
	closes atomic.Int32
	err    error
}

func (s *testStream) Next(ctx context.Context) (Event, error) {
	if err := ctx.Err(); err != nil {
		return Event{}, err
	}
	if s.i < len(s.events) {
		e := s.events[s.i]
		s.i++
		return e, nil
	}
	if s.err != nil {
		return Event{}, s.err
	}
	return Event{}, io.EOF
}
func (s *testStream) Close() error { s.closes.Add(1); return nil }
func TestContentAndRequestWire(t *testing.T) {
	var r ChatRequest
	if err := json.Unmarshal([]byte(`{"model":"m","temperature":0.2,"messages":[{"role":"user","content":[{"type":"text","text":"你好"},{"type":"image_url","image_url":{"url":"https://cdn/image.png"}}]}]}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.Messages[0].Content.Text() != "你好" || string(r.Extra["temperature"]) != "0.2" {
		t.Fatal(r)
	}
	b, _ := json.Marshal(r.Messages[0])
	if !strings.Contains(string(b), `"type":"text"`) {
		t.Fatalf("Content serialized as base64: %s", b)
	}
	files, err := ExtractFiles(r.Messages, false)
	if err != nil || len(files) != 1 || files[0].Source != "https://cdn/image.png" {
		t.Fatal(files, err)
	}
}
func TestAuthAndModelFlags(t *testing.T) {
	c, err := Bearer("Bearer token:a:b agentId:42 hy_user:123")
	if err != nil || c.Fields["token"] != "a:b" {
		t.Fatal(c, err)
	}
	if _, err := Bearer("token:a token:b"); err == nil {
		t.Fatal("duplicate fields accepted")
	}
	strategy := ModelStrategy{DefaultModel: "hunyuan_gpt_175B_0404", ModelName: StripFeatureFlags, ToolMode: "bracket"}
	cfg, err := strategy.Build(ChatRequest{Model: "hunyuan_gpt_175B_0404_think_deepsearch"})
	if err != nil || cfg.ModelName != "hunyuan_gpt_175B_0404" || !cfg.Features.Thinking || !cfg.Features.Searching || !cfg.Features.DeepSearching {
		t.Fatal(cfg, err)
	}
}
func TestSSEFragmentedUnicodeAndMultiline(t *testing.T) {
	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := NewSSEReader(ctx, reader, 4096)
	defer s.Close()
	go func() {
		defer writer.Close()
		for _, b := range []byte(": comment\r\nevent: patch\r\nid: abc\r\nretry: 10\r\ndata: 你好\r\ndata: 世界\r\n\r\ndata: [DONE]\n\n") {
			_, _ = writer.Write([]byte{b})
		}
	}()
	e, err := s.Next(ctx)
	if err != nil || e.Data != "你好\n世界" || e.Event != "patch" || e.ID != "abc" || e.Retry != 10 {
		t.Fatal(e, err)
	}
	e, err = s.Next(ctx)
	if err != nil || e.Data != "[DONE]" {
		t.Fatal(e, err)
	}
	if _, err = s.Next(ctx); err != io.EOF {
		t.Fatal(err)
	}
}
func TestSSELimitsCancellationAndIncompleteEOF(t *testing.T) {
	for name, source := range map[string]string{"too-big": "data: " + strings.Repeat("x", 100) + "\n\n", "multiline-limit": strings.Repeat("data: abc\n", 10) + "\n"} {
		t.Run(name, func(t *testing.T) {
			s := NewSSEReader(context.Background(), io.NopCloser(strings.NewReader(source)), 32)
			defer s.Close()
			if _, err := s.Next(context.Background()); err == nil {
				t.Fatal("event limit not enforced")
			}
		})
	}
	s := NewSSEReader(context.Background(), io.NopCloser(strings.NewReader("data: incomplete")), 4096)
	if _, err := s.Next(context.Background()); err != io.EOF {
		t.Fatal(err)
	}
	s.Close()
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		r, w := io.Pipe()
		cancel()
		s := NewSSEReader(ctx, r, 4096)
		done := make(chan error, 1)
		go func() { _, err := s.Next(ctx); done <- err }()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("canceled SSE read leaked")
		}
		s.Close()
		w.Close()
	}
}
func TestConversationCleanupEveryExit(t *testing.T) {
	for _, mode := range []string{"success", "error", "panic", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			var cleaned atomic.Int32
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			source := &testStream{}
			var stream EventStream
			var err error
			func() {
				defer func() {
					if recover() != nil && mode != "panic" {
						t.Error("unexpected panic")
					}
				}()
				stream, err = Conversation(ctx, func(context.Context) (int, error) { return 7, nil }, func(context.Context, int) (EventStream, error) {
					switch mode {
					case "error":
						return nil, errors.New("primary")
					case "panic":
						panic("test")
					}
					return source, nil
				}, func(c context.Context, id int) error {
					if c.Err() != nil || id != 7 {
						t.Error("cleanup did not get a live context and conversation")
					}
					cleaned.Add(1)
					return errors.New("cleanup must not mask primary")
				})
			}()
			if mode == "error" && (err == nil || err.Error() != "primary") {
				t.Fatal(err)
			}
			if stream != nil {
				if mode == "cancel" {
					cancel()
				}
				var wg sync.WaitGroup
				for i := 0; i < 10; i++ {
					wg.Add(1)
					go func() { defer wg.Done(); stream.Close() }()
				}
				wg.Wait()
			}
			if cleaned.Load() != 1 {
				t.Fatalf("cleanup calls %d", cleaned.Load())
			}
		})
	}
}
func TestCacheSingleFlightAndCallerCancellation(t *testing.T) {
	cache := NewRefreshCache[string](16, time.Second)
	defer cache.Close()
	started := make(chan struct{})
	release := make(chan struct{})
	var loads atomic.Int32
	load := func(ctx context.Context) (string, time.Time, error) {
		if loads.Add(1) == 1 {
			close(started)
		}
		select {
		case <-ctx.Done():
			return "", time.Time{}, ctx.Err()
		case <-release:
			return "access", time.Now().Add(time.Hour), nil
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	canceled := make(chan error, 1)
	go func() { _, err := cache.Get(ctx, "private-key", load); canceled <- err }()
	<-started
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := cache.Get(context.Background(), "private-key", load)
			if err != nil || v != "access" {
				t.Errorf("%s %v", v, err)
			}
		}()
	}
	cancel()
	if err := <-canceled; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	wg.Wait()
	if loads.Load() != 1 {
		t.Fatalf("refreshes %d", loads.Load())
	}
	cache.Invalidate("private-key")
	_, err := cache.Get(context.Background(), "private-key", load)
	if err != nil || loads.Load() != 2 {
		t.Fatal(loads.Load(), err)
	}
}
func TestCacheCloseCancelsRefreshAndFailureCanRetry(t *testing.T) {
	cache := NewRefreshCache[string](2, time.Second)
	ready := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := cache.Get(context.Background(), "token", func(ctx context.Context) (string, time.Time, error) {
			close(ready)
			<-ctx.Done()
			return "", time.Time{}, ctx.Err()
		})
		result <- err
	}()
	<-ready
	cache.Close()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("closed refresh succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("refresh not canceled")
	}
	other := NewRefreshCache[string](2, time.Second)
	defer other.Close()
	for i := 0; i < 2; i++ {
		_, err := other.Get(context.Background(), "key", func(context.Context) (string, time.Time, error) { return "", time.Time{}, errors.New("bad") })
		if err == nil {
			t.Fatal("failed refresh cached")
		}
	}
}
func TestInvalidationDuringRefreshDoesNotRecacheStaleValue(t *testing.T) {
	cache := NewRefreshCache[string](2, time.Second)
	defer cache.Close()
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var loads atomic.Int32
	load := func(context.Context) (string, time.Time, error) {
		if loads.Add(1) == 1 {
			close(started)
			<-release
		}
		return "value", time.Now().Add(time.Hour), nil
	}
	go func() { defer close(done); _, _ = cache.Get(context.Background(), "credential", load) }()
	<-started
	cache.Invalidate("credential")
	close(release)
	<-done
	_, err := cache.Get(context.Background(), "credential", load)
	if err != nil || loads.Load() != 2 {
		t.Fatal("invalidated refresh populated cache", loads.Load(), err)
	}
}
func TestAccountGateSerialAndCanceledWaiter(t *testing.T) {
	gate := NewAccountGate(1)
	release, err := gate.Acquire(context.Background(), "secret")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := gate.Acquire(ctx, "secret"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	other, err := gate.Acquire(context.Background(), "different-secret")
	if err != nil {
		t.Fatal(err)
	}
	other()
	release()
	release()
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if len(gate.entries) != 0 {
		t.Fatal("account gate entries retained credentials")
	}
}
func TestFileResolverBoundaries(t *testing.T) {
	resolver := NewFileResolver(4, false)
	defer resolver.Close()
	f, err := resolver.Resolve(context.Background(), FileRef{Source: "data:text/plain;base64,YWJj", Filename: "../../input.txt"})
	if err != nil || string(f.Bytes) != "abc" || f.Filename != "input.txt" {
		t.Fatal(f, err)
	}
	for _, source := range []string{"data:text/plain;base64,YWJjZGVmZw==", "data:text/plain,abc", "file:///etc/passwd", "http://127.0.0.1:9/private"} {
		if _, err := resolver.Resolve(context.Background(), FileRef{Source: source}); err == nil {
			t.Fatal("invalid attachment accepted", source)
		}
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("credential on file download")
		}
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "abcdef")
	}))
	defer s.Close()
	allowed := NewFileResolver(4, true)
	defer allowed.Close()
	if _, err := allowed.Resolve(context.Background(), FileRef{Source: s.URL}); err == nil {
		t.Fatal("oversized download accepted")
	}
}
func TestHTTPClientRejectsHTMLAndHTTPErrorSSE(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(401)
		io.WriteString(w, "data: secret\n\n")
	}))
	defer s.Close()
	client := NewHTTPClient()
	defer client.Close()
	req, _ := http.NewRequest("POST", s.URL, nil)
	if _, err := client.SSE(context.Background(), req); err == nil || PublicError(err).Status != 401 {
		t.Fatal(err)
	}
}
func TestToolMarkersSplitAtEveryByte(t *testing.T) {
	for _, mode := range []string{"bracket", "delimited"} {
		t.Run(mode, func(t *testing.T) {
			p, _ := NewToolProtocol(mode)
			block := "[ToolCalls][Call:weather][CallParameter:city]北京[/CallParameter][/Call][/ToolCalls]"
			if mode == "delimited" {
				m := p.Markers
				block = m.Start + m.NameStart + "weather" + m.NameEnd + m.ArgsStart + `{"city":"北京"}` + m.ArgsEnd + m.End
			}
			for split := 0; split <= len(block); split++ {
				buf := NewToolBuffer(p, 4096)
				var events []Event
				for _, part := range []string{"before " + block[:split], block[split:] + " after"} {
					out, err := buf.Push(part)
					if err != nil {
						t.Fatal(split, err)
					}
					events = append(events, out...)
				}
				tail, err := buf.Flush()
				if err != nil {
					t.Fatal(err)
				}
				events = append(events, tail...)
				var text string
				calls := 0
				for _, e := range events {
					if e.Kind == EventContent {
						text += e.Text
					}
					if e.Kind == EventToolCall {
						calls++
						if e.ToolCall.Function.Name != "weather" || !strings.Contains(e.ToolCall.Function.Arguments, "北京") {
							t.Fatal(e)
						}
					}
				}
				if calls != 1 || text != "before  after" {
					t.Fatal(split, calls, text)
				}
			}
			buf := NewToolBuffer(p, 4096)
			_, _ = buf.Push(p.Markers.Start + "incomplete")
			if _, err := buf.Flush(); err == nil {
				t.Fatal("truncated tool block accepted")
			}
		})
	}
}
func TestToolHistoryUsesMostRecentMatchingCall(t *testing.T) {
	m := []Message{{Role: "system", Content: TextContent("system")}, {Role: "assistant", ToolCalls: []ToolCall{{ID: "old", Function: ToolFunction{Name: "first"}}}}, {Role: "assistant", ToolCalls: []ToolCall{{ID: "new", Function: ToolFunction{Name: "second"}}}}, {Role: "tool", ToolCallID: "new", Content: TextContent("result")}}
	p, _ := NewToolProtocol("bracket")
	cfg := ChatConfig{ToolProtocol: p, Tools: []Tool{{Type: "function", Function: Function{Name: "second"}}}}
	merged := MergeMessages(m, cfg)
	if !strings.Contains(merged[0].Content.Text(), "second") || !strings.Contains(merged[1].Content.Text(), "[Result:second]") {
		t.Fatal(merged)
	}
}
func TestStructuredOutputAndToolSchemas(t *testing.T) {
	f := ResponseFormat{Type: "json_schema", JSONSchema: json.RawMessage(`{"name":"answer","schema":{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"],"additionalProperties":false}}`)}
	if _, err := NormalizeJSON("```json\n{\"value\":2}\n```", f); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`{"value":"bad"}`, `{"wrong":1}`, `[]`, `not json`} {
		if _, err := NormalizeJSON(value, f); err == nil {
			t.Fatal("invalid structured output", value)
		}
	}
	if _, err := compileSchema(json.RawMessage(`{"$ref":"http://127.0.0.1/private"}`)); err == nil {
		t.Fatal("external schema reference accepted")
	}
	cfg := ChatConfig{ParallelToolCalls: false, Tools: []Tool{{Type: "function", Function: Function{Name: "f", Parameters: json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"]}`)}}}}
	bad := ToolCall{ID: "call", Type: "function", Function: ToolFunction{Name: "f", Arguments: `{"count":"bad"}`}}
	if err := ValidateToolCalls([]ToolCall{bad}, cfg); err == nil {
		t.Fatal("schema not enforced")
	}
	bad.Function.Arguments = `{"count":2}`
	if err := ValidateToolCalls([]ToolCall{bad, bad}, cfg); err == nil {
		t.Fatal("parallel calls dropped silently")
	}
}
func TestCollectReasoningUsageAndToolDeltas(t *testing.T) {
	s := &testStream{events: []Event{{Kind: EventReasoning, Text: "think"}, {Kind: EventToolCall, ToolCall: &ToolCallDelta{Index: 0, ID: "call", Function: ToolFunction{Name: "f", Arguments: `{"x":`}}}, {Kind: EventToolCall, ToolCall: &ToolCallDelta{Index: 0, Function: ToolFunction{Arguments: `1}`}}}, {Kind: EventUsage, Usage: &Usage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7}}, {Kind: EventFinish, FinishReason: "stop"}}}
	cfg := ChatConfig{ParallelToolCalls: true, Tools: []Tool{{Type: "function", Function: Function{Name: "f"}}}}
	r, err := Collect(context.Background(), s, cfg, 4096)
	if err != nil || r.Reasoning != "think" || r.FinishReason != "tool_calls" || r.Usage.TotalTokens != 7 || r.ToolCalls[0].Function.Arguments != `{"x":1}` {
		t.Fatal(r, err)
	}
	if s.closes.Load() != 1 {
		t.Fatal("stream not closed")
	}
}
