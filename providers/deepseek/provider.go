// Package deepseek ports the deepseek-deno-api service onto the shared core
// SDK: real access-token refresh, the DeepSeekHashV1 proof-of-work challenge,
// multipart upload with batched parse polling and the JSON-patch completion
// stream. No challenge is skipped or answered from a cache.
package deepseek

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/WebLLmTech/bifrost-web-providers/core"
)

// ModelOption overrides the static model list; id is required.
type ModelOption struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by,omitempty"`
}

// Options is the plugin configuration. Unknown fields fail the factory so
// typos never silently disable behavior.
type Options struct {
	BaseURL              string        `json:"base_url,omitempty"`
	UserAgent            string        `json:"user_agent,omitempty"`
	AppVersion           string        `json:"app_version,omitempty"`
	ClientVersion        string        `json:"client_version,omitempty"`
	SecChUa              string        `json:"sec_ch_ua,omitempty"`
	DeviceID             string        `json:"device_id,omitempty"`
	IntercomDeviceID     string        `json:"intercom_device_id,omitempty"`
	IntercomSession      string        `json:"intercom_session,omitempty"`
	TokenTTLSeconds      int           `json:"token_ttl_seconds,omitempty"`
	PowMaxIterations     int           `json:"pow_max_iterations,omitempty"`
	FilePollIntervalMS   int           `json:"file_poll_interval_ms,omitempty"`
	FileParseTimeoutMS   int           `json:"file_parse_timeout_ms,omitempty"`
	FilePollMaxBackoffMS int           `json:"file_poll_max_backoff_ms,omitempty"`
	MaxConcurrentUploads int           `json:"max_concurrent_uploads,omitempty"`
	Models               []ModelOption `json:"models,omitempty"`
}

const (
	defaultUserAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"
	defaultAppVersion  = "20241129.1"
	defaultClientVer   = "2.5.0"
	defaultSecChUa     = `"Chromium";v="133", "Google Chrome";v="133", "Not?A_Brand";v="99"`
	defaultPowMaxIter  = 2_000_000
	defaultModelOwner  = "deepseek-free-api"
)

// staticModels mirrors the old utils.ts model list. Descriptions were dropped:
// core.Model has no description field and Bifrost ignores it.
var staticModelIDs = []string{
	"deepseek",
	"deepseek_search",
	"deepseek_think",
	"deepseek_think_search",
	"deepseek_expert",
	"deepseek_expert_search",
	"deepseek_expert_think",
	"deepseek_expert_think_search",
}

func applyDefaults(o *Options) error {
	if o.BaseURL == "" {
		o.BaseURL = defaultBaseURL
	}
	u, err := url.Parse(o.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return core.Invalid("base_url must be an absolute http(s) URL")
	}
	o.BaseURL = strings.TrimSuffix(u.Scheme + "://" + u.Host + u.Path, "/")
	if o.UserAgent == "" {
		o.UserAgent = defaultUserAgent
	}
	if o.AppVersion == "" {
		o.AppVersion = defaultAppVersion
	}
	if o.ClientVersion == "" {
		o.ClientVersion = defaultClientVer
	}
	if o.SecChUa == "" {
		o.SecChUa = defaultSecChUa
	}
	defaults := []struct {
		name string
		value *int
		def  int
	}{
		{"token_ttl_seconds", &o.TokenTTLSeconds, 3600},
		{"pow_max_iterations", &o.PowMaxIterations, defaultPowMaxIter},
		{"file_poll_interval_ms", &o.FilePollIntervalMS, 3000},
		{"file_parse_timeout_ms", &o.FileParseTimeoutMS, 120000},
		{"file_poll_max_backoff_ms", &o.FilePollMaxBackoffMS, 30000},
		{"max_concurrent_uploads", &o.MaxConcurrentUploads, maxConcurrentUploads},
	}
	for _, d := range defaults {
		if *d.value < 0 {
			return core.Invalid(d.name + " must not be negative")
		}
		if *d.value == 0 {
			*d.value = d.def
		}
	}
	for _, m := range o.Models {
		if m.ID == "" {
			return core.Invalid("models[].id is required")
		}
	}
	return nil
}

// Provider is the Bifrost-facing adapter; apiClient holds all upstream calls.
type Provider struct {
	opts     Options
	models   []core.Model
	api      *apiClient
	cancel   context.CancelFunc
	ownHTTP  *core.HTTPClient
	ownFiles *core.FileResolver
}

// New is the pluginruntime factory entry point.
func New(ctx context.Context, raw json.RawMessage, services *core.Services) (core.Provider, error) {
	var opts Options
	if len(raw) > 0 && string(raw) != "null" {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&opts); err != nil {
			return nil, core.Invalid("deepseek options: " + err.Error())
		}
		if dec.More() {
			return nil, core.Invalid("deepseek options: unexpected trailing data")
		}
	}
	if err := applyDefaults(&opts); err != nil {
		return nil, err
	}

	httpClient := (*core.HTTPClient)(nil)
	files := (*core.FileResolver)(nil)
	p := &Provider{opts: opts}
	if services != nil && services.HTTP != nil {
		httpClient = services.HTTP
	} else {
		httpClient = core.NewHTTPClient()
		p.ownHTTP = httpClient
	}
	if services != nil && services.Files != nil {
		files = services.Files
	} else {
		files = core.NewFileResolver(0, false)
		p.ownFiles = files
	}

	baseCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.api = newAPIClient(baseCtx, opts, httpClient, files)
	p.models = buildModels(opts.Models)
	return p, nil
}

func buildModels(options []ModelOption) []core.Model {
	out := make([]core.Model, 0, len(options)+len(staticModelIDs))
	if len(options) == 0 {
		for _, id := range staticModelIDs {
			out = append(out, core.Model{ID: id, Object: "model", Created: 0, OwnedBy: defaultModelOwner})
		}
		return out
	}
	for _, m := range options {
		owner := m.OwnedBy
		if owner == "" {
			owner = defaultModelOwner
		}
		out = append(out, core.Model{ID: m.ID, Object: "model", Created: 0, OwnedBy: owner})
	}
	return out
}

func (p *Provider) Models(context.Context, core.RequestContext) ([]core.Model, error) {
	out := make([]core.Model, len(p.models))
	copy(out, p.models)
	return out, nil
}

var strategy = core.ModelStrategy{
	DefaultModel: "deepseek",
	Separator:    "_",
	ModelName:    core.StripFeatureFlags,
	ToolMode:     "delimited",
}

func (p *Provider) Configure(r core.ChatRequest) (core.ChatConfig, error) {
	return strategy.Build(r)
}

func (p *Provider) Open(ctx context.Context, in core.RunInput) (core.EventStream, error) {
	token := in.Context.Credential.Value
	if token == "" {
		return nil, core.Unauthorized("need token")
	}
	cfg := in.Config

	modelType := "default"
	for _, part := range strings.Split(cfg.Model, "_") {
		if part == "expert" {
			modelType = "expert"
			break
		}
	}

	// The old service extracted file URLs from the last message only.
	refs, err := core.ExtractFiles(in.Request.Messages, true)
	if err != nil {
		return nil, err
	}
	attachments, err := p.uploadAll(ctx, token, refs, modelType, cfg.Features.Thinking)
	if err != nil {
		return nil, err
	}
	refIDs := make([]string, 0, len(attachments))
	for _, a := range attachments {
		refIDs = append(refIDs, a.ID)
	}
	prompt := buildPrompt(cfg, in.Request.Messages)

	return core.Conversation(ctx,
		func(ctx context.Context) (string, error) {
			return p.api.createConversation(ctx, token)
		},
		func(ctx context.Context, sessionID string) (core.EventStream, error) {
			powResponse, err := p.api.solveChallengeFor(ctx, token, completionPath)
			if err != nil {
				return nil, err
			}
			return p.api.completionStream(ctx, token, &completionMessage{
				ChatSessionID:   sessionID,
				ParentMessageID: nil,
				Prompt:          prompt,
				Preempt:         false,
				RefFileIDs:      refIDs,
				SearchEnabled:   cfg.Features.Searching,
				ThinkingEnabled: cfg.Features.Thinking,
				ModelType:       modelType,
			}, powResponse)
		},
		func(ctx context.Context, sessionID string) error {
			return p.api.deleteConversation(ctx, sessionID, token)
		},
	)
}

// uploadAll mirrors Promise.all over uploadFile: concurrent uploads, first
// error cancels the rest. The per-account gate inside uploadFile bounds real
// upstream concurrency; parse polling happens outside the gate.
func (p *Provider) uploadAll(ctx context.Context, token string, refs []core.FileRef, modelType string, thinking bool) ([]attachment, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([]attachment, len(refs))
	var (
		mu      sync.Mutex
		first   error
		wg      sync.WaitGroup
	)
	for i, ref := range refs {
		wg.Add(1)
		go func(i int, ref core.FileRef) {
			defer wg.Done()
			att, err := p.api.uploadFile(ctx, token, ref, modelType, thinking)
			mu.Lock()
			if err != nil && first == nil {
				first = err
				cancel()
			}
			mu.Unlock()
			results[i] = att
		}(i, ref)
	}
	wg.Wait()
	if first != nil {
		return nil, first
	}
	return results, nil
}

// buildPrompt reproduces utils.ts mergeMessages: assistant tool calls are
// pre-serialized with the delimited markers (ToolCalls stay on the message so
// core.MergeMessages can resolve tool-result names), then merged messages are
// concatenated with system envelopes and the raw user envelope.
func buildPrompt(cfg core.ChatConfig, messages []core.Message) string {
	prepared := make([]core.Message, len(messages))
	copy(prepared, messages)
	if cfg.ToolProtocol != nil {
		m := cfg.ToolProtocol.Markers
		for i, msg := range prepared {
			if msg.Role != "assistant" || len(msg.ToolCalls) == 0 {
				continue
			}
			blocks := make([]string, 0, len(msg.ToolCalls))
			for _, call := range msg.ToolCalls {
				blocks = append(blocks, m.Start+"\n"+
					m.NameStart+call.Function.Name+m.NameEnd+"\n"+
					m.ArgsStart+call.Function.Arguments+m.ArgsEnd+"\n"+m.End)
			}
			prepared[i].Content = core.TextContent(msg.Content.Text() + "\n" + strings.Join(blocks, "\n"))
		}
	}
	merged := core.MergeMessages(prepared, cfg)
	var b strings.Builder
	for _, msg := range merged {
		if msg.Role == "system" {
			fmt.Fprintf(&b, "<message>system\n%s</message>\n", msg.Content.Text())
			continue
		}
		b.WriteString(msg.Content.Text())
	}
	return b.String()
}

func (p *Provider) Close(context.Context) error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.api != nil {
		p.api.close()
	}
	if p.ownHTTP != nil {
		p.ownHTTP.Close()
	}
	if p.ownFiles != nil {
		p.ownFiles.Close()
	}
	return nil
}
