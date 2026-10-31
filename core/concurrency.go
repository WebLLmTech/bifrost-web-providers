package core

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"time"
)

func CredentialID(secret string) [32]byte { return sha256.Sum256([]byte(secret)) }

type gateEntry struct {
	slots chan struct{}
	refs  int
}
type AccountGate struct {
	mu      sync.Mutex
	entries map[[32]byte]*gateEntry
	limit   int
}

func NewAccountGate(limit int) *AccountGate {
	if limit < 1 {
		limit = 1
	}
	return &AccountGate{limit: limit, entries: map[[32]byte]*gateEntry{}}
}
func (g *AccountGate) Acquire(ctx context.Context, credential string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := CredentialID(credential)
	g.mu.Lock()
	e := g.entries[key]
	if e == nil {
		e = &gateEntry{slots: make(chan struct{}, g.limit)}
		g.entries[key] = e
	}
	e.refs++
	g.mu.Unlock()
	drop := func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		e.refs--
		if e.refs == 0 {
			delete(g.entries, key)
		}
	}
	select {
	case e.slots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-e.slots; drop() }) }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

type cacheEntry[T any] struct {
	value  T
	expiry time.Time
}
type cacheFlight[T any] struct {
	done        chan struct{}
	value       T
	err         error
	invalidated bool
}

// RefreshCache performs a bounded shared refresh; canceling one caller does
// not cancel other callers. Close cancels all in-flight refreshes.
type RefreshCache[T any] struct {
	mu      sync.Mutex
	entries map[[32]byte]cacheEntry[T]
	flights map[[32]byte]*cacheFlight[T]
	base    context.Context
	cancel  context.CancelFunc
	timeout time.Duration
	max     int
	closed  bool
}

func NewRefreshCache[T any](max int, refreshTimeout time.Duration) *RefreshCache[T] {
	if max < 1 {
		max = 256
	}
	if refreshTimeout <= 0 {
		refreshTimeout = 30 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &RefreshCache[T]{entries: map[[32]byte]cacheEntry[T]{}, flights: map[[32]byte]*cacheFlight[T]{}, base: ctx, cancel: cancel, timeout: refreshTimeout, max: max}
}
func (c *RefreshCache[T]) Get(ctx context.Context, credential string, load func(context.Context) (T, time.Time, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	key := CredentialID(credential)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return zero, errors.New("refresh cache is closed")
	}
	if e, ok := c.entries[key]; ok && time.Now().Before(e.expiry) {
		c.mu.Unlock()
		return e.value, nil
	}
	f := c.flights[key]
	if f == nil {
		if len(c.flights) >= c.max {
			c.mu.Unlock()
			return zero, Upstream(429, "refresh_capacity", "Too many concurrent credential refreshes")
		}
		f = &cacheFlight[T]{done: make(chan struct{})}
		c.flights[key] = f
		go func() {
			refreshCtx, cancel := context.WithTimeout(c.base, c.timeout)
			defer cancel()
			var value T
			var expiry time.Time
			var err error
			func() {
				defer func() {
					if recover() != nil {
						err = errors.New("credential refresh failed")
					}
				}()
				value, expiry, err = load(refreshCtx)
			}()
			c.mu.Lock()
			defer c.mu.Unlock()
			f.value, f.err = value, err
			delete(c.flights, key)
			if err == nil && !c.closed && !f.invalidated && time.Now().Before(expiry) {
				for k, e := range c.entries {
					if !time.Now().Before(e.expiry) {
						delete(c.entries, k)
					}
				}
				if len(c.entries) >= c.max {
					for k := range c.entries {
						delete(c.entries, k)
						break
					}
				}
				c.entries[key] = cacheEntry[T]{value, expiry}
			}
			close(f.done)
		}()
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case <-f.done:
		return f.value, f.err
	}
}
func (c *RefreshCache[T]) Invalidate(credential string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := CredentialID(credential)
	delete(c.entries, key)
	if f := c.flights[key]; f != nil {
		f.invalidated = true
	}
}
func (c *RefreshCache[T]) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		c.cancel()
		clear(c.entries)
	}
}
