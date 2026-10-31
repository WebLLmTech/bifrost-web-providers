package core

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"time"
)

type HTTPClient struct {
	Client       *http.Client
	MaxJSONBytes int64
}

func NewHTTPClient() *HTTPClient {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 45 * time.Second
	return &HTTPClient{Client: &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, MaxJSONBytes: 4 << 20}
}
func ReadBounded(r io.Reader, max int64) ([]byte, error) {
	if max < 1 {
		return nil, Invalid("positive body limit required")
	}
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, Upstream(502, "body_too_large", "Upstream body exceeds configured limit")
	}
	return b, nil
}
func (c *HTTPClient) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, Invalid("upstream request is nil")
	}
	return c.Client.Do(req.Clone(ctx))
}
func (c *HTTPClient) JSON(ctx context.Context, req *http.Request, out any) error {
	r, err := c.Do(ctx, req)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return ErrorFromStatus(req.URL.Host, r.StatusCode)
	}
	max := c.MaxJSONBytes
	if max < 1 {
		max = 4 << 20
	}
	b, err := ReadBounded(r.Body, max)
	if err != nil {
		return err
	}
	if json.Unmarshal(b, out) != nil {
		return Upstream(502, "invalid_json", "Upstream returned invalid JSON")
	}
	return nil
}
func (c *HTTPClient) SSE(ctx context.Context, req *http.Request) (*SSEReader, error) {
	r, err := c.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		r.Body.Close()
		return nil, ErrorFromStatus(req.URL.Host, r.StatusCode)
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct != "text/event-stream" {
		r.Body.Close()
		return nil, Upstream(502, "invalid_content_type", "Upstream did not return text/event-stream")
	}
	return NewSSEReader(ctx, r.Body, 4<<20), nil
}
func (c *HTTPClient) Close() { c.Client.CloseIdleConnections() }
