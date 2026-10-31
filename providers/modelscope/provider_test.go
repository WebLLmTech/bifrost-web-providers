package modelscope

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/WebLLmTech/bifrost-web-providers/core"
	"github.com/WebLLmTech/bifrost-web-providers/pluginruntime"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A real 1x1 PNG, small enough that tests never need external image services.
const pngBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jRZkAAAAASUVORK5CYII="

type adapterFixture struct {
	*pluginruntime.Runtime
	provider *Provider
	cfg      pluginruntime.Config
}

func (a *adapterFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/health" {
		r.URL.Path = "/providers/modelscope" + r.URL.Path
	}
	r.Body = http.MaxBytesReader(w, r.Body, a.cfg.MaxRequestMB<<20)
	a.Runtime.ServeHTTP(w, r)
}
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
func testAdapter(t *testing.T, handler http.HandlerFunc) *adapterFixture {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	registry := pluginruntime.NewRegistry()
	var provider *Provider
	registry.Register("modelscope", func(ctx context.Context, raw json.RawMessage, services *core.Services) (core.Provider, error) {
		p, err := New(ctx, raw, services)
		if err == nil {
			provider = p.(*Provider)
		}
		return p, err
	})
	cfg := pluginruntime.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.AllowPrivateAttachments = true
	cfg.MaxRequestMB = 100
	raw, _ := json.Marshal(Options{BaseURL: server.URL, PollIntervalMS: 1, TimeoutSeconds: 2})
	cfg.Providers = map[string]pluginruntime.ProviderConfig{"modelscope": {Type: "modelscope", Options: raw}}
	runtime, err := pluginruntime.New(context.Background(), cfg, registry)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close(context.Background()) })
	return &adapterFixture{Runtime: runtime, provider: provider, cfg: cfg}
}

func request(a *adapterFixture, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer test-provider-token")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}

func TestMultipartEditAsyncAndBase64Result(t *testing.T) {
	var submits, polls, downloads atomic.Int32
	var root string
	a := testAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/images/generations":
			submits.Add(1)
			if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer selected-key" || r.Header.Get("X-ModelScope-Async-Mode") != "true" {
				t.Error("missing submit method/auth/async header")
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body["model"] != "EA-Qwen-Image/Qwen-Image-2.1" || body["prompt"] != "Translate text" || body["seed"] != float64(42) {
				t.Errorf("lost model/prompt/seed: %v", body)
			}
			images, ok := body["image_url"].([]any)
			if !ok || len(images) != 1 || images[0] != "data:image/png;base64,"+pngBase64 {
				t.Error("multipart image was not converted to an image data URL")
			}
			if _, ok := body["response_format"]; ok {
				t.Error("response_format leaked into upstream payload")
			}
			writeJSON(w, 200, map[string]any{"task_id": "task-123", "task_status": "SUCCEED"})
		case "/v1/tasks/task-123":
			if r.Header.Get("X-ModelScope-Task-Type") != "image_generation" || r.Header.Get("Authorization") != "Bearer selected-key" {
				t.Error("missing poll headers")
			}
			if polls.Add(1) == 1 {
				writeJSON(w, 200, map[string]string{"task_status": "RUNNING"})
			} else {
				writeJSON(w, 200, map[string]any{"task_status": "SUCCEED", "output_images": []string{root + "/result.png"}})
			}
		case "/result.png":
			downloads.Add(1)
			if r.Header.Get("Authorization") != "" {
				t.Error("provider credential leaked to image download")
			}
			b, _ := base64.StdEncoding.DecodeString(pngBase64)
			_, _ = w.Write(b)
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	root = a.provider.opts.BaseURL
	var body bytes.Buffer
	m := multipart.NewWriter(&body)
	for k, v := range map[string]string{"model": "EA-Qwen-Image/Qwen-Image-2.1", "prompt": "Translate text", "seed": "42", "response_format": "b64_json"} {
		_ = m.WriteField(k, v)
	}
	f, _ := m.CreateFormFile("image[]", "input.png")
	b, _ := base64.StdEncoding.DecodeString(pngBase64)
	_, _ = f.Write(b)
	_ = m.Close()
	r := httptest.NewRequest("POST", "/v1/images/edits", &body)
	r.Header.Set("Content-Type", m.FormDataContentType())
	r.Header.Set("Authorization", "Bearer selected-key")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		Created int64 `json:"created"`
		Data    []struct {
			Base64 string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data) != 1 || response.Data[0].Base64 != pngBase64 || response.Created == 0 {
		t.Fatalf("invalid OpenAI result: %s", w.Body.String())
	}
	if w.Header().Get("X-ModelScope-Task-ID") != "task-123" || submits.Load() != 1 || polls.Load() != 2 || downloads.Load() != 1 {
		t.Fatal("incorrect submit/poll/download lifecycle")
	}
}

func TestGenerationURLResultDoesNotDownload(t *testing.T) {
	a := testAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/images/generations" {
			writeJSON(w, 200, map[string]string{"task_id": "gen"})
			return
		}
		if r.URL.Path == "/v1/tasks/gen" {
			writeJSON(w, 200, map[string]any{"task_status": "SUCCEED", "output_images": []string{"https://cdn.modelscope.cn/result.png"}})
			return
		}
		t.Error("unexpected image download")
		w.WriteHeader(500)
	})
	w := request(a, "/v1/images/generations", `{"model":"Qwen/Qwen-Image-2.1","prompt":"a dog"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"url":"https://cdn.modelscope.cn/result.png"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestImageValidationDoesNotSubmit(t *testing.T) {
	var calls atomic.Int32
	a := testAdapter(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) })
	for _, body := range []string{
		`{}`, `null`, `[]`, `{"model":"x","prompt":"y"} {}`,
		`{"model":"x","prompt":"y","stream":true}`,
		`{"model":"x","prompt":"y","stream":{}}`,
		`{"model":"x","prompt":"y","mask":[]}`,
		`{"model":"x","prompt":"y","response_format":{}}`,
		`{"model":"x","prompt":"y","n":2}`,
		`{"model":"x","prompt":"y","mask":"data:image/png;base64,abc"}`,
		`{"model":"x","prompt":"y","image_url":[]}`,
		`{"model":"x","prompt":"y","image_url":[17]}`,
		`{"model":"x","prompt":"y","image_url":"file:///etc/passwd"}`,
		`{"model":"x","prompt":"y","image_url":"https://a","image":"https://b"}`,
		`{"model":"x","prompt":"y","image_url":"https://a","response_format":"bad"}`,
	} {
		t.Run(body, func(t *testing.T) {
			w := request(a, "/v1/images/edits", body)
			if w.Code != 400 {
				t.Errorf("got %d: %s", w.Code, w.Body.String())
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("invalid request submitted a task")
	}
}

func TestJSONImageAliases(t *testing.T) {
	for _, alias := range []string{"image", "images", "image_url"} {
		t.Run(alias, func(t *testing.T) {
			a := testAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					var b map[string]any
					_ = json.NewDecoder(r.Body).Decode(&b)
					v, ok := b["image_url"].([]any)
					if !ok || len(v) != 2 {
						t.Error("JSON image array not preserved")
					}
					writeJSON(w, 200, map[string]string{"task_id": "json"})
				} else {
					writeJSON(w, 200, map[string]any{"task_status": "SUCCEED", "output_images": []string{"https://cdn/image.png"}})
				}
			})
			w := request(a, "/v1/images/edits", fmt.Sprintf(`{"model":"x","prompt":"y","%s":["https://cdn/a","data:image/png;base64,%s"]}`, alias, pngBase64))
			if w.Code != 200 {
				t.Fatalf("%d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestUpstreamFailureNeverResubmits(t *testing.T) {
	for _, status := range []int{400, 401, 404, 413, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			a := testAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "<html>private request content</html>")
			})
			w := request(a, "/v1/images/generations", `{"model":"x","prompt":"y"}`)
			if w.Code != status || calls.Load() != 1 || strings.Contains(w.Body.String(), "private request content") {
				t.Fatalf("%d %s, calls=%d", w.Code, w.Body.String(), calls.Load())
			}
		})
	}
}

func TestTransientPollFailureRetriesPollOnly(t *testing.T) {
	var submits, polls atomic.Int32
	a := testAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			submits.Add(1)
			writeJSON(w, 200, map[string]string{"task_id": "retry"})
			return
		}
		if polls.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		writeJSON(w, 200, map[string]any{"task_status": "SUCCEED", "output_images": []string{"https://cdn/image.png"}})
	})
	w := request(a, "/v1/images/generations", `{"model":"x","prompt":"y"}`)
	if w.Code != 200 || submits.Load() != 1 || polls.Load() != 2 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestTaskFailureAndMalformedResponses(t *testing.T) {
	for name, response := range map[string]string{
		"failed":            `{"task_status":"FAILED","message":"generation rejected"}`,
		"failed-with-image": `{"task_status":"FAILED","output_images":["https://cdn/image.png"]}`,
		"nested-error":      `{"task_status":"FAILED","errors":{"message":"invalid response format"}}`,
		"canceled":          `{"task_status":"CANCELED"}`,
		"empty":             `{"task_status":"SUCCEED","output_images":[]}`,
		"malformed":         `<html>oops</html>`,
		"missing-task-id":   `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			a := testAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					_, _ = io.WriteString(w, `{"task_id":"failure"}`)
				} else {
					_, _ = io.WriteString(w, response)
				}
			})
			w := request(a, "/v1/images/generations", `{"model":"x","prompt":"y"}`)
			if w.Code != 502 || !strings.Contains(w.Body.String(), "failure") {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestCancellationStopsPolling(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	a := testAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, 200, map[string]string{"task_id": "cancel"})
		cancel()
	})
	r := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"model":"x","prompt":"y"}`)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer test")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 499 || calls.Load() != 1 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestTimeoutReportsSubmittedTask(t *testing.T) {
	a := testAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"task_id": "slow", "task_status": "RUNNING"})
	})
	a.provider.opts.TimeoutSeconds = 1
	a.provider.opts.PollIntervalMS = 2000
	start := time.Now()
	w := request(a, "/v1/images/generations", `{"model":"x","prompt":"y"}`)
	if w.Code != 504 || !strings.Contains(w.Body.String(), "task_id=slow") || time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestBodyLimit(t *testing.T) {
	a := testAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("oversize request submitted")
		w.WriteHeader(500)
	})
	a.cfg.MaxRequestMB = 1
	w := request(a, "/v1/images/generations", `{"model":"x","prompt":"`+strings.Repeat("x", 2<<20)+`"}`)
	if w.Code != 413 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestChatAndHeaderIsolation(t *testing.T) {
	a := testAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/v1/chat/completions" || body["model"] != "chat" || body["temperature"] != 0.25 {
			t.Error("chat request changed")
		}
		if r.Header.Get("Authorization") != "Bearer upstream" || r.Header.Get("X-Bf-Vk") != "" || r.Header.Get("Cookie") != "" {
			t.Error("credential isolation failed")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\",\"reasoning_content\":\"reason\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	})
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"test"}],"stream":true,"temperature":0.25}`))
	r.Header.Set("Authorization", "Bearer upstream")
	r.Header.Set("X-Bf-Vk", "private")
	r.Header.Set("Cookie", "private=cookie")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "hello") || !strings.Contains(w.Body.String(), "reason") || !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestUnauthorizedAndHealth(t *testing.T) {
	a := testAdapter(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unauthenticated upstream call") })
	for path, want := range map[string]int{"/v1/images/generations": 401, "/health": 200} {
		w := httptest.NewRecorder()
		a.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != want {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
}

func TestProviderOptionsAndModels(t *testing.T) {
	for _, raw := range []string{`{"listen_addr":"127.0.0.1:8791"}`, `{} {}`, `{"base_url":"https://user:secret@example.com"}`, `{"base_url":"https://example.com?token=secret"}`, `{"timeout_seconds":-1}`, `{"base_url":"https://example.com","upstream_url":"https://other.com"}`} {
		if _, err := New(context.Background(), json.RawMessage(raw), nil); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	a := testAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer model-token" {
			t.Error("incorrect models request")
		}
		writeJSON(w, 200, map[string]any{"data": []map[string]string{{"id": "org/model_with_underscores"}}})
	})
	models, err := a.provider.Models(context.Background(), core.RequestContext{Credential: core.Credential{Value: "model-token"}})
	if err != nil || len(models) != 1 || models[0].ID != "org/model_with_underscores" {
		t.Fatalf("%+v %v", models, err)
	}
}
func TestErrorsRedactSelectedTokenAndKeepTaskID(t *testing.T) {
	a := testAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			writeJSON(w, 200, map[string]string{"task_id": "redacted-task"})
		} else {
			writeJSON(w, 200, map[string]any{"task_status": "FAILED", "errors": map[string]string{"message": "failure token=test-provider-token"}})
		}
	})
	w := request(a, "/v1/images/generations", `{"model":"x","prompt":"y"}`)
	if w.Code != 502 || strings.Contains(w.Body.String(), "test-provider-token") || !strings.Contains(w.Body.String(), "[redacted]") || w.Header().Get("X-ModelScope-Task-ID") != "redacted-task" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}
func TestProviderCloseStopsTaskPolling(t *testing.T) {
	submitted := make(chan struct{})
	a := testAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"task_id": "closing", "task_status": "RUNNING"})
		if r.Method == "POST" {
			close(submitted)
		}
	})
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- request(a, "/v1/images/generations", `{"model":"x","prompt":"y"}`) }()
	<-submitted
	a.provider.Close(context.Background())
	select {
	case w := <-result:
		if w.Code != 499 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("task waiter leaked")
	}
}
func TestOutputDownloadBlocksPrivateHosts(t *testing.T) {
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { downloads.Add(1) }))
	defer server.Close()
	p, err := New(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(context.Background())
	_, err = p.(*Provider).results.Resolve(context.Background(), core.FileRef{Source: server.URL + "/image.png"})
	if err == nil || downloads.Load() != 0 {
		t.Fatal("private result address was fetched")
	}
}

func TestOutputBudgetAppliesAcrossImages(t *testing.T) {
	var root string
	a := testAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST":
			writeJSON(w, 200, map[string]string{"task_id": "budget"})
		case strings.HasPrefix(r.URL.Path, "/v1/tasks/"):
			writeJSON(w, 200, map[string]any{"task_status": "SUCCEED", "output_images": []string{root + "/a.png", root + "/b.png"}})
		default:
			b, _ := base64.StdEncoding.DecodeString(pngBase64)
			b = append(b, make([]byte, 600<<10)...)
			w.Write(b)
		}
	})
	root = a.provider.opts.BaseURL
	a.provider.opts.MaxResultMB = 1
	w := request(a, "/v1/images/generations", `{"model":"x","prompt":"y","response_format":"b64_json"}`)
	if w.Code != 502 || w.Header().Get("X-ModelScope-Task-ID") != "budget" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}
