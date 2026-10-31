package deepseek

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/WebLLmTech/bifrost-web-providers/core"
)

// decoder ports chunk-transformer.ts: DeepSeek streams JSON-patch fragments
// instead of OpenAI deltas. Fragments carry a type (THINK, RESPONSE,
// TOOL_SEARCH, TOOL_OPEN, ...); patches address them by path, with negative
// indices counting from the end. Tool-call text blocks inside RESPONSE
// fragments stay verbatim: core.NormalizeStream parses them via the delimited
// ToolProtocol configured by the provider.
type decoder struct {
	fragments   []*fragment
	currentType string
	displayed   map[string]bool
	citationURL map[string]bool
	finished    bool
}

func newDecoder() *decoder {
	return &decoder{displayed: map[string]bool{}, citationURL: map[string]bool{}}
}

type searchResult struct {
	URL      string `json:"url"`
	Title    string `json:"title"`
	SiteName string `json:"site_name"`
}

type fragmentQuery struct {
	Query string `json:"query"`
}

type fragment struct {
	ID      int            `json:"id"`
	Type    string         `json:"type"`
	Content *string        `json:"content"`
	Status  string         `json:"status"`
	Queries []fragmentQuery `json:"queries"`
	Results []searchResult `json:"results"`
	Result  *searchResult  `json:"result"`
}

type patchChunk struct {
	O string          `json:"o"`
	P *string         `json:"p"`
	V json.RawMessage `json:"v"`
}

var (
	citationRefRE  = regexp.MustCompile(`\[citation:([0-9]+)\]`)
	referenceRefRE = regexp.MustCompile(`\[reference:([0-9]+)\]`)
	fragmentPathRE = regexp.MustCompile(`^response/fragments/(-?\d+)`)
)

func rawKind(raw json.RawMessage) byte {
	trimmed := strings.TrimLeft(string(raw), " \t\r\n")
	if trimmed == "" {
		return 0
	}
	return trimmed[0]
}

func rawString(raw json.RawMessage) (string, bool) {
	if rawKind(raw) != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// handleData decodes one SSE data payload into zero or more events. Malformed
// JSON is ignored like the old handleMalformedEvent default.
func (d *decoder) handleData(data string) []core.Event {
	var out []core.Event
	emit := func(events ...core.Event) { out = append(out, events...) }

	var chunk patchChunk
	if json.Unmarshal([]byte(data), &chunk) != nil {
		return nil
	}

	// Init chunk: {v: {response: {fragments: [...]}}}; JS checked "response" in v.
	if rawKind(chunk.V) == '{' {
		var probe map[string]json.RawMessage
		if json.Unmarshal(chunk.V, &probe) == nil {
			if raw, ok := probe["response"]; ok {
				var resp struct {
					Fragments []fragment `json:"fragments"`
				}
				if rawKind(raw) == '{' {
					_ = json.Unmarshal(raw, &resp)
				}
				d.handleFragments(resp.Fragments, emit)
				return out
			}
		}
	}

	if rawKind(chunk.V) == '[' {
		if chunk.P != nil && strings.HasSuffix(*chunk.P, "/results") {
			d.handlePatch(*chunk.P, chunk.V, emit)
			return out
		}
		if chunk.P != nil && (*chunk.P == "response/fragments" || *chunk.P == "fragments") {
			var fragments []fragment
			if json.Unmarshal(chunk.V, &fragments) == nil {
				d.handleFragments(fragments, emit)
			}
			return out
		}
		var patches []patchChunk
		if json.Unmarshal(chunk.V, &patches) == nil {
			base := "response/fragments/-1"
			if chunk.P != nil {
				base = *chunk.P
			}
			d.handlePatchList(patches, base, emit)
		}
		return out
	}

	if chunk.P != nil {
		d.handlePatch(*chunk.P, chunk.V, emit)
		return out
	}

	if s, ok := rawString(chunk.V); ok && d.currentType != "" {
		d.writeFragmentContent(s, emit)
	}
	return out
}

func (d *decoder) handlePatchList(patches []patchChunk, basePath string, emit func(...core.Event)) {
	for _, patch := range patches {
		path := resolvePatchPath(basePath, patch.P)
		if patch.O == "BATCH" && rawKind(patch.V) == '[' {
			var nested []patchChunk
			if json.Unmarshal(patch.V, &nested) == nil {
				d.handlePatchList(nested, path, emit)
				continue
			}
		}
		d.handlePatch(path, patch.V, emit)
	}
}

func (d *decoder) handlePatch(path string, value json.RawMessage, emit func(...core.Event)) {
	if strings.HasSuffix(path, "/results") && rawKind(value) == '[' {
		var results []searchResult
		if json.Unmarshal(value, &results) != nil {
			return
		}
		if frag := d.fragmentByPath(path); frag != nil {
			frag.Results = results
			d.writeToolResults(frag, results, emit)
		}
		d.addCitations(results, emit)
		return
	}

	if path == "response/quasi_status" {
		if s, ok := rawString(value); ok && s == "FINISHED" {
			d.finished = true
			emit(core.Event{Kind: core.EventFinish, FinishReason: "stop"})
		}
		return
	}

	if (path == "response/fragments" || path == "fragments") && rawKind(value) == '[' {
		var fragments []fragment
		if json.Unmarshal(value, &fragments) == nil {
			d.handleFragments(fragments, emit)
		}
		return
	}

	if strings.HasSuffix(path, "/status") {
		if s, ok := rawString(value); ok {
			if frag := d.fragmentByPath(path); frag != nil {
				frag.Status = s
			}
		}
		return
	}

	if strings.HasSuffix(path, "/content") {
		if s, ok := rawString(value); ok {
			d.writeFragmentContent(s, emit)
		}
	}
}

func (d *decoder) handleFragments(fragments []fragment, emit func(...core.Event)) {
	d.writeToolOpenBatch(fragments, emit)
	for i := range fragments {
		frag := fragments[i]
		d.currentType = frag.Type
		stored := frag
		d.fragments = append(d.fragments, &stored)
		d.writeToolStart(&stored, emit)
		if len(stored.Results) > 0 {
			d.writeToolResults(&stored, stored.Results, emit)
			d.addCitations(stored.Results, emit)
		}
		if stored.Result != nil && stored.Result.URL != "" {
			d.addCitations([]searchResult{*stored.Result}, emit)
		}
		if stored.Content != nil {
			d.writeFragmentContent(*stored.Content, emit)
		}
	}
}

func (d *decoder) writeToolStart(frag *fragment, emit func(...core.Event)) {
	if frag.Type != "TOOL_SEARCH" || frag.Queries == nil {
		return
	}
	lines := make([]string, 0, len(frag.Queries)+2)
	lines = append(lines, "", fmt.Sprintf("🔍 搜索 %d 个关键词：", len(frag.Queries)))
	for _, q := range frag.Queries {
		lines = append(lines, "- "+q.Query)
	}
	d.writeToolEvent("start:"+strconv.Itoa(frag.ID), strings.Join(lines, "\n")+"\n", emit)
}

func (d *decoder) writeToolResults(frag *fragment, results []searchResult, emit func(...core.Event)) {
	if frag.Type != "TOOL_SEARCH" || len(results) == 0 {
		return
	}
	lines := []string{"", fmt.Sprintf("🔎 搜索到 %d 个网页：", len(results))}
	for _, r := range results {
		if r.Title == "" && r.URL == "" {
			continue
		}
		if len(lines) >= 10 { // header lines + at most 8 results
			break
		}
		title := r.Title
		if title == "" {
			title = r.URL
		}
		site := ""
		if r.SiteName != "" {
			site = " (" + r.SiteName + ")"
		}
		lines = append(lines, "- "+title+site)
	}
	d.writeToolEvent("results:"+strconv.Itoa(frag.ID), strings.Join(lines, "\n")+"\n", emit)
}

func (d *decoder) writeToolOpenBatch(fragments []fragment, emit func(...core.Event)) {
	var open []fragment
	for _, frag := range fragments {
		if frag.Type == "TOOL_OPEN" && frag.Result != nil && frag.Result.URL != "" {
			open = append(open, frag)
		}
	}
	if len(open) == 0 {
		return
	}
	ids := make([]string, len(open))
	for i, frag := range open {
		ids[i] = strconv.Itoa(frag.ID)
	}
	lines := []string{"", fmt.Sprintf("📄 浏览 %d 个页面：", len(open))}
	for i, frag := range open {
		if i >= 8 {
			break
		}
		title := frag.Result.Title
		if title == "" {
			title = frag.Result.URL
		}
		site := ""
		if frag.Result.SiteName != "" {
			site = " (" + frag.Result.SiteName + ")"
		}
		lines = append(lines, "- "+title+site)
	}
	d.writeToolEvent("open:"+strings.Join(ids, ","), strings.Join(lines, "\n")+"\n", emit)
}

func (d *decoder) writeToolEvent(key, content string, emit func(...core.Event)) {
	if d.displayed[key] {
		return
	}
	d.displayed[key] = true
	emit(core.Event{Kind: core.EventReasoning, Text: formatContent(content)})
}

func (d *decoder) writeFragmentContent(content string, emit func(...core.Event)) {
	formatted := formatContent(content)
	switch d.currentType {
	case "THINK":
		emit(core.Event{Kind: core.EventReasoning, Text: formatted})
	case "RESPONSE":
		emit(core.Event{Kind: core.EventContent, Text: formatted})
	}
}

// addCitations emits only URLs not seen before; the runtime handler renders
// the accumulated list.
func (d *decoder) addCitations(results []searchResult, emit func(...core.Event)) {
	var fresh []json.RawMessage
	for _, r := range results {
		if r.URL == "" || d.citationURL[r.URL] {
			continue
		}
		d.citationURL[r.URL] = true
		raw, err := json.Marshal(r.URL)
		if err != nil {
			continue
		}
		fresh = append(fresh, raw)
	}
	if len(fresh) > 0 {
		emit(core.Event{Kind: core.EventCitations, Citations: fresh})
	}
}

func (d *decoder) fragmentByPath(path string) *fragment {
	match := fragmentPathRE.FindStringSubmatch(path)
	if match == nil {
		return nil
	}
	index, err := strconv.Atoi(match[1])
	if err != nil {
		return nil
	}
	if index < 0 {
		at := len(d.fragments) + index
		if at < 0 || at >= len(d.fragments) {
			return nil
		}
		return d.fragments[at]
	}
	if index < len(d.fragments) {
		return d.fragments[index]
	}
	for _, frag := range d.fragments {
		if frag.ID == index {
			return frag
		}
	}
	return nil
}

func resolvePatchPath(basePath string, path *string) string {
	if path == nil || *path == "" {
		return basePath
	}
	if basePath == "" {
		return *path
	}
	if strings.HasPrefix(*path, "response/") {
		return *path
	}
	return basePath + "/" + *path
}

func formatContent(content string) string {
	content = citationRefRE.ReplaceAllString(content, "[$1]")
	return referenceRefRE.ReplaceAllString(content, "[$1]")
}
