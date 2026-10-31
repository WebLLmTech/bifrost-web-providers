package yuanbao

import (
	"context"
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
	"github.com/gorilla/websocket"
)

func newTestProvider(t *testing.T, o Options) *Provider {
	t.Helper()
	raw, _ := json.Marshal(o)
	v, err := New(context.Background(), raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := v.(*Provider)
	t.Cleanup(func() { p.Close(context.Background()) })
	return p
}
func testInput(t *testing.T, p *Provider) core.RunInput {
	t.Helper()
	r := core.ChatRequest{Model: "deep_seek_v3_deepsearch", Messages: []core.Message{{Role: "user", Content: core.TextContent("hello")}}}
	cfg, err := p.Configure(r)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := core.Bearer("token:synthetic-token agentId:synthetic-agent hy_user:synthetic-user")
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	h.Set("x-yuanbao-uskey", "synthetic-signature")
	h.Set("x-yuanbao-hy92", strings.Repeat("h", 38))
	h.Set("x-yuanbao-timestamp", "100")
	return core.RunInput{Request: r, Config: cfg, Context: core.RequestContext{Credential: cred, Headers: h}}
}
func TestProviderConversationBodyAndEvents(t *testing.T) {
	var creates, deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Cookie"), "hy_token=synthetic-token") {
			t.Error("missing selected key")
		}
		switch r.URL.Path {
		case "/api/user/agent/conversation/create":
			creates.Add(1)
			if r.Header.Get("X-Uskey") != "synthetic-signature" {
				t.Error("signature not forwarded")
			}
			fmt.Fprint(w, `{"code":0,"data":{"id":"chat"}}`)
		case "/api/user/agent/conversation/v1/clear":
			deletes.Add(1)
			fmt.Fprint(w, "{}")
		case "/api/chat/chat":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["chatModelId"] != "deep_seek_v3" || body["searchDeepMode"] != true || body["offsetOfHour"] != float64(8) {
				t.Errorf("body %+v", body)
			}
			if !strings.Contains(body["prompt"].(string), "hello") {
				t.Error("lost history")
			}
			var ext map[string]any
			json.Unmarshal([]byte(body["chatModelExtInfo"].(string)), &ext)
			if ext["modelId"] != "deep_seek_v3" {
				t.Error("model underscores lost")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			for _, data := range []string{`{"type":"meta"}`, `{"type":"think","content":"reason"}`, `{"type":"deepSearch","contents":[{"msg":"research"}]}`, `{"type":"searchGuid","docs":[{"url":"https://example.com"}]}`, `{"type":"text","msg":"answer[1,2](@ref)[citation:3]"}`, "[DONE]"} {
				fmt.Fprintf(w, "data: %s\n\n", data)
				w.(http.Flusher).Flush()
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	p := newTestProvider(t, Options{BaseURL: server.URL})
	s, err := p.Open(context.Background(), testInput(t, p))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var text, reason string
	citations := 0
	for {
		e, err := s.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch e.Kind {
		case core.EventContent:
			text += e.Text
		case core.EventReasoning:
			reason += e.Text
		case core.EventCitations:
			citations += len(e.Citations)
		}
	}
	if text != "answer[1][2]" || reason != "reasonresearch" || citations != 1 || creates.Load() != 1 || deletes.Load() != 1 {
		t.Fatalf("text=%q reason=%q citations=%d sessions %d/%d", text, reason, citations, creates.Load(), deletes.Load())
	}
}
func TestNoUnsignedOrDroppedAttachments(t *testing.T) {
	p := newTestProvider(t, Options{})
	in := testInput(t, p)
	in.Context.Headers = nil
	if _, err := p.Open(context.Background(), in); core.PublicError(err).Code != "unsupported_operation" {
		t.Fatalf("unsigned: %v", err)
	}
	in = testInput(t, p)
	in.Request.Messages[0].Content = core.Content(`[{"type":"image_url","image_url":{"url":"https://example.com/image.png"}}]`)
	if _, err := p.Open(context.Background(), in); core.PublicError(err).Code != "unsupported_operation" {
		t.Fatalf("attachments: %v", err)
	}
}
func TestStreamErrorsAndTruncation(t *testing.T) {
	for _, data := range []string{"data: {}\n\n", "data: invalid\n\n", "event: error\ndata: secret\n\n", "data: {\"type\":\"error\",\"msg\":\"secret\"}\n\n"} {
		s := &completionStream{sse: core.NewSSEReader(context.Background(), io.NopCloser(strings.NewReader(data)), 4096)}
		if _, err := s.Next(context.Background()); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("err %v", err)
		}
	}
}

// Fake CDP exercises target/cookie lifecycle and the SDK result transport;
// it does not claim to validate Tencent's current SDK against a live account.
type fakeBrowser struct {
	server                          *httptest.Server
	created, closed, signs, cookies atomic.Int32
	failFirst                       bool
	stall                           bool
}

func startBrowser(t *testing.T, failFirst, stall bool) *fakeBrowser {
	t.Helper()
	f := &fakeBrowser{failFirst: failFirst, stall: stall}
	upgrader := websocket.Upgrader{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/json/new":
			id := f.created.Add(1)
			if r.Method != "PUT" || r.URL.Query().Get("token") != "synthetic-browser-token" {
				t.Error("lost browser auth")
			}
			json.NewEncoder(w).Encode(map[string]string{"id": fmt.Sprint(id), "webSocketDebuggerUrl": "ws://unreachable/devtools/page/" + fmt.Sprint(id)})
		case strings.HasPrefix(r.URL.Path, "/json/close/"):
			f.closed.Add(1)
			fmt.Fprint(w, "ok")
		case strings.HasPrefix(r.URL.Path, "/devtools/page/"):
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			for {
				var cmd struct {
					ID     int                        `json:"id"`
					Method string                     `json:"method"`
					Params map[string]json.RawMessage `json:"params"`
				}
				if conn.ReadJSON(&cmd) != nil {
					return
				}
				result := any(map[string]any{})
				switch cmd.Method {
				case "Network.setCookies":
					f.cookies.Add(1)
					var cookies []map[string]string
					json.Unmarshal(cmd.Params["cookies"], &cookies)
					if len(cookies) != 3 || cookies[2]["value"] != "synthetic-token" {
						t.Error("cookies not applied")
					}
				case "Runtime.evaluate":
					var expression string
					json.Unmarshal(cmd.Params["expression"], &expression)
					if strings.Contains(expression, "const uskey =") {
						number := f.signs.Add(1)
						value := any(map[string]any{"h38": strings.Repeat("h", 38), "ts": 100, "uskey": "key +!", "deviceId": "device"})
						if failFirst && number == 1 {
							value = map[string]string{"error": "expired"}
						}
						result = map[string]any{"result": map[string]any{"value": value}}
					} else {
						if stall { // Wait for the canceled client to close its WebSocket.
							for {
								if _, _, err := conn.ReadMessage(); err != nil {
									return
								}
							}
						}
						result = map[string]any{"result": map[string]any{"value": true}}
					}
				}
				if conn.WriteJSON(map[string]any{"id": cmd.ID, "result": result}) != nil {
					return
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}
func (f *fakeBrowser) wsURL() string {
	return strings.Replace(f.server.URL, "http", "ws", 1) + "?token=synthetic-browser-token"
}
func TestSignerRealCDPTransportRetryAndCleanup(t *testing.T) {
	f := startBrowser(t, true, false)
	p := newTestProvider(t, Options{BrowserlessWS: f.wsURL()})
	id := identity{Token: "synthetic-token", AgentID: "agent", User: "user"}
	signer, release, err := p.acquireSigner(id)
	if err != nil {
		t.Fatal(err)
	}
	h, err := signer.sign(context.Background())
	release()
	if err != nil {
		t.Fatal(err)
	}
	if h.Get("X-Uskey") != "key%20%2B!" || h.Get("X-HY92") != strings.Repeat("h", 38) {
		t.Fatalf("signature: %+v", h)
	}
	p.Close(context.Background())
	if f.created.Load() != 2 || f.closed.Load() != 2 || f.cookies.Load() != 2 {
		t.Fatalf("sessions %d/%d cookies %d", f.created.Load(), f.closed.Load(), f.cookies.Load())
	}
}
func TestSignerCancellationClosesTarget(t *testing.T) {
	f := startBrowser(t, false, true)
	p := newTestProvider(t, Options{BrowserlessWS: f.wsURL()})
	signer, release, err := p.acquireSigner(identity{Token: "synthetic-token", AgentID: "agent", User: "user"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = signer.sign(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	if f.created.Load() != 1 || f.closed.Load() != 1 {
		t.Fatalf("leaked target %d/%d", f.created.Load(), f.closed.Load())
	}
}
func TestSignerIdentityIsolationAndCapacity(t *testing.T) {
	p := newTestProvider(t, Options{MaxSignerSessions: 1})
	id := identity{Token: "synthetic-token", AgentID: "agent", User: "user"}
	first, release, err := p.acquireSigner(id)
	if err != nil {
		t.Fatal(err)
	}
	second, release2, err := p.acquireSigner(id)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("same identity not reused")
	}
	other := id
	other.AgentID = "other"
	if _, _, err = p.acquireSigner(other); core.PublicError(err).Status != 429 {
		t.Fatalf("err %v", err)
	}
	release()
	release2()
	next, releaseNext, err := p.acquireSigner(other)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseNext()
	if next == first {
		t.Fatal("cross-account signer reuse")
	}
}
func TestBodyHunyuanAgentMode(t *testing.T) {
	p := newTestProvider(t, Options{})
	cfg, err := p.Configure(core.ChatRequest{Model: "hunyuan_t1_search"})
	if err != nil {
		t.Fatal(err)
	}
	b := p.completionBody(identity{}, "session", "prompt", cfg)
	var ext map[string]any
	json.Unmarshal([]byte(b["chatModelExtInfo"].(string)), &ext)
	if ext["modelId"] != "hunyuan_gpt_175B_0404" || ext["agentModeModelSetting"].(map[string]any)["modelId"] != "hunyuan_t1" {
		t.Fatal(ext)
	}
}

func TestProviderCloseInterruptsActiveStreamAndCleans(t *testing.T) {
	var deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/agent/conversation/create":
			fmt.Fprint(w, `{"id":"session"}`)
		case "/api/user/agent/conversation/v1/clear":
			deletes.Add(1)
			fmt.Fprint(w, "{}")
		case "/api/chat/session":
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	}))
	defer server.Close()
	p := newTestProvider(t, Options{BaseURL: server.URL})
	s, err := p.Open(context.Background(), testInput(t, p))
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

func TestSignatureTupleAndConfigurationValidation(t *testing.T) {
	p := newTestProvider(t, Options{})
	in := testInput(t, p)
	id, err := parseIdentity(in.Context)
	if err != nil {
		t.Fatal(err)
	}
	in.Context.Headers.Del("x-yuanbao-timestamp")
	if _, err = p.security(context.Background(), id, in.Context.Headers); err == nil {
		t.Fatal("accepted incomplete signature tuple")
	}
	for _, raw := range []string{`{"browserless_url":"ws://example.com"}`, `{} {}`, `{"base_url":"https://user:secret@example.com"}`, `{"browserless_ws":"http://example.com"}`, `{"signer_timeout_seconds":-1}`} {
		if _, err := New(context.Background(), json.RawMessage(raw), nil); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestSpeechAndBracketControlFrames(t *testing.T) {
	data := "data: {\"type\":\"text\",\"msg\":\"O\"}\n\nevent: speech_type\ndata: status\n\nevent: speech_type\ndata: text\n\ndata: {\"type\":\"text\",\"msg\":\"K\"}\n\ndata: [plugin: ]\n\ndata: [MSGINDEX:2]\n\ndata: [TRACEID:synthetic-trace]\n\ndata: [DONE]\n\n"
	s := &completionStream{sse: core.NewSSEReader(context.Background(), io.NopCloser(strings.NewReader(data)), 4096)}
	result, err := core.Collect(context.Background(), s, core.ChatConfig{}, 4096)
	if err != nil || result.Content != "OK" {
		t.Fatalf("%+v %v", result, err)
	}
	s = &completionStream{sse: core.NewSSEReader(context.Background(), io.NopCloser(strings.NewReader("data: [TRACEID:synthetic]\n\ndata: [DONE]\n\n")), 4096)}
	if _, err = core.Collect(context.Background(), s, core.ChatConfig{}, 4096); err == nil {
		t.Fatal("metadata-only stream accepted as an answer")
	}
}
