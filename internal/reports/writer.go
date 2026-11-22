package reports

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrQueueFull    = errors.New("report write queue full")
	ErrWriterClosed = errors.New("report writer closed")
)

// SnapshotProcessor is intentionally limited to one report transaction.
type SnapshotProcessor interface {
	Apply(context.Context, int64, string, Prepared) (int64, error)
}

type WriterOptions struct {
	QueueCapacity      int
	TransactionTimeout time.Duration
}

func DefaultWriterOptions() WriterOptions {
	return WriterOptions{QueueCapacity: 128, TransactionTimeout: 5 * time.Second}
}

type WriteResult struct {
	Revision int64
	Err      error
}

type writeRequest struct {
	ctx               context.Context
	writerID          int64
	day               string
	prepared          Prepared
	result            chan WriteResult
	maintenance       func(context.Context) error
	maintenanceResult chan error
}

// Writer has one worker and a bounded pending queue. The queue itself is never
// closed, so concurrent admission and shutdown cannot send on a closed channel.
type Writer struct {
	mu          sync.Mutex
	closed      bool
	queue       chan writeRequest
	maintenance chan writeRequest
	stopping    chan struct{}
	done        chan struct{}
	abort       context.Context
	cancel      context.CancelFunc
	processor   SnapshotProcessor
	timeout     time.Duration
}

func NewWriter(processor SnapshotProcessor, options WriterOptions) (*Writer, error) {
	if processor == nil || options.QueueCapacity <= 0 || options.TransactionTimeout <= 0 {
		return nil, fmt.Errorf("invalid report writer options")
	}
	abort, cancel := context.WithCancel(context.Background())
	w := &Writer{queue: make(chan writeRequest, options.QueueCapacity), maintenance: make(chan writeRequest, 1), stopping: make(chan struct{}), done: make(chan struct{}), abort: abort, cancel: cancel, processor: processor, timeout: options.TransactionTimeout}
	go w.run()
	return w, nil
}

// Enqueue returns immediately on admission or full queue. The result channel is
// buffered: a caller that disconnects never blocks the single worker.
func (w *Writer) Enqueue(ctx context.Context, writerID int64, day string, prepared Prepared) (<-chan WriteResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, ErrWriterClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	req := writeRequest{ctx: ctx, writerID: writerID, day: day, prepared: prepared, result: make(chan WriteResult, 1)}
	select {
	case w.queue <- req:
		return req.result, nil
	default:
		return nil, ErrQueueFull
	}
}

// EnqueueMaintenance 为单个维护事务预留一个有界槽，与报告交替执行。
func (w *Writer) EnqueueMaintenance(ctx context.Context, operation func(context.Context) error) (<-chan error, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, ErrWriterClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if operation == nil {
		return nil, errors.New("nil maintenance operation")
	}
	req := writeRequest{ctx: ctx, maintenance: operation, maintenanceResult: make(chan error, 1)}
	select {
	case w.maintenance <- req:
		return req.maintenanceResult, nil
	default:
		return nil, ErrQueueFull
	}
}

func (w *Writer) run() {
	defer close(w.done)
	preferMaintenance, draining := false, false
	for {
		// 两边均有积压时严格交替；一方空闲不妨碍另一方推进。
		var preferred <-chan writeRequest
		if preferMaintenance {
			preferred = w.maintenance
		} else {
			preferred = w.queue
		}
		select {
		case req := <-preferred:
			w.execute(req)
			preferMaintenance = req.maintenance == nil
			continue
		default:
		}
		if draining {
			select {
			case req := <-w.queue:
				w.execute(req)
				preferMaintenance = true
			case req := <-w.maintenance:
				w.execute(req)
				preferMaintenance = false
			default:
				return
			}
			continue
		}
		select {
		case req := <-w.queue:
			w.execute(req)
			preferMaintenance = true
		case req := <-w.maintenance:
			w.execute(req)
			preferMaintenance = false
		case <-w.stopping:
			// 入队与关闭受同一锁保护，已接纳的队列必定有限。
			draining = true
		}
	}
}

func (w *Writer) execute(req writeRequest) {
	ctx, cancel := context.WithTimeout(req.ctx, w.timeout)
	stop := context.AfterFunc(w.abort, cancel)
	defer func() { stop(); cancel() }()
	if w.abort.Err() != nil {
		cancel()
	}
	if req.maintenance != nil {
		if err := ctx.Err(); err != nil {
			req.maintenanceResult <- err
		} else {
			req.maintenanceResult <- req.maintenance(ctx)
		}
		return
	}
	if err := ctx.Err(); err != nil {
		req.result <- WriteResult{Err: err}
		return
	}
	revision, err := w.processor.Apply(ctx, req.writerID, req.day, req.prepared)
	req.result <- WriteResult{Revision: revision, Err: err}
}

// Close stops admission, drains accepted work until ctx expires, then cancels
// unfinished transactions and waits for rollback/worker exit before returning.
// The database must be closed only after this returns.
func (w *Writer) Close(ctx context.Context) error {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.stopping)
	}
	w.mu.Unlock()
	select {
	case <-w.done:
		w.cancel()
		return nil
	case <-ctx.Done():
		w.cancel()
		<-w.done
		return ctx.Err()
	}
}
