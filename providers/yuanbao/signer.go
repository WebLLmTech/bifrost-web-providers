package yuanbao

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/WebLLmTech/bifrost-web-providers/core"
	"github.com/gorilla/websocket"
)

// JS resolves and calls the web client's actual Qimei SDK. No signature is
// synthesized in Go. These expressions are ported from yuanbao-deno-api/signer.ts.
//
//go:embed resolve.js
var resolveJS string

//go:embed ready.js
var readyJS string

//go:embed sign.js
var signJS string

type signerEntry struct {
	signer *uskeySigner
	users  int
	used   time.Time
}

func (p *Provider) acquireSigner(id identity) (*uskeySigner, func(), error) {
	raw, _ := json.Marshal(id)
	key := sha256.Sum256(raw)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, nil, context.Canceled
	}
	entry := p.signers[key]
	var evicted *uskeySigner
	if entry == nil {
		if len(p.signers) >= p.opts.MaxSignerSessions {
			var oldestKey [32]byte
			var oldest *signerEntry
			for k, e := range p.signers {
				if e.users == 0 && (oldest == nil || e.used.Before(oldest.used)) {
					oldestKey = k
					oldest = e
				}
			}
			if oldest == nil {
				p.mu.Unlock()
				return nil, nil, core.Upstream(429, "signer_capacity", "Yuanbao signer sessions are busy")
			}
			delete(p.signers, oldestKey)
			evicted = oldest.signer
		}
		entry = &signerEntry{signer: &uskeySigner{provider: p, id: id, gate: make(chan struct{}, 1)}}
		p.signers[key] = entry
	}
	entry.users++
	p.mu.Unlock()
	if evicted != nil {
		evicted.close()
	}
	var once sync.Once
	return entry.signer, func() { once.Do(func() { p.mu.Lock(); entry.users--; entry.used = time.Now(); p.mu.Unlock() }) }, nil
}

type cdpSession struct {
	conn   *websocket.Conn
	target string
	nextID int
}
type uskeySigner struct {
	provider *Provider
	id       identity
	gate     chan struct{}
	mu       sync.Mutex
	session  *cdpSession
}

func (s *uskeySigner) expressions() (string, string) {
	quote := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	resolved := strings.NewReplacer("__APP_KEY__", quote(s.provider.opts.QimeiAppKey), "__MODULE_IDS__", quote(s.provider.opts.QimeiModuleIDs)).Replace(resolveJS)
	ready := strings.ReplaceAll(readyJS, "__RESOLVE__", resolved)
	sign := strings.NewReplacer("__RESOLVE__", resolved, "__APP_ID__", quote(s.provider.opts.QimeiAppID)).Replace(signJS)
	return ready, sign
}
func (s *uskeySigner) sign(parent context.Context) (http.Header, error) {
	ctx, cancel := context.WithTimeout(parent, time.Duration(s.provider.opts.SignerTimeoutSeconds)*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.provider.base, cancel)
	defer stop()
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-s.gate }()
	ready, expression := s.expressions()
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s.mu.Lock()
		session := s.session
		s.mu.Unlock()
		var err error
		if session == nil {
			session, err = s.createSession(ctx, ready)
		}
		if err == nil {
			var raw struct {
				Error     string `json:"error"`
				H38       string `json:"h38"`
				Timestamp int64  `json:"ts"`
				Uskey     string `json:"uskey"`
				DeviceID  string `json:"deviceId"`
			}
			err = session.evaluate(ctx, expression, &raw)
			if err == nil && raw.Error == "" && raw.Uskey != "" && len(raw.H38) == 38 && raw.Timestamp > 0 {
				// encodeURIComponent differs from QueryEscape for space and !'()*.
				escaped := url.QueryEscape(raw.Uskey)
				escaped = strings.NewReplacer("+", "%20", "%21", "!", "%27", "'", "%28", "(", "%29", ")", "%2A", "*").Replace(escaped)
				h := http.Header{}
				h.Set("X-Uskey", escaped)
				h.Set("X-HY92", raw.H38)
				b, _ := json.Marshal(raw.Timestamp)
				h.Set("X-Timestamp", string(b))
				if raw.DeviceID != "" {
					h.Set("X-Device-Id", raw.DeviceID)
					h.Set("X-HY93", raw.DeviceID)
				}
				return h, nil
			}
		}
		s.close()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, core.Upstream(502, "signing_failed", "Yuanbao browser signing failed")
}
func (s *uskeySigner) browserURL(path string) *url.URL {
	u, _ := url.Parse(s.provider.opts.BrowserlessWS)
	if u.Scheme == "wss" {
		u.Scheme = "https"
	} else {
		u.Scheme = "http"
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	return u
}
func (s *uskeySigner) createSession(ctx context.Context, ready string) (*cdpSession, error) {
	u := s.browserURL("/json/new")
	// Chrome's /json/new accepts a bare encoded URL; preserve Browserless auth.
	if u.RawQuery != "" {
		u.RawQuery += "&"
	}
	u.RawQuery += url.QueryEscape("about:blank")
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), nil)
	if err != nil {
		return nil, err
	}
	var target struct {
		ID string `json:"id"`
		WS string `json:"webSocketDebuggerUrl"`
	}
	if err = s.provider.http.JSON(ctx, req, &target); err != nil {
		return nil, errors.New("Browserless target creation failed")
	}
	if target.ID == "" || strings.ContainsAny(target.ID, "/?#\r\n") {
		return nil, errors.New("invalid Browserless target")
	}
	session := &cdpSession{target: target.ID}
	// Even invalid returned WebSocket URLs must not leak the created target.
	defer func() {
		if session.conn == nil {
			s.closeTarget(target.ID)
		}
	}()
	remote, err := url.Parse(target.WS)
	if err != nil || (remote.Scheme != "ws" && remote.Scheme != "wss") || remote.Host == "" || remote.User != nil {
		return nil, errors.New("invalid Browserless WebSocket")
	}
	base, _ := url.Parse(s.provider.opts.BrowserlessWS)
	remote.Host = base.Host
	remote.Scheme = base.Scheme
	query := remote.Query()
	for k, values := range base.Query() {
		query[k] = values
	}
	remote.RawQuery = query.Encode()
	conn, resp, err := (&websocket.Dialer{HandshakeTimeout: 15 * time.Second}).DialContext(ctx, remote.String(), nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return nil, errors.New("Browserless WebSocket connection failed")
	}
	conn.SetReadLimit(4 << 20)
	session.conn = conn
	s.mu.Lock()
	if s.provider.base.Err() != nil || ctx.Err() != nil {
		s.mu.Unlock()
		conn.Close()
		session.conn = nil
		return nil, context.Canceled
	}
	s.session = session
	s.mu.Unlock()
	fail := func(err error) (*cdpSession, error) { s.close(); return nil, err }
	cookies := []map[string]string{{"name": "hy_source", "value": "web", "domain": ".tencent.com", "path": "/"}, {"name": "hy_user", "value": s.id.User, "domain": ".tencent.com", "path": "/"}, {"name": "hy_token", "value": s.id.Token, "domain": ".tencent.com", "path": "/"}}
	commands := []struct {
		method string
		params any
	}{
		{"Network.enable", map[string]any{}}, {"Network.setCookies", map[string]any{"cookies": cookies}},
		{"Emulation.setUserAgentOverride", map[string]string{"userAgent": userAgent, "acceptLanguage": "zh-CN,zh;q=0.9", "platform": "Windows"}},
		{"Page.enable", map[string]any{}}, {"Page.navigate", map[string]string{"url": "https://yuanbao.tencent.com/chat/" + url.PathEscape(s.id.AgentID)}},
	}
	for _, cmd := range commands {
		if err = session.call(ctx, cmd.method, cmd.params, nil); err != nil {
			return fail(err)
		}
	}
	for {
		var ok bool
		if err = session.evaluate(ctx, ready, &ok); err != nil {
			return fail(err)
		}
		if ok {
			return session, nil
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return fail(ctx.Err())
		}
	}
}

// A signer serializes RPCs through gate. Closing the socket on cancellation
// interrupts both reads and writes, including commands with no response.
func (s *cdpSession) call(parent context.Context, method string, params, out any) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { s.conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	s.conn.SetWriteDeadline(deadline)
	s.conn.SetReadDeadline(deadline)
	s.nextID++
	id := s.nextID
	if err := s.conn.WriteJSON(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return err
	}
	for {
		var result struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := s.conn.ReadJSON(&result); err != nil {
			return err
		}
		if result.ID != id {
			continue
		}
		if len(result.Error) > 0 && string(result.Error) != "null" {
			return errors.New("CDP command failed")
		}
		if out != nil && json.Unmarshal(result.Result, out) != nil {
			return errors.New("invalid CDP result")
		}
		return nil
	}
}
func (s *cdpSession) evaluate(ctx context.Context, expression string, out any) error {
	var result struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception json.RawMessage `json:"exceptionDetails"`
	}
	if err := s.call(ctx, "Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true, "awaitPromise": true}, &result); err != nil {
		return err
	}
	if len(result.Exception) > 0 && string(result.Exception) != "null" {
		return errors.New("browser expression failed")
	}
	if json.Unmarshal(result.Result.Value, out) != nil {
		return errors.New("invalid browser expression result")
	}
	return nil
}
func (s *uskeySigner) closeTarget(target string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	u := s.browserURL("/json/close/" + url.PathEscape(target))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return
	}
	resp, err := s.provider.http.Do(ctx, req)
	if err == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
	}
}
func (s *uskeySigner) close() {
	s.mu.Lock()
	session := s.session
	s.session = nil
	s.mu.Unlock()
	if session != nil {
		session.conn.Close()
		s.closeTarget(session.target)
	}
}
