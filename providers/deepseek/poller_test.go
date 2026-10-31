package deepseek

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func testAttachment(id, status string) attachment {
	return attachment{ID: id, Status: status, FileName: id + ".txt", FileSize: 1, ModelKind: "VISION"}
}

func successFiles(ids []string) []attachment {
	out := make([]attachment, len(ids))
	for i, id := range ids {
		out[i] = testAttachment(id, "SUCCESS")
	}
	return out
}

// Port of the Deno test "one poll queries all pending files and resolves each
// result": concurrent waiters must share a single batched fetch.
func TestPollerBatchesPendingFiles(t *testing.T) {
	var mu sync.Mutex
	var requested [][]string
	poller := NewFileParsePoller(context.Background(),
		func(ctx context.Context, ids []string) ([]attachment, error) {
			mu.Lock()
			requested = append(requested, append([]string(nil), ids...))
			mu.Unlock()
			return successFiles(ids), nil
		},
		50*time.Millisecond, 5*time.Second, 200*time.Millisecond, nil)

	results := make([]attachment, 5)
	errs := make([]error, 5)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = poller.WaitFor(context.Background(), testAttachment("file-"+strconv.Itoa(i), "PENDING"))
		}(i)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(requested) != 1 || len(requested[0]) != 5 {
		t.Fatalf("expected one batched query with 5 ids, got %v", requested)
	}
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("waiter %d: %v", i, errs[i])
		}
		if results[i].Status != "SUCCESS" {
			t.Fatalf("waiter %d resolved with status %q", i, results[i].Status)
		}
	}
}

// Port of the Deno test "429 backs off and retries the same pending files".
func TestPollerBacksOffOnRateLimit(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	poller := NewFileParsePoller(context.Background(),
		func(ctx context.Context, ids []string) ([]attachment, error) {
			mu.Lock()
			attempts++
			n := attempts
			mu.Unlock()
			if n == 1 {
				return nil, &FileRateLimitError{RetryAfter: 10 * time.Millisecond}
			}
			return successFiles(ids), nil
		},
		10*time.Millisecond, 5*time.Second, 50*time.Millisecond, nil)

	file, err := poller.WaitFor(context.Background(), testAttachment("file-1", "PENDING"))
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
	if file.Status != "SUCCESS" {
		t.Fatalf("unexpected status %q", file.Status)
	}
}

func TestPollerTimeout(t *testing.T) {
	poller := NewFileParsePoller(context.Background(),
		func(ctx context.Context, ids []string) ([]attachment, error) {
			return []attachment{testAttachment(ids[0], "PENDING")}, nil
		},
		5*time.Millisecond, 40*time.Millisecond, 20*time.Millisecond, nil)

	_, err := poller.WaitFor(context.Background(), testAttachment("file-1", "PENDING"))
	if err == nil || !strings.Contains(err.Error(), "uploaded file parsing timed out: file-1") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}

func TestPollerTerminalFailure(t *testing.T) {
	code := "CONTENT_FILTER"
	failed := testAttachment("file-1", "FAILED")
	failed.ErrorCode = &code
	poller := NewFileParsePoller(context.Background(),
		func(ctx context.Context, ids []string) ([]attachment, error) {
			return []attachment{failed}, nil
		},
		5*time.Millisecond, 5*time.Second, 20*time.Millisecond, nil)

	_, err := poller.WaitFor(context.Background(), testAttachment("file-1", "PENDING"))
	if err == nil || err.Error() != "uploaded file parsing failed: CONTENT_FILTER" {
		t.Fatalf("expected failure error, got %v", err)
	}
}

func TestPollerTerminalStatusShortCircuits(t *testing.T) {
	called := false
	poller := NewFileParsePoller(context.Background(),
		func(ctx context.Context, ids []string) ([]attachment, error) {
			called = true
			return successFiles(ids), nil
		},
		5*time.Millisecond, time.Second, 20*time.Millisecond, nil)

	file, err := poller.WaitFor(context.Background(), testAttachment("file-1", "SUCCESS"))
	if err != nil || file.Status != "SUCCESS" || called {
		t.Fatalf("SUCCESS must settle without polling: %v %v %v", file, err, called)
	}
	code := "CONTENT_EMPTY"
	failed := testAttachment("file-2", "CONTENT_EMPTY")
	failed.ErrorCode = &code
	if _, err := poller.WaitFor(context.Background(), failed); err == nil ||
		err.Error() != "uploaded file parsing failed: CONTENT_EMPTY" {
		t.Fatalf("expected immediate failure error, got %v", err)
	}
	if called {
		t.Fatal("terminal statuses must not start the poll loop")
	}
}

func TestPollerFetchErrorSettlesWaiters(t *testing.T) {
	poller := NewFileParsePoller(context.Background(),
		func(ctx context.Context, ids []string) ([]attachment, error) {
			return nil, context.DeadlineExceeded
		},
		5*time.Millisecond, 5*time.Second, 20*time.Millisecond, nil)

	if _, err := poller.WaitFor(context.Background(), testAttachment("file-1", "PENDING")); err != context.DeadlineExceeded {
		t.Fatalf("expected fetch error to surface, got %v", err)
	}
}

func TestPollerWaiterCancelStopsLoopAndReportsIdle(t *testing.T) {
	idle := make(chan struct{}, 4)
	poller := NewFileParsePoller(context.Background(),
		func(ctx context.Context, ids []string) ([]attachment, error) {
			return []attachment{testAttachment(ids[0], "PENDING")}, nil
		},
		200*time.Millisecond, 5*time.Second, time.Second,
		func() { idle <- struct{}{} })

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := poller.WaitFor(ctx, testAttachment("file-1", "PENDING"))
		errCh <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-errCh; err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	// The cancelled waiter must wake the loop, which exits and reports idle.
	select {
	case <-idle:
	case <-time.After(2 * time.Second):
		t.Fatal("poll loop did not report idle after cancel")
	}
	poller.mu.Lock()
	running := poller.running
	pending := len(poller.pending)
	poller.mu.Unlock()
	if running || pending != 0 {
		t.Fatalf("poller not cleaned up: running=%v pending=%d", running, pending)
	}
}

func TestPollerBaseContextCancelSettlesAll(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	poller := NewFileParsePoller(base,
		func(ctx context.Context, ids []string) ([]attachment, error) {
			return []attachment{testAttachment(ids[0], "PENDING")}, nil
		},
		200*time.Millisecond, 5*time.Second, time.Second, nil)

	errCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			_, err := poller.WaitFor(context.Background(), testAttachment("file-"+strconv.Itoa(i), "PENDING"))
			errCh <- err
		}(i)
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errCh:
			if err != context.Canceled {
				t.Fatalf("waiter %d: expected context.Canceled, got %v", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("waiter %d not settled after base cancel", i)
		}
	}
}
