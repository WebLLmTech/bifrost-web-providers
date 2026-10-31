package deepseek

import (
	"context"
	"errors"
	"fmt"
	"github.com/WebLLmTech/bifrost-web-providers/core"
	"sync"
	"time"
)

// attachment is the DeepSeek file record; only the fields used by the poller
// and the ref_file_ids list are decoded.
type attachment struct {
	ID        string  `json:"id"`
	Status    string  `json:"status"`
	FileName  string  `json:"file_name"`
	FileSize  int64   `json:"file_size"`
	ModelKind string  `json:"model_kind"`
	ErrorCode *string `json:"error_code"`
	IsImage   bool    `json:"is_image"`
}

var failedFileStatuses = map[string]bool{
	"CANCELLED":                 true,
	"CONTENT_EMPTY":             true,
	"CONTENT_FILTER":            true,
	"CONTENT_TOO_LONG":          true,
	"FAILED":                    true,
	"_CUSTOM_SYSTEM_ERROR_FAIL": true,
}

// FileRateLimitError maps file-parse-poller.ts FileRateLimitError; the poller
// backs off instead of failing waiters.
type FileRateLimitError struct {
	RetryAfter time.Duration
}

func (e *FileRateLimitError) Error() string { return "DeepSeek rate limited the request" }
func (e *FileRateLimitError) Unwrap() error {
	return core.Upstream(429, "rate_limit_exceeded", e.Error())
}

func fileError(f attachment) error {
	code := f.Status
	if f.ErrorCode != nil && *f.ErrorCode != "" {
		code = *f.ErrorCode
	}
	return fmt.Errorf("uploaded file parsing failed: %s", code)
}

type fileWaiter struct {
	deadline time.Time
	result   chan fileResult
}

type fileResult struct {
	file attachment
	err  error
}

// FileParsePoller batches fetch_files queries for every pending upload, like
// file-parse-poller.ts: one in-flight poll loop per access token, per-file
// deadlines, exponential backoff on rate limits. Wakeups only recompute the
// sleep window; they never trigger an early fetch, so concurrent WaitFor
// callers share one batched query like the old timer loop.
type FileParsePoller struct {
	base       context.Context
	fetchFiles func(context.Context, []string) ([]attachment, error)
	interval   time.Duration
	timeout    time.Duration
	maxBackoff time.Duration
	onIdle     func()

	mu          sync.Mutex
	pending     map[string][]*fileWaiter
	running     bool
	wake        chan struct{}
	fetchCancel context.CancelFunc
}

func NewFileParsePoller(
	base context.Context,
	fetchFiles func(context.Context, []string) ([]attachment, error),
	interval, timeout, maxBackoff time.Duration,
	onIdle func(),
) *FileParsePoller {
	if interval <= 0 {
		interval = 3 * time.Second
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	if maxBackoff <= 0 {
		maxBackoff = 30 * time.Second
	}
	if base == nil {
		base = context.Background()
	}
	return &FileParsePoller{
		base:       base,
		fetchFiles: fetchFiles,
		interval:   interval,
		timeout:    timeout,
		maxBackoff: maxBackoff,
		onIdle:     onIdle,
		pending:    map[string][]*fileWaiter{},
		wake:       make(chan struct{}, 1),
	}
}

// WaitFor resolves when the file leaves the parsing state. Already-terminal
// files settle immediately without touching the poll loop.
func (p *FileParsePoller) WaitFor(ctx context.Context, file attachment) (attachment, error) {
	if file.Status == "SUCCESS" {
		p.notifyIdle()
		return file, nil
	}
	if failedFileStatuses[file.Status] {
		p.notifyIdle()
		return attachment{}, fileError(file)
	}

	if err := ctx.Err(); err != nil {
		return attachment{}, err
	}
	if err := p.base.Err(); err != nil {
		return attachment{}, err
	}
	waiter := &fileWaiter{deadline: time.Now().Add(p.timeout), result: make(chan fileResult, 1)}
	p.mu.Lock()
	if err := p.base.Err(); err != nil {
		p.mu.Unlock()
		return attachment{}, err
	}
	p.pending[file.ID] = append(p.pending[file.ID], waiter)
	start := !p.running
	p.running = true
	p.mu.Unlock()
	if start {
		go p.poll()
	}

	select {
	case res := <-waiter.result:
		return res.file, res.err
	case <-ctx.Done():
		p.removeWaiter(file.ID, waiter)
		return attachment{}, ctx.Err()
	}
}

func (p *FileParsePoller) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *FileParsePoller) removeWaiter(id string, waiter *fileWaiter) {
	p.mu.Lock()
	list := p.pending[id]
	for i, w := range list {
		if w == waiter {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(p.pending, id)
	} else {
		p.pending[id] = list
	}
	if len(p.pending) == 0 && p.fetchCancel != nil {
		p.fetchCancel()
	}
	p.mu.Unlock()
	p.signal()
	p.notifyIdle()
}

func (p *FileParsePoller) notifyIdle() {
	p.mu.Lock()
	idle := !p.running && len(p.pending) == 0
	p.mu.Unlock()
	if idle && p.onIdle != nil {
		p.onIdle()
	}
}

// finish clears running under the same lock as the pending checks in poll, so
// a concurrent WaitFor either sees a live loop or starts a fresh one.
func (p *FileParsePoller) finish() {
	p.mu.Lock()
	p.running = false
	p.mu.Unlock()
	p.notifyIdle()
}

func (p *FileParsePoller) poll() {
	delay := p.interval
	ctx := p.base
	for {
		p.mu.Lock()
		if len(p.pending) == 0 {
			p.running = false
			p.mu.Unlock()
			p.notifyIdle()
			return
		}
		var nextDeadline time.Time
		first := true
		for _, waiters := range p.pending {
			for _, w := range waiters {
				if first || w.deadline.Before(nextDeadline) {
					nextDeadline = w.deadline
					first = false
				}
			}
		}
		p.mu.Unlock()

		wait := delay
		if until := time.Until(nextDeadline); until < wait {
			wait = until
		}
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-p.wake:
			// Recompute the window (waiters removed or deadlines changed);
			// never fetch early or the batch would split.
			timer.Stop()
			continue
		case <-ctx.Done():
			timer.Stop()
			p.settleAll(ctx.Err())
			p.finish()
			return
		}

		p.expireWaiters()
		p.mu.Lock()
		empty := len(p.pending) == 0
		ids := make([]string, 0, len(p.pending))
		for id := range p.pending {
			ids = append(ids, id)
		}
		if empty {
			p.running = false
		}
		p.mu.Unlock()
		if empty {
			p.notifyIdle()
			return
		}

		p.mu.Lock()
		deadline := time.Now().Add(p.timeout)
		for _, waiters := range p.pending {
			for _, w := range waiters {
				if w.deadline.Before(deadline) {
					deadline = w.deadline
				}
			}
		}
		fetchCtx, cancel := context.WithDeadline(ctx, deadline)
		p.fetchCancel = cancel
		if len(p.pending) == 0 {
			cancel()
		}
		p.mu.Unlock()
		files, err := p.fetchFiles(fetchCtx, ids)
		fetchExpired := fetchCtx.Err() != nil
		cancel()
		p.mu.Lock()
		p.fetchCancel = nil
		p.mu.Unlock()
		p.expireWaiters()
		if fetchExpired {
			if ctx.Err() != nil {
				p.settleAll(ctx.Err())
			}
			continue
		}
		if err != nil {
			var rateLimit *FileRateLimitError
			if errors.As(err, &rateLimit) {
				delay *= 2
				if delay > p.maxBackoff {
					delay = p.maxBackoff
				}
				if rateLimit.RetryAfter > delay {
					delay = rateLimit.RetryAfter
				}
				continue
			}
			p.settleAll(err)
			continue
		}
		for _, file := range files {
			switch {
			case file.Status == "SUCCESS":
				p.settle(file.ID, fileResult{file: file})
			case failedFileStatuses[file.Status]:
				p.settle(file.ID, fileResult{err: fileError(file)})
			}
		}
		delay = p.interval
	}
}

func (p *FileParsePoller) expireWaiters() {
	now := time.Now()
	p.mu.Lock()
	for id, waiters := range p.pending {
		active := waiters[:0]
		for _, w := range waiters {
			if w.deadline.After(now) {
				active = append(active, w)
				continue
			}
			w.result <- fileResult{err: core.Upstream(504, "file_parse_timeout", "uploaded file parsing timed out")}
		}
		if len(active) == 0 {
			delete(p.pending, id)
		} else {
			p.pending[id] = active
		}
	}
	p.mu.Unlock()
}

func (p *FileParsePoller) settle(id string, result fileResult) {
	p.mu.Lock()
	waiters := p.pending[id]
	delete(p.pending, id)
	p.mu.Unlock()
	for _, w := range waiters {
		select {
		case w.result <- result:
		default:
		}
	}
}

func (p *FileParsePoller) settleAll(err error) {
	p.mu.Lock()
	ids := make([]string, 0, len(p.pending))
	for id := range p.pending {
		ids = append(ids, id)
	}
	p.mu.Unlock()
	for _, id := range ids {
		p.settle(id, fileResult{err: err})
	}
}
