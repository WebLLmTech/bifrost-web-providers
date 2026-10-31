package core

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

type SSEEvent struct {
	Event string
	Data  string
	ID    string
	Retry int
}

// SSEReader handles LF/CRLF, comments, multiline data and fragmented UTF-8.
// Closing the reader unblocks network reads. Bare EOF never invents [DONE].
type SSEReader struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
	max     int
	once    sync.Once
	cancel  func() bool
	mu      sync.Mutex
}

func NewSSEReader(ctx context.Context, body io.ReadCloser, maxEventBytes int) *SSEReader {
	if maxEventBytes <= 0 {
		maxEventBytes = 4 << 20
	}
	s := &SSEReader{body: body, max: maxEventBytes, scanner: bufio.NewScanner(body)}
	s.scanner.Buffer(make([]byte, 4096), maxEventBytes)
	s.mu.Lock()
	s.cancel = context.AfterFunc(ctx, func() { _ = s.Close() })
	s.mu.Unlock()
	return s
}
func (s *SSEReader) Close() error {
	var err error
	s.once.Do(func() {
		s.mu.Lock()
		if s.cancel != nil {
			s.cancel()
		}
		s.mu.Unlock()
		err = s.body.Close()
	})
	return err
}
func (s *SSEReader) Next(ctx context.Context) (SSEEvent, error) {
	e := SSEEvent{}
	var data []string
	size := 0
	for s.scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return SSEEvent{}, err
		}
		line := s.scanner.Text()
		size += len(line) + 1
		if size > s.max {
			return SSEEvent{}, Upstream(502, "event_too_large", "Upstream SSE event exceeds limit")
		}
		if line == "" {
			if len(data) > 0 {
				e.Data = strings.Join(data, "\n")
				return e, nil
			}
			e = SSEEvent{}
			size = 0
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		key, v, _ := strings.Cut(line, ":")
		v = strings.TrimPrefix(v, " ")
		switch key {
		case "event":
			e.Event = v
		case "data":
			data = append(data, v)
		case "id":
			if !strings.ContainsRune(v, 0) {
				e.ID = v
			}
		case "retry":
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				e.Retry = n
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return SSEEvent{}, err
	}
	if err := s.scanner.Err(); err != nil {
		return SSEEvent{}, Upstream(502, "invalid_sse", "Could not read upstream SSE")
	}
	// The HTML event stream spec discards an event lacking its final blank line.
	return SSEEvent{}, io.EOF
}
func WriteSSE(w io.Writer, event string, data []byte) error {
	if strings.ContainsAny(event, "\r\n") {
		return fmt.Errorf("invalid SSE event name")
	}
	if event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
			return err
		}
	}
	for _, line := range strings.Split(string(data), "\n") {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "\n")
	return err
}
