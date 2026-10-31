// An isolated, offline integration test against the real dynamic Bifrost host.
// Run via scripts/test-gateway.sh after building deploy/Dockerfile.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

var upstreamCalls, created, deleted atomic.Int32

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	upstream := httptest.NewServer(http.HandlerFunc(mock))
	defer upstream.Close()
	dir, err := os.MkdirTemp("", "gateway-test-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	plugin := map[string]any{"name": "web-providers", "enabled": true, "path": "/plugins/web-providers.so", "config": map[string]any{"listen_addr": "127.0.0.1:8792", "providers": map[string]any{
		"deepseek-web": map[string]any{"type": "deepseek", "options": map[string]any{"base_url": upstream.URL}},
		"yuanbao-web":  map[string]any{"type": "yuanbao", "options": map[string]any{"base_url": upstream.URL}},
	}}}
	allowed := map[string]bool{"list_models": true, "chat_completion": true, "chat_completion_stream": true, "responses": true, "responses_stream": true}
	providers := map[string]any{}
	credentials := map[string]string{"deepseek-web": "synthetic-refresh", "yuanbao-web": "token:synthetic-token agentId:agent hy_user:user"}
	for name, value := range credentials {
		providers[name] = map[string]any{"keys": []any{map[string]any{"id": name + "-key", "name": "fixture", "value": value, "models": []string{"*"}, "weight": 1, "is_active": true, "aliases": map[string]string{"fixture-alias": map[string]string{"deepseek-web": "deepseek", "yuanbao-web": "deep_seek_v3"}[name]}}},
			"network_config":         map[string]any{"base_url": "http://127.0.0.1:8792/providers/" + name, "allow_private_network": true, "max_retries": 0},
			"custom_provider_config": map[string]any{"base_provider_type": "openai", "allowed_requests": allowed},
		}
	}
	config := map[string]any{
		"client":    map[string]any{"enable_logging": true, "enforce_auth_on_inference": true},
		"providers": providers, "plugins": []any{plugin},
		"governance": map[string]any{"virtual_keys": []any{map[string]any{"id": "vk-fixture", "name": "fixture", "value": "sk-bf-synthetic-fixture", "is_active": true, "provider_configs": []any{
			map[string]any{"provider": "deepseek-web", "allowed_models": []string{"deepseek", "deepseek_search", "fixture-alias"}, "key_ids": []string{"*"}, "weight": 1},
			map[string]any{"provider": "yuanbao-web", "allowed_models": []string{"deep_seek_v3", "hunyuan_t1"}, "key_ids": []string{"*"}, "weight": 1},
		}}}},
	}
	raw, _ := json.Marshal(config)
	if err = os.WriteFile(filepath.Join(dir, "config.json"), raw, 0600); err != nil {
		return err
	}
	logs, err := os.Create(filepath.Join(dir, "gateway.log"))
	if err != nil {
		return err
	}
	defer logs.Close()
	cmd := exec.Command("/app/main", "-app-dir", dir, "-port", "18880", "-host", "127.0.0.1", "-log-level", "warn")
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err = cmd.Start(); err != nil {
		return err
	}
	defer func() {
		cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			<-done
		}
		if err != nil {
			b, _ := os.ReadFile(filepath.Join(dir, "gateway.log"))
			fmt.Fprintln(os.Stderr, string(b))
		}
	}()
	base := "http://127.0.0.1:18880"
	client := &http.Client{Timeout: 15 * time.Second}
	deadline := time.Now().Add(60 * time.Second)
	for {
		res, e := client.Get(base + "/health")
		if e == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			err = fmt.Errorf("Bifrost startup timeout")
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
	request := func(path string, body any, auth bool) (int, []byte, error) {
		b, _ := json.Marshal(body)
		req, e := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(b))
		if e != nil {
			return 0, nil, e
		}
		req.Header.Set("Content-Type", "application/json")
		if auth {
			req.Header.Set("Authorization", "Bearer sk-bf-synthetic-fixture")
		}
		req.Header.Set("x-bf-eh-x-yuanbao-uskey", "fixture")
		req.Header.Set("x-bf-eh-x-yuanbao-timestamp", "100")
		req.Header.Set("x-bf-eh-x-yuanbao-hy92", strings.Repeat("h", 38))
		res, e := client.Do(req)
		if e != nil {
			return 0, nil, e
		}
		defer res.Body.Close()
		data, e := io.ReadAll(res.Body)
		return res.StatusCode, data, e
	}
	chatBody := func(model string, stream bool) map[string]any {
		return map[string]any{"model": model, "messages": []any{map[string]string{"role": "user", "content": "hello"}}, "stream": stream}
	}
	status, data, e := request("/v1/chat/completions", chatBody("deepseek-web/deepseek", false), false)
	if e != nil || status != 401 || upstreamCalls.Load() != 0 {
		err = fmt.Errorf("authentication not enforced: status %d body %s err %v calls %d", status, data, e, upstreamCalls.Load())
		return err
	}
	for _, model := range []string{"deepseek-web/deepseek", "yuanbao-web/deep_seek_v3"} {
		for _, stream := range []bool{false, true} {
			status, data, e = request("/v1/chat/completions", chatBody(model, stream), true)
			if e != nil || status != 200 || !bytes.Contains(data, []byte("fixture answer")) {
				err = fmt.Errorf("chat %s stream %v: status %d body %s err %v", model, stream, status, data, e)
				return err
			}
			if stream && bytes.Count(data, []byte("data: [DONE]")) != 1 {
				err = fmt.Errorf("invalid done markers: %s", data)
				return err
			}
			fmt.Printf("PASS real Bifrost chat %s stream=%v\n", model, stream)
		}
		for _, stream := range []bool{false, true} {
			body := map[string]any{"model": model, "input": "hello", "stream": stream}
			status, data, e = request("/v1/responses", body, true)
			if e != nil || status != 200 || !bytes.Contains(data, []byte("fixture answer")) {
				err = fmt.Errorf("responses %s stream %v: status %d body %s err %v", model, stream, status, data, e)
				return err
			}
			fmt.Printf("PASS real Bifrost Responses %s stream=%v\n", model, stream)
		}
	}

	for _, model := range []string{"deepseek-web/deepseek", "yuanbao-web/deep_seek_v3"} {
		for _, stream := range []bool{false, true} {
			body := chatBody(model, stream)
			body["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "result", "strict": true, "schema": map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]string{"type": "boolean"}}, "required": []string{"ok"}, "additionalProperties": false}}}
			status, data, e = request("/v1/chat/completions", body, true)
			if e != nil || status != 200 || !bytes.Contains(data, []byte("ok")) {
				err = fmt.Errorf("JSON %s stream %v: status %d body %s err %v", model, stream, status, data, e)
				return err
			}
			body = chatBody(model, stream)
			body["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]string{"type": "string"}}, "required": []string{"city"}}}}}
			body["tool_choice"] = "required"
			status, data, e = request("/v1/chat/completions", body, true)
			if e != nil || status != 200 || !bytes.Contains(data, []byte("tool_calls")) || !bytes.Contains(data, []byte("lookup")) {
				err = fmt.Errorf("tools %s stream %v: status %d body %s err %v", model, stream, status, data, e)
				return err
			}
		}
		fmt.Printf("PASS real Bifrost JSON schema and split tool markers %s\n", model)
	}
	status, data, e = request("/v1/chat/completions", chatBody("deepseek-web/fixture-alias", false), true)
	if e != nil || status != 200 {
		err = fmt.Errorf("model alias: status %d body %s err %v", status, data, e)
		return err
	}
	before := upstreamCalls.Load()
	status, data, e = request("/v1/chat/completions", chatBody("deepseek-web/not-allowed", false), true)
	if e != nil || status < 400 || upstreamCalls.Load() != before {
		err = fmt.Errorf("governance model scope bypassed: status %d body %s", status, data)
		return err
	}
	if created.Load() != deleted.Load() || created.Load() != 17 {
		err = fmt.Errorf("session leak: created=%d deleted=%d", created.Load(), deleted.Load())
		return err
	}
	fmt.Println("PASS authentication, selected provider keys, forwarded signature headers, model scope and session cleanup")
	return nil
}
func biz(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"biz_code": 0, "biz_data": v}})
}
func mock(w http.ResponseWriter, r *http.Request) {
	upstreamCalls.Add(1)
	if strings.HasPrefix(r.URL.Path, "/api/v0/") {
		expected := "Bearer synthetic-access"
		if r.URL.Path == "/api/v0/users/current" {
			expected = "Bearer synthetic-refresh"
		}
		if r.Header.Get("Authorization") != expected {
			http.Error(w, "incorrect selected key", 401)
			return
		}
	} else if !strings.Contains(r.Header.Get("Cookie"), "hy_token=synthetic-token") {
		http.Error(w, "incorrect selected cookie", 401)
		return
	}
	switch r.URL.Path {
	case "/api/v0/users/current":
		fmt.Fprint(w, `{"code":0,"data":"synthetic-access"}`)
	case "/api/v0/chat_session/create":
		created.Add(1)
		biz(w, map[string]any{"chat_session": map[string]string{"id": "deepseek-session"}})
	case "/api/v0/chat_session/delete":
		deleted.Add(1)
		fmt.Fprint(w, "{}")
	case "/api/v0/chat/create_pow_challenge":
		biz(w, map[string]any{"challenge": map[string]any{"algorithm": "DeepSeekHashV1", "challenge": "98bf31ee5d56e86c9e2eb344cdc77e048e76e6cbeec8588342fbafc6deb579b8", "salt": "7d5ef52aa0ce83940e1d", "expire_at": 1780227446451, "difficulty": 144000, "signature": "synthetic"}})
	case "/api/v0/chat/completion":
		if r.Header.Get("X-Ds-Pow-Response") == "" {
			http.Error(w, "missing proof of work", 400)
			return
		}

		output := fixtureOutput(r)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"v\":{\"response\":{\"fragments\":[{\"id\":0,\"type\":\"THINK\",\"content\":\"fixture reasoning\"},{\"id\":1,\"type\":\"RESPONSE\",\"content\":\"\"}]}}}\n\n")
		for _, text := range splitOutput(output) {
			b, _ := json.Marshal(map[string]any{"p": "response/fragments/1/content", "v": text})
			fmt.Fprintf(w, "data: %s\n\n", b)
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "data: {\"p\":\"response/quasi_status\",\"v\":\"FINISHED\"}\n\n")

	case "/api/user/agent/conversation/create":
		if r.Header.Get("X-Uskey") != "fixture" {
			http.Error(w, "missing signature override", 400)
			return
		}
		created.Add(1)
		fmt.Fprint(w, `{"code":0,"id":"yuanbao-session"}`)
	case "/api/user/agent/conversation/v1/clear":
		deleted.Add(1)
		fmt.Fprint(w, "{}")
	case "/api/chat/yuanbao-session":
		if r.Header.Get("X-Uskey") != "fixture" {
			http.Error(w, "missing signature override", 400)
			return
		}

		output := fixtureOutput(r)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"think\",\"content\":\"fixture reasoning\"}\n\n")
		w.(http.Flusher).Flush()
		for _, text := range splitOutput(output) {
			b, _ := json.Marshal(map[string]string{"type": "text", "msg": text})
			fmt.Fprintf(w, "data: %s\n\n", b)
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")

	default:
		http.NotFound(w, r)
	}
}

// Split inside protocol delimiters as well as ordinary text.
func splitOutput(s string) []string {
	if len(s) < 6 {
		return []string{s}
	}
	return []string{s[:3], s[3 : len(s)-3], s[len(s)-3:]}
}
func fixtureOutput(r *http.Request) string {
	var body struct {
		Prompt string `json:"prompt"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if strings.Contains(body.Prompt, "Tool lookup:") {
		marker := regexp.MustCompile(`<tc_[a-f0-9]+>`).FindString(body.Prompt)
		if marker != "" {
			return marker + "<name>lookup</name><arguments>{\"city\":\"Paris\"}</arguments>" + strings.Replace(marker, "<", "</", 1)
		}
		return "[ToolCalls][Call:lookup][CallParameter:city]Paris[/CallParameter][/Call][/ToolCalls]"
	}
	if strings.Contains(body.Prompt, "Return only a valid JSON object") {
		return `{"ok":true}`
	}
	return "fixture answer"
}
