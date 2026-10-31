package core

import (
	"context"
	"sync"
	"time"
)

// ManagedStream guarantees cleanup once, even if EOF, cancellation and Close
// race. Cleanup has its own deadline and cannot mask an inference error.
type managedStream struct {
	stream  EventStream
	once    sync.Once
	cleanup func(context.Context) error
	base    context.Context
	timeout time.Duration
	stop    func() bool
	mu      sync.Mutex
}

func ManagedStream(ctx context.Context, stream EventStream, cleanup func(context.Context) error, timeout time.Duration) EventStream {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	s := &managedStream{stream: stream, cleanup: cleanup, base: context.WithoutCancel(ctx), timeout: timeout}
	s.mu.Lock()
	s.stop = context.AfterFunc(ctx, func() { _ = s.Close() })
	s.mu.Unlock()
	return s
}
func (s *managedStream) Next(ctx context.Context) (Event, error) {
	e, err := s.stream.Next(ctx)
	if err != nil {
		_ = s.Close()
	}
	return e, err
}
func (s *managedStream) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		if s.stop != nil {
			s.stop()
		}
		s.mu.Unlock()
		_ = CloseStream(s.stream)
		if s.cleanup != nil {
			ctx, cancel := context.WithTimeout(s.base, s.timeout)
			defer cancel()
			_ = s.cleanup(ctx)
		}
	})
	return nil
}

// CloseStream keeps a provider Close panic from skipping higher-level cleanup.
func CloseStream(stream EventStream) (err error) {
	defer func() {
		if recover() != nil {
			err = Upstream(502, "stream_close_failed", "Provider stream cleanup failed")
		}
	}()
	return stream.Close()
}
func Conversation[T any](ctx context.Context, create func(context.Context) (T, error), open func(context.Context, T) (EventStream, error), cleanup func(context.Context, T) error) (stream EventStream, err error) {
	value, err := create(ctx)
	if err != nil {
		return nil, err
	}
	clean := func(c context.Context) error {
		if cleanup != nil {
			return cleanup(c, value)
		}
		return nil
	}
	defer func() {
		if stream == nil {
			c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = clean(c)
		}
	}()
	stream, err = open(ctx, value)
	if err != nil {
		if stream != nil {
			_ = stream.Close()
			stream = nil
		}
		return nil, err
	}
	if stream == nil {
		return nil, Upstream(502, "empty_stream", "Provider returned no stream")
	}
	stream = ManagedStream(ctx, stream, clean, 5*time.Second)
	return stream, nil
}
