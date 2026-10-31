package deepseek

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/WebLLmTech/bifrost-web-providers/core"
)

const (
	defaultBaseURL       = "https://chat.deepseek.com"
	completionPath       = "/api/v0/chat/completion"
	fileUploadPath       = "/api/v0/file/upload_file"
	powChallengePath     = "/api/v0/chat/create_pow_challenge"
	sessionCreatePath    = "/api/v0/chat_session/create"
	sessionDeletePath    = "/api/v0/chat_session/delete"
	usersCurrentPath     = "/api/v0/users/current"
	fileFetchPath        = "/api/v0/file/fetch_files"
	maxConcurrentUploads = 3
)

var rateLimitTextRE = regexp.MustCompile(`(?i)TOO_MANY_REQUESTS|RATE_LIMIT|too many requests`)

// apiClient ports api.ts: access-token refresh with a 3600s TTL, PoW
// challenge solving, chat session lifecycle, the patch-protocol completion
// stream, multipart upload and batched file-parse polling.
type apiClient struct {
	http   *core.HTTPClient
	files  *core.FileResolver
	opts   Options
	base   string
	origin string

	baseCtx context.Context
	tokens  *core.RefreshCache[string]
	uploads *core.AccountGate

	deviceID        string
	intercomDevice  string
	intercomSession string

	pollersMu sync.Mutex
	pollers   map[[32]byte]*pollerEntry
}

func newAPIClient(baseCtx context.Context, opts Options, httpClient *core.HTTPClient, files *core.FileResolver) *apiClient {
	u, _ := url.Parse(opts.BaseURL)
	c := &apiClient{
		http:            httpClient,
		files:           files,
		opts:            opts,
		base:            opts.BaseURL,
		origin:          u.Scheme + "://" + u.Host,
		baseCtx:         baseCtx,
		tokens:          core.NewRefreshCache[string](256, 30*time.Second),
		uploads:         core.NewAccountGate(opts.MaxConcurrentUploads),
		deviceID:        firstNonEmpty(opts.DeviceID, uuidHex()),
		intercomDevice:  firstNonEmpty(opts.IntercomDeviceID, uuidV4()),
		intercomSession: firstNonEmpty(opts.IntercomSession, randomIntercomSession()),
		pollers:         map[[32]byte]*pollerEntry{},
	}
	return c
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func randomIntercomSession() string {
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		panic("cryptographic random source unavailable")
	}
	return base64.StdEncoding.EncodeToString(b) + "--" + randomHex(20)
}

func (c *apiClient) close() {
	c.tokens.Close()
}

// generateHeaders reproduces the fixed browser header set of api.ts. The
// intercom/WAF cookie values are synthetic per-instance telemetry, never
// copied credentials. Accept-Encoding is left to Go's transparent gzip.
func (c *apiClient) generateHeaders(token string) http.Header {
	h := http.Header{}
	h.Set("Cookie", strings.Join([]string{
		"intercom-device-id-guh50jw4=" + c.intercomDevice,
		"intercom-session-guh50jw4=" + c.intercomSession,
		"HWWAFSESTIME=" + strconv.FormatInt(time.Now().UnixMilli(), 10),
		"HWWAFSESID=" + randomHex(9),
		"ds_session_id=" + uuidHex(),
	}, "; "))
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("Accept-Language", "zh-CN,zh;q=0.9,en-US;q=0.8,en;q=0.7")
	h.Set("Origin", c.origin)
	h.Set("Pragma", "no-cache")
	h.Set("Priority", "u=1, i")
	h.Set("Referer", c.origin+"/")
	h.Set("Sec-Ch-Ua", c.opts.SecChUa)
	h.Set("Sec-Ch-Ua-Mobile", "?0")
	h.Set("Sec-Ch-Ua-Platform", `"Windows"`)
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
	h.Set("User-Agent", c.opts.UserAgent)
	h.Set("X-App-Version", c.opts.AppVersion)
	h.Set("X-Client-Bundle-Id", "com.deepseek.chat")
	h.Set("X-Client-Locale", "zh_CN")
	h.Set("X-Client-Platform", "web")
	h.Set("x-client-timezone-offset", "28800")
	h.Set("X-Client-Version", c.opts.ClientVersion)
	h.Set("X-Device-Id", c.deviceID)
	h.Set("X-Device-Model", "")
	return h
}

func applyHeaders(req *http.Request, h http.Header) {
	for key, values := range h {
		for _, v := range values {
			req.Header.Add(key, v)
		}
	}
}

func (c *apiClient) invalidateOnAuth(refreshToken string, err error) error {
	var apiErr *core.APIError
	if errors.As(err, &apiErr) && (apiErr.Status == 401 || apiErr.Status == 403) {
		c.tokens.Invalidate(refreshToken)
	}
	return err
}

// acquireToken refreshes the access token via /users/current, cached per
// refresh token for token_ttl_seconds like the old accessTokenMap.
func (c *apiClient) acquireToken(ctx context.Context, refreshToken string) (string, error) {
	return c.tokens.Get(ctx, refreshToken, func(ctx context.Context) (string, time.Time, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+usersCurrentPath, nil)
		if err != nil {
			return "", time.Time{}, err
		}
		applyHeaders(req, c.generateHeaders(refreshToken))
		resp, err := c.http.Do(ctx, req)
		if err != nil {
			return "", time.Time{}, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", time.Time{}, core.ErrorFromStatus("DeepSeek token refresh", resp.StatusCode)
		}
		ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		body, err := core.ReadBounded(resp.Body, c.http.MaxJSONBytes)
		if err != nil {
			return "", time.Time{}, err
		}
		if ct == "text/html" {
			return "", time.Time{}, core.Upstream(502, "rejected", "rejected by server")
		}
		var env struct {
			Code *int            `json:"code"`
			Msg  string          `json:"msg"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(body, &env) != nil {
			return "", time.Time{}, core.Upstream(502, "invalid_json", "token refresh failed: invalid JSON")
		}
		if env.Code == nil || *env.Code != 0 {
			return "", time.Time{}, core.Unauthorized("DeepSeek rejected the refresh token")
		}
		token := ""
		if s, ok := jsonString(env.Data); ok {
			token = s
		} else {
			var obj struct {
				BizData *struct {
					Token string `json:"token"`
				} `json:"biz_data"`
			}
			if json.Unmarshal(env.Data, &obj) == nil && obj.BizData != nil {
				token = obj.BizData.Token
			}
		}
		if token == "" {
			return "", time.Time{}, core.Unauthorized("users/current returned no token")
		}
		return token, time.Now().Add(time.Duration(c.opts.TokenTTLSeconds) * time.Second), nil
	})
}

func jsonString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// readBizData enforces the envelope: HTTP ok, code == 0 and data.biz_code == 0
// before returning data.biz_data, matching api.ts readBizData.
func (c *apiClient) readBizData(resp *http.Response, action string, out any) error {
	defer resp.Body.Close()
	if resp.StatusCode == 429 {
		return &FileRateLimitError{}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return core.ErrorFromStatus("DeepSeek "+action, resp.StatusCode)
	}
	body, err := core.ReadBounded(resp.Body, c.http.MaxJSONBytes)
	if err != nil {
		return err
	}
	var env struct {
		Code *int   `json:"code"`
		Msg  string `json:"msg"`
		Data *struct {
			BizCode *int            `json:"biz_code"`
			BizMsg  string          `json:"biz_msg"`
			BizData json.RawMessage `json:"biz_data"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &env) != nil {
		return core.Upstream(502, "invalid_json", "DeepSeek "+action+" returned invalid JSON")
	}
	codeOK := env.Code != nil && *env.Code == 0
	bizOK := env.Data != nil && env.Data.BizCode != nil && *env.Data.BizCode == 0
	if !codeOK || !bizOK {
		message := env.Msg
		if env.Data != nil {
			message += " " + env.Data.BizMsg
		}
		if rateLimitTextRE.MatchString(message) {
			return &FileRateLimitError{}
		}
		return core.Upstream(502, "upstream_rejected", "DeepSeek "+action+" rejected the request")
	}
	raw := env.Data.BizData
	if len(raw) == 0 || string(raw) == "null" || string(raw) == `""` {
		return core.Upstream(502, "invalid_data", "DeepSeek "+action+" returned incomplete data")
	}
	if out != nil && json.Unmarshal(raw, out) != nil {
		return core.Upstream(502, "invalid_data", "DeepSeek "+action+" returned incomplete data")
	}
	return nil
}

func (c *apiClient) postJSON(ctx context.Context, path, token string, payload []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	applyHeaders(req, c.generateHeaders(token))
	return c.http.Do(ctx, req)
}

func (c *apiClient) createConversation(ctx context.Context, refreshToken string) (string, error) {
	accessToken, err := c.acquireToken(ctx, refreshToken)
	if err != nil {
		return "", err
	}
	resp, err := c.postJSON(ctx, sessionCreatePath, accessToken, []byte(`{"character_id":null}`))
	if err != nil {
		return "", err
	}
	var out struct {
		ChatSession *struct {
			ID string `json:"id"`
		} `json:"chat_session"`
	}
	if err := c.readBizData(resp, "createConversation", &out); err != nil {
		return "", c.invalidateOnAuth(refreshToken, err)
	}
	if out.ChatSession == nil || out.ChatSession.ID == "" {
		return "", errors.New("createConversation failed: missing chat session id")
	}
	return out.ChatSession.ID, nil
}

// Cleanup uses the token already selected at Open, so closing the refresh
// cache cannot prevent deletion of an active session.
func (c *apiClient) deleteConversation(ctx context.Context, sessionID, accessToken string) error {
	body, _ := json.Marshal(map[string]string{"chat_session_id": sessionID})
	resp, err := c.postJSON(ctx, sessionDeletePath, accessToken, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return core.ErrorFromStatus("DeepSeek cleanup", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}

// solveChallengeFor fetches a PoW challenge for targetPath and returns the
// X-Ds-Pow-Response value. Failures are explicit; challenges are never
// skipped or answered from a cache.
func (c *apiClient) solveChallengeFor(ctx context.Context, refreshToken, targetPath string) (string, error) {
	accessToken, err := c.acquireToken(ctx, refreshToken)
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]string{"target_path": targetPath})
	resp, err := c.postJSON(ctx, powChallengePath, accessToken, body)
	if err != nil {
		return "", err
	}
	var out struct {
		Challenge *powChallenge `json:"challenge"`
	}
	if err := c.readBizData(resp, "get challenge response", &out); err != nil {
		return "", c.invalidateOnAuth(refreshToken, err)
	}
	if out.Challenge == nil {
		return "", errors.New("get challenge response failed")
	}
	return answerChallengeContext(ctx, *out.Challenge, targetPath, c.opts.PowMaxIterations)
}

type completionMessage struct {
	ChatSessionID   string   `json:"chat_session_id"`
	ParentMessageID *string  `json:"parent_message_id"`
	Prompt          string   `json:"prompt"`
	Preempt         bool     `json:"preempt"`
	RefFileIDs      []string `json:"ref_file_ids"`
	SearchEnabled   bool     `json:"search_enabled"`
	ThinkingEnabled bool     `json:"thinking_enabled"`
	ModelType       string   `json:"model_type"`
}

// completionStream opens the SSE completion. The content-type guard from
// createCompletion applies to the streaming path as well.
func (c *apiClient) completionStream(ctx context.Context, refreshToken string, message *completionMessage, powResponse string) (core.EventStream, error) {
	accessToken, err := c.acquireToken(ctx, refreshToken)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+completionPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyHeaders(req, c.generateHeaders(accessToken))
	req.Header.Set("X-Ds-Pow-Response", powResponse)
	req.Header.Set("Accept", "text/event-stream,application/json")
	resp, err := c.http.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || ct != "text/event-stream" {
		resp.Body.Close()
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			c.tokens.Invalidate(refreshToken)
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil, core.Upstream(502, "invalid_content_type", "DeepSeek did not return text/event-stream")
		}
		return nil, core.ErrorFromStatus("DeepSeek completion", resp.StatusCode)
	}
	return &completionStream{sse: core.NewSSEReader(ctx, resp.Body, 4<<20), dec: newDecoder()}, nil
}

type completionStream struct {
	sse   *core.SSEReader
	dec   *decoder
	queue []core.Event
	done  bool
}

func (s *completionStream) Next(ctx context.Context) (core.Event, error) {
	for {
		if len(s.queue) > 0 {
			e := s.queue[0]
			s.queue = s.queue[1:]
			return e, nil
		}
		if s.done {
			return core.Event{}, io.EOF
		}
		sseEvent, err := s.sse.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				if s.dec.finished {
					s.done = true
					continue
				}
				return core.Event{}, core.Upstream(502, "truncated_stream", "deepseek completion stream ended without finish")
			}
			return core.Event{}, err
		}
		if sseEvent.Event == "error" {
			return core.Event{}, core.Upstream(502, "upstream_stream_error", "DeepSeek stream reported an error")
		}
		data := sseEvent.Data
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			if !s.dec.finished {
				s.dec.finished = true
				s.queue = append(s.queue, core.Event{Kind: core.EventFinish, FinishReason: "stop"})
			}
			s.done = true
			continue
		}
		var envelope map[string]json.RawMessage
		if json.Unmarshal([]byte(data), &envelope) != nil {
			return core.Event{}, core.Upstream(502, "invalid_stream", "DeepSeek stream returned invalid JSON")
		}
		if v, ok := envelope["error"]; ok && string(v) != "null" {
			return core.Event{}, core.Upstream(502, "upstream_stream_error", "DeepSeek stream reported an error")
		}
		if v, ok := envelope["code"]; ok && string(v) != "0" {
			return core.Event{}, core.Upstream(502, "upstream_stream_error", "DeepSeek stream reported an error")
		}
		s.queue = append(s.queue, s.dec.handleData(data)...)
		if s.dec.finished {
			s.done = true
		}
	}
}

func (s *completionStream) Close() error { return s.sse.Close() }

// uploadFile ports api.ts uploadFile: the concurrency gate covers resolve,
// challenge and POST; parse polling happens outside the gate.
func (c *apiClient) uploadFile(ctx context.Context, refreshToken string, ref core.FileRef, modelType string, thinking bool) (att attachment, err error) {
	defer func() { err = c.invalidateOnAuth(refreshToken, err) }()
	release, err := c.uploads.Acquire(ctx, refreshToken)
	if err != nil {
		return attachment{}, err
	}
	var accessToken string
	func() {
		defer release()
		var input core.FileInput
		if input, err = c.files.Resolve(ctx, ref); err != nil {
			return
		}
		if accessToken, err = c.acquireToken(ctx, refreshToken); err != nil {
			return
		}
		var powResponse string
		if powResponse, err = c.solveChallengeFor(ctx, refreshToken, fileUploadPath); err != nil {
			return
		}
		att, err = c.postUpload(ctx, accessToken, input, powResponse, modelType, thinking)
	}()
	if err != nil {
		return attachment{}, err
	}
	poller, releasePoller := c.acquireFilePoller(accessToken)
	defer releasePoller()
	return poller.WaitFor(ctx, att)
}

func (c *apiClient) postUpload(ctx context.Context, accessToken string, input core.FileInput, powResponse, modelType string, thinking bool) (attachment, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	contentType := input.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if strings.ContainsAny(input.Filename+contentType, "\r\n") {
		return attachment{}, core.Invalid("invalid attachment metadata")
	}
	partHeader := textproto.MIMEHeader{}
	partHeader.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": input.Filename}))
	partHeader.Set("Content-Type", contentType)
	part, err := writer.CreatePart(partHeader)
	if err != nil {
		return attachment{}, err
	}
	if _, err := part.Write(input.Bytes); err != nil {
		return attachment{}, err
	}
	if err := writer.Close(); err != nil {
		return attachment{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+fileUploadPath, &body)
	if err != nil {
		return attachment{}, err
	}
	headers := c.generateHeaders(accessToken)
	headers.Del("Content-Type")
	headers.Set("Content-Type", writer.FormDataContentType())
	headers.Set("Accept", "*/*")
	headers.Set("X-Ds-Pow-Response", powResponse)
	headers.Set("X-File-Size", strconv.Itoa(len(input.Bytes)))
	headers.Set("X-Model-Type", modelType)
	thinkingValue := "0"
	if thinking {
		thinkingValue = "1"
	}
	headers.Set("X-Thinking-Enabled", thinkingValue)
	applyHeaders(req, headers)

	resp, err := c.http.Do(ctx, req)
	if err != nil {
		return attachment{}, err
	}
	var file attachment
	if err := c.readBizData(resp, "upload file", &file); err != nil {
		return attachment{}, err
	}
	if file.ID == "" {
		return attachment{}, errors.New("upload file failed: missing file id")
	}
	return file, nil
}

type pollerEntry struct {
	poller *FileParsePoller
	users  int
}

func (c *apiClient) acquireFilePoller(accessToken string) (*FileParsePoller, func()) {
	key := sha256.Sum256([]byte(accessToken))
	c.pollersMu.Lock()
	entry := c.pollers[key]
	if entry == nil {
		entry = &pollerEntry{poller: NewFileParsePoller(c.baseCtx,
			func(ctx context.Context, ids []string) ([]attachment, error) {
				return c.fetchFiles(ctx, ids, accessToken)
			},
			time.Duration(c.opts.FilePollIntervalMS)*time.Millisecond,
			time.Duration(c.opts.FileParseTimeoutMS)*time.Millisecond,
			time.Duration(c.opts.FilePollMaxBackoffMS)*time.Millisecond, nil)}
		c.pollers[key] = entry
	}
	entry.users++
	c.pollersMu.Unlock()
	var once sync.Once
	return entry.poller, func() {
		once.Do(func() {
			c.pollersMu.Lock()
			entry.users--
			if entry.users == 0 {
				delete(c.pollers, key)
			}
			c.pollersMu.Unlock()
		})
	}
}

// fetchFiles ports fetchFilesInfo: repeated file_ids query parameters, a
// 429 (or rate-limit error text) becoming FileRateLimitError so the poller
// backs off instead of failing waiters.
func (c *apiClient) fetchFiles(ctx context.Context, ids []string, accessToken string) ([]attachment, error) {
	query := url.Values{}
	for _, id := range ids {
		query.Add("file_ids", id)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+fileFetchPath+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	applyHeaders(req, c.generateHeaders(accessToken))
	resp, err := c.http.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
	if resp.StatusCode == http.StatusTooManyRequests {
		resp.Body.Close()
		return nil, &FileRateLimitError{RetryAfter: retryAfter}
	}
	var out struct {
		Files *[]attachment `json:"files"`
	}
	if err := c.readBizData(resp, "fetch uploaded files", &out); err != nil {
		if rateLimitTextRE.MatchString(err.Error()) {
			return nil, &FileRateLimitError{RetryAfter: retryAfter}
		}
		return nil, err
	}
	if out.Files == nil {
		return nil, errors.New("fetch uploaded files failed: missing files")
	}
	return *out.Files, nil
}

// parseRetryAfter mirrors retryAfterMs: numeric seconds first, then HTTP-date,
// otherwise zero.
func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && !math.IsInf(seconds, 0) && !math.IsNaN(seconds) {
		if seconds < 0 {
			seconds = 0
		}
		return time.Duration(seconds * float64(time.Second))
	}
	if date, err := http.ParseTime(value); err == nil {
		if wait := time.Until(date); wait > 0 {
			return wait
		}
	}
	return 0
}
