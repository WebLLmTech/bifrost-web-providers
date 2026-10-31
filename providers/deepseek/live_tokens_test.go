package deepseek

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/WebLLmTech/bifrost-web-providers/core"
)

// Explicit opt-in auth-only audit; no generations are submitted. Input is a
// private temporary file, and output never contains returned access tokens.
func TestLiveTokens(t *testing.T) {
	path := os.Getenv("DEEPSEEK_LIVE_KEYS_FILE")
	if path == "" {
		t.Skip("opt-in live token audit")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read private credential file")
	}
	var entries []struct {
		KeyID      string
		Credential core.Credential
	}
	if json.Unmarshal(data, &entries) != nil {
		t.Fatal("invalid credential file")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	provider, err := New(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := provider.(*Provider)
	defer p.Close(context.Background())
	results := []map[string]any{}
	for _, entry := range entries {
		result := map[string]any{"key_id": entry.KeyID, "valid": false}
		request, _ := http.NewRequestWithContext(ctx, "GET", p.api.base+usersCurrentPath, nil)
		applyHeaders(request, p.api.generateHeaders(entry.Credential.Value))
		response, err := p.api.http.Do(ctx, request)
		if err != nil {
			result["transport_error"] = true
			results = append(results, result)
			continue
		}
		result["http_status"] = response.StatusCode
		raw, err := core.ReadBounded(response.Body, 2<<20)
		response.Body.Close()
		var envelope struct {
			Code *int            `json:"code"`
			Msg  string          `json:"msg"`
			Data json.RawMessage `json:"data"`
		}
		if err != nil || json.Unmarshal(raw, &envelope) != nil {
			result["invalid_json"] = true
			results = append(results, result)
			continue
		}
		result["code"] = envelope.Code
		message := envelope.Msg
		var details struct {
			BizCode *int   `json:"biz_code"`
			BizMsg  string `json:"biz_msg"`
			BizData struct {
				Token string `json:"token"`
			} `json:"biz_data"`
		}
		json.Unmarshal(envelope.Data, &details)
		if details.BizCode != nil {
			result["biz_code"] = *details.BizCode
		}
		if details.BizMsg != "" {
			message += " " + details.BizMsg
		}
		if entry.Credential.Value != "" {
			message = strings.ReplaceAll(message, entry.Credential.Value, "[redacted]")
		}
		if len(message) > 300 {
			message = message[:300]
		}
		result["message"] = strings.TrimSpace(message)
		token, isString := jsonString(envelope.Data)
		result["valid"] = response.StatusCode >= 200 && response.StatusCode < 300 && envelope.Code != nil && *envelope.Code == 0 && ((isString && token != "") || details.BizData.Token != "")
		results = append(results, result)
		b, _ := json.Marshal(result)
		fmt.Printf("token-check %s\n", b)
	}
	output := os.Getenv("DEEPSEEK_LIVE_RESULTS_FILE")
	if output != "" {
		b, _ := json.MarshalIndent(results, "", "  ")
		if os.WriteFile(output, b, 0600) != nil {
			t.Fatal("cannot write audit results")
		}
	}
}
