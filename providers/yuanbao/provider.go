// Package yuanbao adapts Tencent Yuanbao's web protocol to the shared SDK.
package yuanbao

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/WebLLmTech/bifrost-web-providers/core"
)

type Options struct {
	BaseURL               string   `json:"base_url,omitempty"`
	BrowserlessWS         string   `json:"browserless_ws,omitempty"`
	WebVersion            string   `json:"web_version,omitempty"`
	CommitTag             string   `json:"commit_tag,omitempty"`
	TimezoneOffsetMinutes *int     `json:"timezone_offset_minutes,omitempty"`
	SignerTimeoutSeconds  int      `json:"signer_timeout_seconds,omitempty"`
	MaxSignerSessions     int      `json:"max_signer_sessions,omitempty"`
	QimeiAppKey           string   `json:"qimei_app_key,omitempty"`
	QimeiAppID            string   `json:"qimei_app_id,omitempty"`
	QimeiModuleIDs        []string `json:"qimei_module_ids,omitempty"`
}
type Provider struct {
	opts    Options
	http    *core.HTTPClient
	ownHTTP bool
	base    context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	signers map[[32]byte]*signerEntry
	closed  bool
}

var modelIDs = []string{
	"hunyuan_omnipotent_hy4", "hunyuan_gpt_175B_0404", "hunyuan_gpt_175B_0404_search",
	"hunyuan_gpt_175B_0404_deepsearch", "hunyuan_t1", "hunyuan_t1_search",
	"deep_seek_v3", "deep_seek_v3_search", "deep_seek", "deep_seek_search",
}

func New(ctx context.Context, raw json.RawMessage, services *core.Services) (core.Provider, error) {
	o := Options{}
	if len(raw) > 0 && string(raw) != "null" {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if d.Decode(&o) != nil || d.Decode(new(any)) != io.EOF {
			return nil, core.Invalid("invalid yuanbao options")
		}
	}
	if o.BaseURL == "" {
		o.BaseURL = "https://yuanbao.tencent.com"
	}
	u, err := url.Parse(o.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, core.Invalid("base_url must be an absolute http(s) URL without credentials or query")
	}
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	if o.BrowserlessWS != "" {
		u, err = url.Parse(o.BrowserlessWS)
		if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" || u.User != nil || u.Fragment != "" {
			return nil, core.Invalid("browserless_ws must be an absolute ws(s) URL")
		}
	}
	if o.WebVersion == "" {
		o.WebVersion = "2.83.11"
	}
	if o.CommitTag == "" {
		o.CommitTag = "0d2b8477"
	}
	if o.TimezoneOffsetMinutes == nil {
		offset := 480
		o.TimezoneOffsetMinutes = &offset
	}
	if *o.TimezoneOffsetMinutes < -1440 || *o.TimezoneOffsetMinutes > 1440 {
		return nil, core.Invalid("invalid timezone_offset_minutes")
	}
	if o.SignerTimeoutSeconds == 0 {
		o.SignerTimeoutSeconds = 90
	}
	if o.MaxSignerSessions == 0 {
		o.MaxSignerSessions = 4
	}
	if o.SignerTimeoutSeconds < 1 || o.SignerTimeoutSeconds > 300 || o.MaxSignerSessions < 1 || o.MaxSignerSessions > 64 {
		return nil, core.Invalid("invalid signer limits")
	}
	if o.QimeiAppKey == "" {
		o.QimeiAppKey = "0WEB05U9OEC1ZNRY"
	}
	if o.QimeiAppID == "" {
		o.QimeiAppID = "7800385"
	}
	if len(o.QimeiModuleIDs) == 0 {
		o.QimeiModuleIDs = []string{"12601", "77004"}
	}
	base, cancel := context.WithCancel(ctx)
	p := &Provider{opts: o, base: base, cancel: cancel, signers: map[[32]byte]*signerEntry{}}
	if services != nil {
		p.http = services.HTTP
	}
	if p.http == nil {
		p.http = core.NewHTTPClient()
		p.ownHTTP = true
	}
	return p, nil
}
func (p *Provider) Models(context.Context, core.RequestContext) ([]core.Model, error) {
	out := make([]core.Model, len(modelIDs))
	for i, id := range modelIDs {
		out[i] = core.Model{ID: id, Object: "model", OwnedBy: "yuanbao"}
	}
	return out, nil
}
func (p *Provider) Configure(r core.ChatRequest) (core.ChatConfig, error) {
	return (core.ModelStrategy{DefaultModel: "hunyuan_gpt_175B_0404", Separator: "_", ModelName: core.StripFeatureFlags, ToolMode: "bracket"}).Build(r)
}
func (p *Provider) Open(ctx context.Context, in core.RunInput) (core.EventStream, error) {
	id, err := parseIdentity(in.Context)
	if err != nil {
		return nil, err
	}
	refs, err := core.ExtractFiles(in.Request.Messages, false)
	if err != nil {
		return nil, err
	}
	if len(refs) > 0 {
		return nil, core.Unsupported("Yuanbao web attachment upload is not supported")
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.base, cancel)
	ok := false
	defer func() {
		if !ok {
			stop()
			cancel()
		}
	}()
	// Signatures are generated separately for session creation and completion.
	security, err := p.security(ctx, id, in.Context.Headers)
	if err != nil {
		return nil, err
	}
	prompt, err := core.PromptText(in.Request.Messages, in.Config)
	if err != nil {
		return nil, err
	}
	stream, err := core.Conversation(ctx,
		func(ctx context.Context) (string, error) { return p.createConversation(ctx, id, security) },
		func(ctx context.Context, chatID string) (core.EventStream, error) {
			security, err := p.security(ctx, id, in.Context.Headers)
			if err != nil {
				return nil, err
			}
			return p.completion(ctx, id, chatID, prompt, in.Config, security)
		},
		func(ctx context.Context, chatID string) error { return p.deleteConversation(ctx, id, chatID) },
	)
	if err != nil {
		return nil, err
	}
	ok = true
	return core.ManagedStream(ctx, stream, func(context.Context) error { stop(); cancel(); return nil }, time.Second), nil
}
func (p *Provider) Close(context.Context) error {
	p.cancel()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	entries := p.signers
	p.signers = map[[32]byte]*signerEntry{}
	p.mu.Unlock()
	for _, entry := range entries {
		entry.signer.close()
	}
	if p.ownHTTP {
		p.http.Close()
	}
	return nil
}
