package yuanbao

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/WebLLmTech/bifrost-web-providers/core"
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36 Edg/148.0.0.0"

type identity struct{ Token, AgentID, User string }

func parseIdentity(rc core.RequestContext) (identity, error) {
	f := rc.Credential.Fields
	id := identity{Token: f["token"], AgentID: f["agentId"], User: f["hy_user"]}
	if id.Token == "" || id.AgentID == "" || id.User == "" {
		return id, core.Unauthorized("Yuanbao requires token, agentId and hy_user credential fields")
	}
	for _, v := range []string{id.Token, id.User} {
		cookie := http.Cookie{Name: "value", Value: v}
		if cookie.Valid() != nil {
			return id, core.Unauthorized("invalid Yuanbao cookie credential")
		}
	}
	if strings.ContainsAny(id.AgentID, "/?#\r\n\x00") {
		return id, core.Unauthorized("invalid Yuanbao agentId")
	}
	return id, nil
}

var headerAliases = map[string][]string{
	"X-Uskey":          {"x-yuanbao-uskey", "x-uskey"},
	"X-Bus-Params-Md5": {"x-yuanbao-bus-params-md5", "x-bus-params-md5"},
	"X-Timestamp":      {"x-yuanbao-timestamp", "x-timestamp"},
	"X-Device-Id":      {"x-yuanbao-device-id", "x-device-id"},
	"X-Hy92":           {"x-yuanbao-hy92", "x-hy92"},
	"X-Hy93":           {"x-yuanbao-hy93", "x-hy93"},
	"X-Exp-Params":     {"x-yuanbao-exp-params", "x-exp-params"},
	"X-Trid-Channel":   {"x-yuanbao-trid-channel", "x-trid-channel"},
	"X-Web-Ch-Id":      {"x-yuanbao-web-ch-id", "x-web-ch-id"},
}

func overrides(h http.Header) http.Header {
	out := http.Header{}
	for dest, sources := range headerAliases {
		for _, source := range sources {
			if v := h.Get(source); v != "" {
				out.Set(dest, v)
				break
			}
		}
	}
	return out
}
func (p *Provider) security(ctx context.Context, id identity, headers http.Header) (http.Header, error) {
	h := overrides(headers)
	if h.Get("X-Uskey") != "" {
		if h.Get("X-Timestamp") == "" || h.Get("X-HY92") == "" {
			return nil, core.Invalid("explicit Yuanbao uskey requires matching timestamp and hy92")
		}
		return h, nil
	}
	if p.opts.BrowserlessWS == "" {
		return nil, core.Unsupported("Yuanbao requires browserless_ws or explicit uskey, timestamp and hy92 headers")
	}
	signer, release, err := p.acquireSigner(id)
	if err != nil {
		return nil, err
	}
	defer release()
	signed, err := signer.sign(ctx)
	if err != nil {
		return nil, err
	}
	// A generated signature and its timestamp/h38 form one indivisible tuple.
	for key, values := range signed {
		h[key] = values
	}
	h.Del("X-Bus-Params-Md5")
	return h, nil
}
func (p *Provider) headers(id identity, security http.Header) http.Header {
	u, _ := url.Parse(p.opts.BaseURL)
	origin := u.Scheme + "://" + u.Host
	h := http.Header{}
	defaults := map[string]string{
		"Cookie":       "hy_source=web; hy_user=" + id.User + "; hy_token=" + id.Token,
		"chat_version": "v1", "x-agentid": id.AgentID, "x-id": id.User, "t-userid": id.User,
		"x-requested-with": "XMLHttpRequest", "x-source": "web", "x-platform": "win", "x-language": "zh-CN",
		"x-webversion": p.opts.WebVersion, "x-commit-tag": p.opts.CommitTag, "x-instance-id": "5", "x-ybuitest": "0", "x-webdriver": "0", "x-web-third-source": "main", "x-os-version": "Windows(10)-Blink",
		"Content-Type": "application/json", "Accept": "application/json, text/plain, */*",
		"Accept-Language": "zh-CN,zh;q=0.9", "Cache-Control": "no-cache", "Origin": origin, "Pragma": "no-cache",
		"Sec-Ch-Ua":        `"Chromium";v="148", "Microsoft Edge";v="148", "Not/A)Brand";v="99"`,
		"Sec-Ch-Ua-Mobile": "?0", "Sec-Ch-Ua-Platform": `"Windows"`, "Sec-Fetch-Dest": "empty", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Site": "same-origin", "Referer": origin, "User-Agent": userAgent,
	}
	for k, v := range defaults {
		h.Set(k, v)
	}
	for k, v := range security {
		h[k] = append([]string(nil), v...)
	}
	ts := h.Get("X-Timestamp")
	if ts == "" {
		ts = strconv.FormatInt(time.Now().UnixMilli(), 10)
		h.Set("X-Timestamp", ts)
	}
	if h.Get("X-Bus-Params-Md5") == "" {
		sum := md5.Sum([]byte("h38=" + h.Get("X-HY92") + "&timestamp=" + ts + "&platform=web"))
		h.Set("X-Bus-Params-Md5", hex.EncodeToString(sum[:]))
	}
	return h
}
func (p *Provider) request(ctx context.Context, id identity, path string, body any, security http.Header) (*http.Request, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, p.opts.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	r.Header = p.headers(id, security)
	return r, nil
}
func (p *Provider) createConversation(ctx context.Context, id identity, security http.Header) (string, error) {
	req, err := p.request(ctx, id, "/api/user/agent/conversation/create", map[string]string{"agentId": id.AgentID}, security)
	if err != nil {
		return "", err
	}
	var out struct {
		ID   string `json:"id"`
		Code *int   `json:"code"`
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err = p.http.JSON(ctx, req, &out); err != nil {
		return "", err
	}
	if out.Code != nil && *out.Code != 0 {
		return "", core.Upstream(502, "upstream_rejected", "Yuanbao rejected conversation creation")
	}
	if out.ID == "" {
		out.ID = out.Data.ID
	}
	if out.ID == "" || strings.ContainsAny(out.ID, "/?#\r\n\x00") {
		return "", core.Upstream(502, "invalid_session", "Yuanbao returned no valid conversation ID")
	}
	return out.ID, nil
}
func (p *Provider) deleteConversation(ctx context.Context, id identity, chatID string) error {
	req, err := p.request(ctx, id, "/api/user/agent/conversation/v1/clear", map[string]any{"conversationIds": []string{chatID}, "uiOptions": map[string]bool{"noToast": true}}, nil)
	if err != nil {
		return err
	}
	resp, err := p.http.Do(ctx, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return core.ErrorFromStatus("Yuanbao cleanup", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}
func (p *Provider) completionBody(id identity, chatID, prompt string, cfg core.ChatConfig) map[string]any {
	internet := "autoInternetSearch"
	if cfg.Features.Searching {
		internet = "openInternetSearch"
	}
	search := ""
	if cfg.Features.Searching {
		search = internet
	}
	ext := map[string]any{"modelId": cfg.ModelName, "subModelId": "", "supportFunctions": map[string]string{"internetSearch": search}, "internetSearch": internet}
	if cfg.ModelName == "hunyuan_t1" {
		ext["modelId"] = "hunyuan_gpt_175B_0404"
		delete(ext, "subModelId")
		ext["agentModeModelSetting"] = map[string]string{"modelId": cfg.ModelName}
	}
	extJSON, _ := json.Marshal(ext)
	body := map[string]any{
		"model": "gpt_175B_0404", "prompt": prompt, "plugin": "", "displayPrompt": prompt, "displayPromptType": 1,
		"options":    map[string]any{"imageIntention": map[string]any{"needIntentionModel": true, "backendUpdateFlag": 2, "intentionStatus": true}},
		"multimedia": []any{}, "agentId": id.AgentID, "projectId": "", "isTemporary": false, "docOpenid": "", "applicationIdList": []any{},
		"chatModelExtInfo": string(extJSON), "supportHint": 1, "version": "v2", "chatModelId": cfg.ModelName,
		"supportFunctions": []string{"openAutoSearchSwitch", internet}, "extReportParams": nil, "isAtomInput": false, "conversationId": chatID,
		"offsetOfHour": *p.opts.TimezoneOffsetMinutes / 60, "offsetOfMinute": *p.opts.TimezoneOffsetMinutes % 60,
	}
	if cfg.Features.DeepSearching {
		body["isAiDeepSearch"] = true
		body["searchDeepMode"] = true
		body["searchDeepModeParentCid"] = chatID
		body["searchDeepModeParentIndex"] = 2
		body["searchDeepModeParentRepeatIndex"] = 0
		body["speechMode"] = 5
	}
	return body
}
func (p *Provider) completion(ctx context.Context, id identity, chatID, prompt string, cfg core.ChatConfig, security http.Header) (core.EventStream, error) {
	req, err := p.request(ctx, id, "/api/chat/"+url.PathEscape(chatID), p.completionBody(id, chatID, prompt, cfg), security)
	if err != nil {
		return nil, err
	}
	trace := make([]byte, 16)
	if _, err = rand.Read(trace); err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Input-Type", "text")
	req.Header.Set("X-Event-Input-Type", "11")
	req.Header.Set("X-Traceparent", hex.EncodeToString(trace))
	reader, err := p.http.SSE(ctx, req)
	if err != nil {
		return nil, err
	}
	return &completionStream{sse: reader}, nil
}
