package deepseek

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/WebLLmTech/bifrost-web-providers/core"
)

func newTestProvider(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	raw, _ := json.Marshal(map[string]any{"base_url": server.URL, "file_poll_interval_ms": 1, "file_parse_timeout_ms": 200})
	provider, err := New(context.Background(), raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := provider.(*Provider)
	t.Cleanup(func() { p.Close(context.Background()) })
	return p
}
func biz(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"biz_code": 0, "biz_data": v}})
}
func fixtureChallenge() powChallenge {
	hash := deepseekHashV1([]byte("synthetic_1_0"))
	return powChallenge{Algorithm: "DeepSeekHashV1", Challenge: hex.EncodeToString(hash[:]), Salt: "synthetic", ExpireAt: 1, Difficulty: 1, Signature: "synthetic-signature"}
}
func testRun(t *testing.T, p *Provider, model string) core.RunInput {
	t.Helper()
	req := core.ChatRequest{Model: model, Messages: []core.Message{{Role: "user", Content: core.TextContent("hello")}}}
	cfg, err := p.Configure(req)
	if err != nil {
		t.Fatal(err)
	}
	return core.RunInput{Request: req, Config: cfg, Context: core.RequestContext{Credential: core.Credential{Value: "synthetic-refresh"}}}
}
func TestProviderConversationUploadAndCleanup(t *testing.T) {
	var refreshes, deletes, uploads, polls atomic.Int32
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case usersCurrentPath:
			refreshes.Add(1)
			fmt.Fprint(w, `{"code":0,"data":{"biz_data":{"token":"synthetic-access"}}}`)
		case sessionCreatePath:
			biz(w, map[string]any{"chat_session": map[string]string{"id": "session"}})
		case sessionDeletePath:
			deletes.Add(1)
			fmt.Fprint(w, "{}")
		case powChallengePath:
			biz(w, map[string]any{"challenge": fixtureChallenge()})
		case fileUploadPath:
			uploads.Add(1)
			if r.Header.Get("Authorization") != "Bearer synthetic-access" {
				t.Error("access token not selected")
			}
			if err := r.ParseMultipartForm(1024); err != nil {
				t.Error(err)
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Error(err)
			} else {
				file.Close()
			}
			biz(w, attachment{ID: "file", Status: "PARSING"})
		case fileFetchPath:
			polls.Add(1)
			if r.URL.Query().Get("file_ids") != "file" {
				t.Error("file ids missing")
			}
			biz(w, map[string]any{"files": []attachment{{ID: "file", Status: "SUCCESS"}}})
		case completionPath:
			var body completionMessage
			json.NewDecoder(r.Body).Decode(&body)
			if !body.SearchEnabled || !body.ThinkingEnabled || body.ModelType != "expert" || len(body.RefFileIDs) != 1 {
				t.Errorf("body: %+v", body)
			}
			decoded, err := base64.StdEncoding.DecodeString(r.Header.Get("X-Ds-Pow-Response"))
			if err != nil {
				t.Error(err)
			}
			var answer powAnswer
			json.Unmarshal(decoded, &answer)
			if answer.Answer != 0 || answer.TargetPath != completionPath {
				t.Error("invalid PoW response")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			for _, v := range []string{`{"v":{"response":{"fragments":[{"id":0,"type":"THINK","content":"reason"},{"id":1,"type":"RESPONSE","content":"answer"}]}}}`, `{"p":"response/fragments/0/content","v":" more reason"}`, `{"p":"response/quasi_status","v":"FINISHED"}`} {
				fmt.Fprintf(w, "data: %s\n\n", v)
				w.(http.Flusher).Flush()
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	in := testRun(t, p, "deepseek_expert_think_search")
	in.Request.Messages[0].Content = core.Content(`[{"type":"text","text":"hello"},{"type":"file","file":{"filename":"a.txt","file_data":"data:text/plain;base64,aGVsbG8="}}]`)
	stream, err := p.Open(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var text, reason string
	for {
		e, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == core.EventContent {
			text += e.Text
		}
		if e.Kind == core.EventReasoning {
			reason += e.Text
		}
	}
	if text != "answer" || reason != "reason more reason" {
		t.Fatalf("text=%q reason=%q", text, reason)
	}
	if refreshes.Load() != 1 || deletes.Load() != 1 || uploads.Load() != 1 || polls.Load() != 1 {
		t.Fatalf("counts %d %d %d %d", refreshes.Load(), deletes.Load(), uploads.Load(), polls.Load())
	}
}
func TestProviderFailedOpenCleansAndDoesNotLeakBody(t *testing.T) {
	var deletes atomic.Int32
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case usersCurrentPath:
			fmt.Fprint(w, `{"code":0,"data":"synthetic-access"}`)
		case sessionCreatePath:
			biz(w, map[string]any{"chat_session": map[string]string{"id": "session"}})
		case sessionDeletePath:
			deletes.Add(1)
		case powChallengePath:
			biz(w, map[string]any{"challenge": fixtureChallenge()})
		case completionPath:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(401)
			fmt.Fprint(w, "secret-access")
		}
	})
	_, err := p.Open(context.Background(), testRun(t, p, "deepseek"))
	if core.PublicError(err).Status != 401 || strings.Contains(err.Error(), "secret") || deletes.Load() != 1 {
		t.Fatalf("err=%v deletes=%d", err, deletes.Load())
	}
}
func TestStreamRejectsErrorsAndTruncation(t *testing.T) {
	for _, data := range []string{"data: {}\n\n", "event: error\ndata: {}\n\n", "data: {\"code\":7,\"msg\":\"secret\"}\n\n", "data: malformed\n\n"} {
		t.Run(data, func(t *testing.T) {
			s := &completionStream{sse: core.NewSSEReader(context.Background(), io.NopCloser(strings.NewReader(data)), 4096), dec: newDecoder()}
			_, err := s.Next(context.Background())
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
func TestPowCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := solvePowContext(ctx, strings.Repeat("0", 64), "salt", 1, 2000000, 2000000)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestPollerInterruptsStalledFetch(t *testing.T) {
	for _, cancelCaller := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelCaller), func(t *testing.T) {
			started, stopped := make(chan struct{}), make(chan struct{})
			p := NewFileParsePoller(context.Background(), func(ctx context.Context, _ []string) ([]attachment, error) {
				close(started)
				<-ctx.Done()
				close(stopped)
				return nil, ctx.Err()
			}, time.Millisecond, 50*time.Millisecond, time.Second, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := p.WaitFor(ctx, attachment{ID: "x", Status: "PARSING"}); result <- err }()
			<-started
			if cancelCaller {
				cancel()
			}
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("no error")
				}
			case <-time.After(time.Second):
				t.Fatal("waiter leaked")
			}
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("fetch leaked")
			}
		})
	}
}

func TestProviderCloseInterruptsActiveStreamAndCleans(t *testing.T) {
	var deletes atomic.Int32
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case usersCurrentPath:
			fmt.Fprint(w, `{"code":0,"data":"synthetic-access"}`)
		case sessionCreatePath:
			biz(w, map[string]any{"chat_session": map[string]string{"id": "session"}})
		case sessionDeletePath:
			deletes.Add(1)
			fmt.Fprint(w, "{}")
		case powChallengePath:
			biz(w, map[string]any{"challenge": fixtureChallenge()})
		case completionPath:
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	})
	s, err := p.Open(context.Background(), testRun(t, p, "deepseek"))
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() { _, err := s.Next(context.Background()); ended <- err }()
	p.Close(context.Background())
	select {
	case err := <-ended:
		if err == nil {
			t.Fatal("no cancellation error")
		}
	case <-time.After(time.Second):
		t.Fatal("stream leaked")
	}
	s.Close()
	if deletes.Load() != 1 {
		t.Fatalf("cleanup count %d", deletes.Load())
	}
}

func TestOptionsRejectInvalidConfiguration(t *testing.T) {
	for _, raw := range []string{`{"typo":true}`, `{} {}`, `{"base_url":"https://user:secret@example.com"}`, `{"base_url":"https://example.com?token=secret"}`, `{"pow_max_iterations":-1}`} {
		if _, err := New(context.Background(), json.RawMessage(raw), nil); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
