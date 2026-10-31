// Package modelscope migrates the existing Bifrost ModelScope adapter into the
// common plugin. No separate listener, process or credential store is created.
package modelscope

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/WebLLmTech/bifrost-web-providers/core"
)

type Options struct {
	BaseURL        string `json:"base_url,omitempty"`
	UpstreamURL    string `json:"upstream_url,omitempty"`
	PollIntervalMS int    `json:"poll_interval_ms,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	MaxResultMB    int64  `json:"max_result_mb,omitempty"`
}
type Provider struct {
	opts     Options
	http     *core.HTTPClient
	results  *core.FileResolver
	ownHTTP  bool
	ownFiles bool
	base     context.Context
	cancel   context.CancelFunc
}

func New(ctx context.Context, raw json.RawMessage, services *core.Services) (core.Provider, error) {
	var o Options
	if len(raw) > 0 && string(raw) != "null" {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if d.Decode(&o) != nil || d.Decode(new(any)) != io.EOF {
			return nil, core.Invalid("invalid modelscope options")
		}
	}
	if o.BaseURL != "" && o.UpstreamURL != "" {
		return nil, core.Invalid("Specify only one of base_url and upstream_url")
	}
	if o.BaseURL == "" {
		o.BaseURL = o.UpstreamURL
	}
	if o.BaseURL == "" {
		o.BaseURL = "https://api-inference.modelscope.ai"
	}
	u, err := url.Parse(o.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, core.Invalid("ModelScope base URL must use HTTP(S) without credentials or query")
	}
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/v1")
	u.RawPath = ""
	o.BaseURL = u.String()
	if o.PollIntervalMS == 0 {
		o.PollIntervalMS = 2000
	}
	if o.TimeoutSeconds == 0 {
		o.TimeoutSeconds = 540
	}
	if o.MaxResultMB == 0 {
		o.MaxResultMB = 64
	}
	if o.PollIntervalMS < 1 || o.PollIntervalMS > 60000 || o.TimeoutSeconds < 1 || o.TimeoutSeconds > 3600 || o.MaxResultMB < 1 || o.MaxResultMB > 512 {
		return nil, core.Invalid("Invalid ModelScope timeout or size limits")
	}
	base, cancel := context.WithCancel(ctx)
	p := &Provider{opts: o, base: base, cancel: cancel}
	if services != nil {
		p.http = services.HTTP
		if services.Files != nil {
			p.results = &core.FileResolver{Client: services.Files.Client, MaxBytes: o.MaxResultMB << 20}
		}
	}
	if p.http == nil {
		p.http = core.NewHTTPClient()
		p.ownHTTP = true
	}
	if p.results == nil {
		p.results = core.NewFileResolver(o.MaxResultMB<<20, false)
		p.ownFiles = true
	}
	return p, nil
}
func (p *Provider) Close(context.Context) error {
	p.cancel()
	if p.ownHTTP {
		p.http.Close()
	}
	if p.ownFiles {
		p.results.Close()
	}
	return nil
}
func (p *Provider) request(ctx context.Context, rc core.RequestContext, method, path string, body []byte) (*http.Request, error) {
	if rc.Credential.Value == "" {
		return nil, core.Unauthorized("ModelScope provider token is required")
	}
	req, err := http.NewRequestWithContext(ctx, method, p.opts.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+rc.Credential.Value)
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}
func (p *Provider) lifetime(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithTimeout(parent, time.Duration(p.opts.TimeoutSeconds)*time.Second)
	if p.base.Err() != nil {
		cancel()
	}
	stop := context.AfterFunc(p.base, cancel)
	return ctx, func() { stop(); cancel() }
}
func (p *Provider) Models(ctx context.Context, rc core.RequestContext) ([]core.Model, error) {
	ctx, cancel := p.lifetime(ctx)
	defer cancel()
	req, err := p.request(ctx, rc, http.MethodGet, "/v1/models", nil)
	if err != nil {
		return nil, err
	}
	var result struct {
		Data []core.Model `json:"data"`
	}
	if err = p.http.JSON(ctx, req, &result); err != nil {
		return nil, err
	}
	if result.Data == nil {
		return nil, core.Upstream(502, "invalid_models", "ModelScope returned no model list")
	}
	return result.Data, nil
}
func (p *Provider) Configure(r core.ChatRequest) (core.ChatConfig, error) {
	if raw, ok := r.Extra["n"]; ok && string(raw) != "1" {
		return core.ChatConfig{}, core.Unsupported("ModelScope chat supports one choice per request")
	}
	return (core.ModelStrategy{ToolMode: "native", ModelName: func(model string, _ []string) string { return model }}).Build(r)
}
func (p *Provider) Open(ctx context.Context, in core.RunInput) (core.EventStream, error) {
	ctx, cancel := p.lifetime(ctx)
	body, err := core.OpenAIChatBody(in.Request)
	if err != nil {
		cancel()
		return nil, err
	}
	req, err := p.request(ctx, in.Context, http.MethodPost, "/v1/chat/completions", body)
	if err != nil {
		cancel()
		return nil, err
	}
	stream, err := core.OpenAIChatStream(ctx, p.http, req, in.Request.Stream)
	if err != nil {
		cancel()
		return nil, err
	}
	return core.ManagedStream(ctx, stream, func(context.Context) error { cancel(); return nil }, time.Second), nil
}
