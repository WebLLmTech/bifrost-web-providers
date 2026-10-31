package deepseek

import (
	"strings"
	"testing"

	"github.com/WebLLmTech/bifrost-web-providers/core"
)

func eventsText(events []core.Event) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString(string(e.Kind) + ":" + e.Text + e.FinishReason + "\n")
	}
	return b.String()
}

func TestDecoderInitFragmentsAndStreamingContent(t *testing.T) {
	d := newDecoder()

	// Init chunk: a v-object containing "response".
	out := d.handleData(`{"o":"REPLACE","v":{"response":{"fragments":[{"id":0,"type":"RESPONSE"}]}}}`)
	if len(out) != 0 {
		t.Fatalf("init without content must not emit, got %s", eventsText(out))
	}

	// Bare string v streams into the current fragment type.
	out = d.handleData(`{"o":"ADD","v":"Hello"}`)
	if len(out) != 1 || out[0].Kind != core.EventContent || out[0].Text != "Hello" {
		t.Fatalf("unexpected events: %s", eventsText(out))
	}

	// Path-addressed content patch with negative index (last fragment).
	out = d.handleData(`{"o":"REPLACE","p":"response/fragments/-1/content","v":" world[citation:3][reference:12]"}`)
	if len(out) != 1 || out[0].Kind != core.EventContent || out[0].Text != " world[3][12]" {
		t.Fatalf("unexpected events: %s", eventsText(out))
	}

	// quasi_status FINISHED ends the stream with finish reason stop.
	out = d.handleData(`{"o":"REPLACE","p":"response/quasi_status","v":"FINISHED"}`)
	if len(out) != 1 || out[0].Kind != core.EventFinish || out[0].FinishReason != "stop" {
		t.Fatalf("unexpected events: %s", eventsText(out))
	}
	if !d.finished {
		t.Fatal("decoder must mark finished")
	}
}

func TestDecoderThinkFragmentIsReasoning(t *testing.T) {
	d := newDecoder()
	out := d.handleData(`{"v":{"response":{"fragments":[{"id":0,"type":"THINK","content":"Let me think"}]}}}`)
	if len(out) != 1 || out[0].Kind != core.EventReasoning || out[0].Text != "Let me think" {
		t.Fatalf("unexpected events: %s", eventsText(out))
	}
	out = d.handleData(`{"o":"ADD","v":"more"}`)
	if len(out) != 1 || out[0].Kind != core.EventReasoning || out[0].Text != "more" {
		t.Fatalf("THINK must stay the current type: %s", eventsText(out))
	}
}

func TestDecoderToolSearchStartResultsAndCitations(t *testing.T) {
	d := newDecoder()
	out := d.handleData(`{"v":{"response":{"fragments":[{"id":5,"type":"TOOL_SEARCH","queries":[{"query":"golang"},{"query":"race"}]}]}}}`)
	want := "\n🔍 搜索 2 个关键词：\n- golang\n- race\n"
	if len(out) != 1 || out[0].Kind != core.EventReasoning || out[0].Text != want {
		t.Fatalf("unexpected start events: %q", eventsText(out))
	}

	results := `[{"url":"https://a.example/1","title":"A","site_name":"Example"},{"url":"https://b.example/2","title":"B","site_name":""}]`
	out = d.handleData(`{"o":"REPLACE","p":"response/fragments/0/results","v":` + results + `}`)
	if len(out) != 2 {
		t.Fatalf("expected reasoning + citations, got %s", eventsText(out))
	}
	wantResults := "\n🔎 搜索到 2 个网页：\n- A (Example)\n- B\n"
	if out[0].Kind != core.EventReasoning || out[0].Text != wantResults {
		t.Fatalf("unexpected results reasoning: %q", out[0].Text)
	}
	if out[1].Kind != core.EventCitations || len(out[1].Citations) != 2 ||
		string(out[1].Citations[0]) != `"https://a.example/1"` ||
		string(out[1].Citations[1]) != `"https://b.example/2"` {
		t.Fatalf("unexpected citations: %v", out[1].Citations)
	}
	if frag := d.fragments[0]; len(frag.Results) != 2 {
		t.Fatalf("fragment results not stored: %v", frag.Results)
	}

	// Replaying the same results emits nothing: reasoning key and citation
	// URLs are both deduplicated.
	if out = d.handleData(`{"o":"REPLACE","p":"response/fragments/0/results","v":` + results + `}`); len(out) != 0 {
		t.Fatalf("duplicate results must not emit: %s", eventsText(out))
	}

	// A brand-new URL is emitted on its own.
	out = d.handleData(`{"o":"REPLACE","p":"response/fragments/0/results","v":[{"url":"https://c.example/3","title":"C"}]}`)
	if len(out) != 1 || out[0].Kind != core.EventCitations || len(out[0].Citations) != 1 ||
		string(out[0].Citations[0]) != `"https://c.example/3"` {
		t.Fatalf("unexpected fresh citation events: %s", eventsText(out))
	}
}

func TestDecoderToolOpenBatch(t *testing.T) {
	d := newDecoder()
	out := d.handleData(`{"v":{"response":{"fragments":[` +
		`{"id":7,"type":"TOOL_OPEN","result":{"url":"https://x.example","title":"X","site_name":"SX"}},` +
		`{"id":8,"type":"TOOL_OPEN","result":{"url":"https://y.example","title":"Y"}}]}}}`)
	want := "\n📄 浏览 2 个页面：\n- X (SX)\n- Y\n"
	if len(out) == 0 || out[0].Kind != core.EventReasoning || out[0].Text != want {
		t.Fatalf("unexpected open batch: %s", eventsText(out))
	}
	var cited []string
	for _, e := range out[1:] {
		if e.Kind != core.EventCitations {
			t.Fatalf("expected citations after the open batch, got %s", eventsText(out))
		}
		for _, c := range e.Citations {
			cited = append(cited, string(c))
		}
	}
	if strings.Join(cited, ",") != `"https://x.example","https://y.example"` {
		t.Fatalf("unexpected citation urls: %v", cited)
	}
}

func TestDecoderBatchPatchList(t *testing.T) {
	d := newDecoder()
	d.handleData(`{"v":{"response":{"fragments":[{"id":0,"type":"RESPONSE"}]}}}`)

	// Relative patch paths resolve against the batch base path.
	out := d.handleData(`{"o":"BATCH","p":"response/fragments/-1","v":[` +
		`{"o":"REPLACE","p":"content","v":"abc"},` +
		`{"o":"REPLACE","p":"status","v":"DONE"}]}`)
	if len(out) != 1 || out[0].Kind != core.EventContent || out[0].Text != "abc" {
		t.Fatalf("unexpected batch events: %s", eventsText(out))
	}
	if d.fragments[0].Status != "DONE" {
		t.Fatalf("status patch not applied: %q", d.fragments[0].Status)
	}

	// Nested BATCH with an absolute response/ path overrides the base.
	out = d.handleData(`{"o":"BATCH","v":[{"o":"BATCH","v":[` +
		`{"o":"REPLACE","p":"response/fragments/0/content","v":"deep"}]}]}`)
	if len(out) != 1 || out[0].Text != "deep" {
		t.Fatalf("unexpected nested batch events: %s", eventsText(out))
	}

	// Missing p on the outer batch defaults to response/fragments/-1.
	out = d.handleData(`{"o":"BATCH","v":[{"o":"REPLACE","p":"content","v":"tail"}]}`)
	if len(out) != 1 || out[0].Text != "tail" {
		t.Fatalf("unexpected default base events: %s", eventsText(out))
	}
}

func TestDecoderFragmentsArrayPatch(t *testing.T) {
	d := newDecoder()
	out := d.handleData(`{"o":"REPLACE","p":"response/fragments","v":[{"id":1,"type":"RESPONSE","content":"hi"}]}`)
	if len(out) != 1 || out[0].Kind != core.EventContent || out[0].Text != "hi" {
		t.Fatalf("unexpected events: %s", eventsText(out))
	}
	if len(d.fragments) != 1 {
		t.Fatalf("fragments not stored: %v", d.fragments)
	}
}

func TestDecoderFragmentLookupByIDAndBadPaths(t *testing.T) {
	d := newDecoder()
	d.handleData(`{"v":{"response":{"fragments":[{"id":42,"type":"RESPONSE"}]}}}`)

	// Index 42 is out of range, so the id fallback matches the fragment.
	d.handleData(`{"o":"REPLACE","p":"response/fragments/42/status","v":"DONE"}`)
	if d.fragments[0].Status != "DONE" {
		t.Fatalf("id lookup failed: %q", d.fragments[0].Status)
	}

	// Out-of-range negative index and non-fragment paths must be no-ops.
	d.handleData(`{"o":"REPLACE","p":"response/fragments/-5/status","v":"X"}`)
	d.handleData(`{"o":"REPLACE","p":"response/status","v":"X"}`)
	if d.fragments[0].Status != "DONE" {
		t.Fatalf("bad paths mutated state: %q", d.fragments[0].Status)
	}
}

func TestDecoderIgnoresMalformedAndStrayData(t *testing.T) {
	d := newDecoder()
	for _, data := range []string{
		`not json`,
		`{"o":"ADD"}`,             // no v, no p
		`{"o":"ADD","v":"stray"}`, // string v before any fragment type
		`{"o":"ADD","v":123}`,     // non-string scalar v
		`{"o":"REPLACE","p":"response/fragments/-1/content","v":7}`, // non-string content
	} {
		if out := d.handleData(data); len(out) != 0 {
			t.Fatalf("data %s must not emit: %s", data, eventsText(out))
		}
	}
}

func TestResolvePatchPath(t *testing.T) {
	p := func(s string) *string { return &s }
	cases := []struct {
		base string
		path *string
		want string
	}{
		{"response/fragments/-1", nil, "response/fragments/-1"},
		{"response/fragments/-1", p(""), "response/fragments/-1"},
		{"", p("response/x"), "response/x"},
		{"response/fragments/-1", p("content"), "response/fragments/-1/content"},
		{"response/fragments/-1", p("response/quasi_status"), "response/quasi_status"},
	}
	for _, c := range cases {
		if got := resolvePatchPath(c.base, c.path); got != c.want {
			t.Errorf("resolvePatchPath(%q, %v) = %q, want %q", c.base, c.path, got, c.want)
		}
	}
}
