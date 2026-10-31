package pluginruntime

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/WebLLmTech/bifrost-web-providers/core"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

func jsonResponse(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
func sendError(w http.ResponseWriter, err error, credential core.Credential) {
	secrets := []string{credential.Value}
	for _, value := range credential.Fields {
		secrets = append(secrets, value)
	}
	e := core.PublicError(err, secrets...)
	jsonResponse(w, e.Status, map[string]any{"error": e})
}
func (r *Runtime) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	defer func() {
		if recover() != nil {
			sendError(w, errors.New("provider panic"), core.Credential{})
		}
	}()
	if request.Method == "GET" && request.URL.Path == "/health" {
		var names []string
		for n := range r.providers {
			names = append(names, n)
		}
		sort.Strings(names)
		jsonResponse(w, 200, map[string]any{"status": "ok", "providers": names})
		return
	}
	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	if len(parts) < 4 || parts[0] != "providers" || parts[2] != "v1" {
		sendError(w, core.Upstream(404, "not_found", "Unknown provider endpoint"), core.Credential{})
		return
	}
	p := r.providers[parts[1]]
	if p == nil {
		sendError(w, core.Upstream(404, "unknown_provider", "Provider is not enabled"), core.Credential{})
		return
	}
	credential, err := core.Bearer(request.Header.Get("Authorization"))
	if err != nil {
		sendError(w, err, credential)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), time.Duration(r.cfg.RequestTimeoutSeconds)*time.Second)
	defer cancel()
	headers := request.Header.Clone()
	for k := range headers {
		if strings.HasPrefix(strings.ToLower(k), "x-bf-") || strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "Cookie") || strings.EqualFold(k, "Proxy-Authorization") {
			headers.Del(k)
		}
	}
	rc := core.RequestContext{Credential: credential, Headers: headers, RequestID: headers.Get("X-Request-ID")}
	route := strings.Join(parts[3:], "/")
	if route == "models" && request.Method == "GET" {
		models, err := p.Models(ctx, rc)
		if err != nil {
			sendError(w, err, credential)
			return
		}
		for i := range models {
			if models[i].Object == "" {
				models[i].Object = "model"
			}
			if models[i].OwnedBy == "" {
				models[i].OwnedBy = parts[1]
			}
		}
		jsonResponse(w, 200, map[string]any{"object": "list", "data": models})
		return
	}
	if route == "images/generations" || route == "images/edits" {
		r.images(ctx, w, request, p, rc, route == "images/edits")
		return
	}
	if route != "chat/completions" && route != "responses" {
		sendError(w, core.Unsupported("This provider endpoint is not supported"), credential)
		return
	}
	if request.Method != "POST" {
		w.Header().Set("Allow", "POST")
		sendError(w, core.Upstream(405, "method_not_allowed", "Use POST for inference"), credential)
		return
	}
	request.Body = http.MaxBytesReader(w, request.Body, r.cfg.MaxRequestMB<<20)
	stop := context.AfterFunc(ctx, func() { _ = request.Body.Close() })
	defer stop()
	var body core.ChatRequest
	if route == "responses" {
		body, err = readResponses(request.Body)
	} else {
		dec := json.NewDecoder(request.Body)
		err = dec.Decode(&body)
		if err == nil {
			var tail any
			if dec.Decode(&tail) != io.EOF {
				err = core.Invalid("Expected one JSON object")
			}
		}
	}
	if err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			sendError(w, core.Upstream(413, "request_too_large", "Request body exceeds configured limit"), credential)
		} else {
			sendError(w, core.Invalid("Invalid request JSON"), credential)
		}
		return
	}
	if len(body.Messages) == 0 {
		sendError(w, core.Invalid("messages must not be empty"), credential)
		return
	}
	for _, m := range body.Messages {
		switch m.Role {
		case "system", "developer", "user", "assistant", "tool":
		default:
			sendError(w, core.Invalid("Invalid message role"), credential)
			return
		}
	}
	cfg, err := p.Configure(body)
	if err != nil {
		sendError(w, err, credential)
		return
	}
	if err = core.ValidateConfig(cfg); err != nil {
		sendError(w, err, credential)
		return
	}
	stream, err := p.Open(ctx, core.RunInput{Request: body, Config: cfg, Context: rc})
	if err != nil {
		if stream != nil {
			_ = stream.Close()
		}
		sendError(w, err, credential)
		return
	}
	if stream == nil {
		sendError(w, core.Upstream(502, "empty_stream", "Provider returned no stream"), credential)
		return
	}
	stream = core.ManagedStream(ctx, stream, nil, 5*time.Second)
	defer stream.Close()
	id := core.NewID("chatcmpl")
	if route == "responses" {
		id = core.NewID("resp")
	}
	created := time.Now().Unix()
	if !body.Stream {
		completion, err := core.Collect(ctx, stream, cfg, r.cfg.MaxCompletionMB<<20)
		if err != nil {
			sendError(w, err, credential)
			return
		}
		if route == "responses" {
			response := responseObject(id, cfg.Model, created, completion)
			response["parallel_tool_calls"] = cfg.ParallelToolCalls
			jsonResponse(w, 200, response)
		} else {
			response := map[string]any{"id": id, "object": "chat.completion", "created": created, "model": cfg.Model, "choices": []any{map[string]any{"index": 0, "message": core.CompletionMessage(completion), "finish_reason": completion.FinishReason}}}
			if completion.Usage != nil {
				response["usage"] = completion.Usage
			}
			if urls := core.CitationURLs(completion.Citations); len(urls) > 0 {
				response["citations"] = urls
			}
			jsonResponse(w, 200, response)
		}
		return
	}
	r.stream(ctx, w, stream, body, cfg, id, created, route == "responses", credential)
}

type streamItem struct {
	event core.Event
	err   error
}

func (r *Runtime) stream(ctx context.Context, w http.ResponseWriter, source core.EventStream, body core.ChatRequest, cfg core.ChatConfig, id string, created int64, responses bool, credential core.Credential) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	source = core.NormalizeStream(source, cfg)
	defer source.Close()
	// Exactly one producer, one bounded slot. Close interrupts provider reads when
	// the HTTP client disconnects, a write fails, or the request deadline expires.
	events := make(chan streamItem, 1)
	go func() {
		defer close(events)
		for {
			e, err := func() (event core.Event, err error) {
				defer func() {
					if recover() != nil {
						err = core.Upstream(502, "provider_stream_panic", "Provider stream failed")
					}
				}()
				return source.Next(ctx)
			}()
			if e.ToolCall != nil {
				copy := *e.ToolCall
				e.ToolCall = &copy
			}
			if e.Usage != nil {
				copy := *e.Usage
				e.Usage = &copy
			}
			if len(e.Citations) > 0 {
				copy := make([]json.RawMessage, len(e.Citations))
				for i, c := range e.Citations {
					copy[i] = append(json.RawMessage(nil), c...)
				}
				e.Citations = copy
			}
			select {
			case events <- streamItem{e, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	write := func(event string, v any) error {
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if err = core.WriteSSE(w, event, data); err != nil {
			return err
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return nil
	}
	chunk := func(delta any, finish any) map[string]any {
		return map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": cfg.Model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	var sink *responseStream
	if responses {
		sink = newResponseStream(id, cfg.Model, created, write)
		sink.parallel = cfg.ParallelToolCalls
		if sink.start() != nil {
			return
		}
	} else {
		if write("", chunk(map[string]string{"role": "assistant"}, nil)) != nil {
			return
		}
	}
	ticker := time.NewTicker(time.Duration(r.cfg.HeartbeatSeconds) * time.Second)
	defer ticker.Stop()
	a := core.Accumulator{MaxBytes: r.cfg.MaxCompletionMB << 20}
	fail := func(err error) {
		secrets := []string{credential.Value}
		for _, v := range credential.Fields {
			secrets = append(secrets, v)
		}
		e := core.PublicError(err, secrets...)
		if responses {
			_ = write("error", map[string]any{"type": "error", "code": e.Code, "message": e.Message})
		} else {
			_ = write("", map[string]any{"error": e})
			_ = core.WriteSSE(w, "", []byte("[DONE]"))
		}
	}
	for {
		select {
		case <-ctx.Done():
			fail(ctx.Err())
			return
		case <-ticker.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		case item, ok := <-events:
			if !ok {
				fail(core.Upstream(502, "truncated_stream", "Provider stream closed without EOF"))
				return
			}
			if item.err != nil {
				if item.err != io.EOF {
					fail(item.err)
					return
				}
				result, err := a.Finish(cfg)
				if err != nil {
					fail(err)
					return
				}
				if responses {
					_ = sink.finish(result)
					return
				}
				// JSON formats are buffered until validation so invalid partial JSON is
				// never advertised as successful structured output.
				if cfg.ResponseFormat.Type != "" && cfg.ResponseFormat.Type != "text" {
					if write("", chunk(map[string]any{"content": result.Content}, nil)) != nil {
						return
					}
				}
				if write("", chunk(map[string]any{}, result.FinishReason)) != nil {
					return
				}
				if body.StreamOptions != nil && body.StreamOptions.IncludeUsage && result.Usage != nil {
					if write("", map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": cfg.Model, "choices": []any{}, "usage": result.Usage}) != nil {
						return
					}
				}
				_ = core.WriteSSE(w, "", []byte("[DONE]"))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				return
			}
			e := item.event
			if err := a.Add(e); err != nil {
				fail(err)
				return
			}
			if responses {
				if err := sink.event(e, cfg.ResponseFormat.Type); err != nil {
					return
				}
				continue
			}
			var delta any
			switch e.Kind {
			case core.EventContent:
				if cfg.ResponseFormat.Type != "" && cfg.ResponseFormat.Type != "text" {
					continue
				}
				delta = map[string]any{"content": e.Text}
			case core.EventReasoning:
				delta = map[string]any{"reasoning_content": e.Text}
			case core.EventToolCall:
				delta = map[string]any{"tool_calls": []any{e.ToolCall}}
			case core.EventCitations:
				citationChunk := chunk(map[string]any{}, nil)
				citationChunk["citations"] = core.CitationURLs(a.Result.Citations)
				if write("", citationChunk) != nil {
					return
				}
				continue
			default:
				continue
			}
			if write("", chunk(delta, nil)) != nil {
				return
			}
		}
	}
}
