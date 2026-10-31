package yuanbao

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"

	"github.com/WebLLmTech/bifrost-web-providers/core"
)

var refPattern = regexp.MustCompile(`\[(\d+(?:,\d+)*)\]\(@ref\)`)
var markPattern = regexp.MustCompile(`\[[^\[\]]*\]\(@mark_[^)]*\)`)
var citationPattern = regexp.MustCompile(`\[citation:\d+\]`)

func cleanMarkers(s string) string {
	s = refPattern.ReplaceAllStringFunc(s, func(v string) string {
		m := refPattern.FindStringSubmatch(v)
		return "[" + strings.Join(strings.Split(m[1], ","), "][") + "]"
	})
	s = markPattern.ReplaceAllString(s, "")
	return citationPattern.ReplaceAllString(s, "")
}

type completionStream struct {
	sse        *core.SSEReader
	queue      []core.Event
	done       bool
	seen       map[string]bool
	seenAnswer bool
}

func (s *completionStream) Close() error { return s.sse.Close() }
func (s *completionStream) Next(ctx context.Context) (core.Event, error) {
	for {
		if len(s.queue) > 0 {
			e := s.queue[0]
			s.queue = s.queue[1:]
			if e.Kind == core.EventContent && e.Text != "" {
				s.seenAnswer = true
			}
			if e.Kind == core.EventFinish && !s.seenAnswer {
				return core.Event{}, core.Upstream(502, "empty_completion", "Yuanbao returned no answer")
			}
			return e, nil
		}
		if s.done {
			return core.Event{}, io.EOF
		}
		event, err := s.sse.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return core.Event{}, core.Upstream(502, "truncated_stream", "Yuanbao stream ended without a finish marker")
			}
			return core.Event{}, err
		}
		if event.Event == "error" {
			return core.Event{}, core.Upstream(502, "upstream_stream_error", "Yuanbao stream reported an error")
		}
		data := strings.TrimSpace(event.Data)
		if data == "" {
			continue
		}
		if data == "[DONE]" || data == "[done]" || event.Event == "done" {
			if !s.seenAnswer {
				return core.Event{}, core.Upstream(502, "empty_completion", "Yuanbao returned no answer")
			}
			s.done = true
			return core.Event{Kind: core.EventFinish, FinishReason: "stop"}, nil
		}
		// Yuanbao emits speech mode and bracketed metadata alongside JSON.
		// These frames are control information, not assistant content.
		if event.Event == "speech_type" {
			continue
		}
		if strings.HasSuffix(data, "]") && (strings.HasPrefix(data, "[plugin:") || strings.HasPrefix(data, "[MSGINDEX:") || strings.HasPrefix(data, "[TRACEID:")) {
			continue
		}
		// The service sends textual heartbeat events alongside JSON chunks.
		if data == "ping" || data == "heartbeat" {
			continue
		}
		events, err := s.decode([]byte(data))
		if err != nil {
			return core.Event{}, err
		}
		s.queue = events
	}
}
func (s *completionStream) decode(data []byte) ([]core.Event, error) {
	var c struct {
		Type     string          `json:"type"`
		Msg      string          `json:"msg"`
		Content  string          `json:"content"`
		Error    json.RawMessage `json:"error"`
		Code     *int            `json:"code"`
		Contents []struct {
			Msg string `json:"msg"`
		} `json:"contents"`
		Docs []struct {
			URL string `json:"url"`
		} `json:"docs"`
		Outline  []string `json:"outlineList"`
		Divider  string   `json:"dividerText"`
		Entities []struct {
			Name string `json:"name"`
			Desc string `json:"desc"`
		} `json:"entityList"`
	}
	if json.Unmarshal(data, &c) != nil {
		return nil, core.Upstream(502, "invalid_stream", "Yuanbao stream returned invalid JSON")
	}
	if c.Type == "error" || (len(c.Error) > 0 && string(c.Error) != "null") || (c.Code != nil && *c.Code != 0) {
		return nil, core.Upstream(502, "upstream_stream_error", "Yuanbao stream reported an error")
	}
	text := ""
	kind := core.EventContent
	switch c.Type {
	case "text":
		text = cleanMarkers(c.Msg)
	case "think":
		text = c.Content
		kind = core.EventReasoning
	case "deepSearch":
		if len(c.Contents) > 0 {
			text = c.Contents[0].Msg
		}
		kind = core.EventReasoning
	case "searchGuid":
		if s.seen == nil {
			s.seen = map[string]bool{}
		}
		var citations []json.RawMessage
		for _, doc := range c.Docs {
			if doc.URL != "" && !s.seen[doc.URL] {
				s.seen[doc.URL] = true
				b, _ := json.Marshal(doc.URL)
				citations = append(citations, b)
			}
		}
		if len(citations) > 0 {
			return []core.Event{{Kind: core.EventCitations, Citations: citations}}, nil
		}
	case "outline":
		text = "# 研究大纲\n- " + strings.Join(c.Outline, "\n- ")
	case "dividerLine":
		text = "\n# " + c.Divider + "\n"
	case "relevantEntities":
		escape := func(v string) string {
			return strings.NewReplacer("|", "\\|", "\r", "", "\n", "<br>").Replace(cleanMarkers(v))
		}
		var b strings.Builder
		b.WriteString("\n# 相关组织及人物\n| name | desc |\n| --- | --- |\n")
		for _, entity := range c.Entities {
			b.WriteString("| " + escape(entity.Name) + " | " + escape(entity.Desc) + " |\n")
		}
		text = b.String()
	case "done", "finish":
		s.done = true
		return []core.Event{{Kind: core.EventFinish, FinishReason: "stop"}}, nil
	case "meta", "step", "components", "mindmap", "timeline", "relevantEvents", "card":
		// Non-text metadata has no OpenAI content equivalent.
	default:
		// Unknown presentation chunks are ignored; an explicit finish is still required.
	}
	if text != "" {
		return []core.Event{{Kind: kind, Text: text}}, nil
	}
	return nil, nil
}
