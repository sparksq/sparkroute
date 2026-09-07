package savedtrace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type LossReason string

const (
	LossInvalidRecord  LossReason = "invalid_record"
	LossQueueFull      LossReason = "queue_full"
	LossRecorderClosed LossReason = "recorder_closed"
	LossStoreFailure   LossReason = "store_failure"
)

type LossEvent struct {
	At     time.Time
	Reason LossReason
	Err    error
}

type OverflowPolicy string

const (
	DefaultAsyncQueueCapacity = 4096
	DefaultAsyncBatchSize     = 256
	DefaultAsyncBatchInterval = 2 * time.Millisecond
	DefaultSessionInterval    = time.Second

	OverflowBlock OverflowPolicy = "block"
	OverflowDrop  OverflowPolicy = "drop"
)

func ParseOverflowPolicy(value string) (OverflowPolicy, error) {
	switch policy := OverflowPolicy(strings.ToLower(strings.TrimSpace(value))); policy {
	case OverflowBlock, OverflowDrop:
		return policy, nil
	default:
		return "", fmt.Errorf("saved trace overflow policy must be block or drop")
	}
}

type AsyncOptions struct {
	QueueCapacity int
	WriteTimeout  time.Duration
	Workers       int
	Overflow      OverflowPolicy
	BatchSize     int
	BatchInterval time.Duration
	OnLoss        func(LossEvent)
	// SessionID may be supplied by tests or an embedding application. A secure
	// process-unique identifier is generated when it is empty.
	SessionID string
	// SessionInterval bounds capture-accounting checkpoint overhead. Trace
	// batches remain durable independently of these replica-level counters.
	SessionInterval time.Duration
}

type RecorderStats struct {
	Accepted      uint64
	Persisted     uint64
	Pending       uint64
	Lost          uint64
	Invalid       uint64
	QueueFull     uint64
	Closed        uint64
	StoreFailures uint64
}

type AsyncRecorder struct {
	store   Store
	options AsyncOptions
	queue   chan Record
	done    chan struct{}

	stateMu   sync.RWMutex
	closed    bool
	closeOnce sync.Once
	errMu     sync.Mutex
	closeErr  error

	accepted      atomic.Uint64
	persisted     atomic.Uint64
	pending       atomic.Uint64
	lost          atomic.Uint64
	invalid       atomic.Uint64
	queueFull     atomic.Uint64
	closedRecords atomic.Uint64
	storeFailures atomic.Uint64

	sessionStore CaptureSessionStore
	sessionID    string
	sessionStart time.Time
	sessionLast  time.Time
	sessionMu    sync.Mutex
}

func NewAsyncRecorder(store Store, options AsyncOptions) (*AsyncRecorder, error) {
	if store == nil {
		return nil, fmt.Errorf("saved trace store is required")
	}
	if options.QueueCapacity == 0 {
		options.QueueCapacity = DefaultAsyncQueueCapacity
	}
	if options.WriteTimeout == 0 {
		options.WriteTimeout = 30 * time.Second
	}
	if options.Workers == 0 {
		options.Workers = 1
	}
	if options.Overflow == "" {
		options.Overflow = OverflowBlock
	}
	if options.BatchSize == 0 {
		options.BatchSize = DefaultAsyncBatchSize
	}
	if options.BatchInterval == 0 {
		options.BatchInterval = DefaultAsyncBatchInterval
	}
	if options.SessionInterval == 0 {
		options.SessionInterval = DefaultSessionInterval
	}
	if options.QueueCapacity <= 0 || options.WriteTimeout <= 0 || options.Workers <= 0 {
		return nil, fmt.Errorf("saved trace queue capacity, write timeout, and worker count must be positive")
	}
	if options.BatchSize <= 0 || options.BatchInterval <= 0 || options.SessionInterval <= 0 {
		return nil, fmt.Errorf("saved trace batch size, batch interval, and session interval must be positive")
	}
	if options.Overflow != OverflowBlock && options.Overflow != OverflowDrop {
		return nil, fmt.Errorf("saved trace overflow policy must be block or drop")
	}
	recorder := &AsyncRecorder{
		store:   store,
		options: options,
		queue:   make(chan Record, options.QueueCapacity),
		done:    make(chan struct{}),
	}
	if sessionStore, ok := store.(CaptureSessionStore); ok {
		recorder.sessionStore = sessionStore
		recorder.sessionID = options.SessionID
		if recorder.sessionID == "" {
			identifier := make([]byte, 16)
			if _, err := rand.Read(identifier); err != nil {
				return nil, fmt.Errorf("create saved trace capture session ID: %w", err)
			}
			recorder.sessionID = hex.EncodeToString(identifier)
		}
		recorder.sessionStart = time.Now().UTC()
		if err := recorder.persistSession(context.Background(), false, true); err != nil {
			return nil, fmt.Errorf("start saved trace capture session: %w", err)
		}
	}
	var workers sync.WaitGroup
	workers.Add(options.Workers)
	for range options.Workers {
		go func() {
			defer workers.Done()
			recorder.runWorker()
		}()
	}
	go func() {
		workers.Wait()
		close(recorder.done)
	}()
	return recorder, nil
}

// Record applies the configured overflow policy. The default block policy
// provides bounded backpressure and never discards a valid record merely
// because the queue is full. Drop is an explicit best-effort compatibility
// mode.
func (r *AsyncRecorder) Record(record Record) {
	if err := record.Validate(); err != nil {
		r.loss(LossInvalidRecord, err)
		return
	}
	record = cloneRecord(record)
	if r.sessionID != "" {
		record.CaptureSessionID = r.sessionID
	}
	r.stateMu.RLock()
	if r.closed {
		r.stateMu.RUnlock()
		r.loss(LossRecorderClosed, nil)
		return
	}
	switch r.options.Overflow {
	case OverflowBlock:
		r.accepted.Add(1)
		r.pending.Add(1)
		r.queue <- record
	case OverflowDrop:
		r.accepted.Add(1)
		r.pending.Add(1)
		select {
		case r.queue <- record:
		default:
			r.accepted.Add(^uint64(0))
			r.pending.Add(^uint64(0))
			r.stateMu.RUnlock()
			r.loss(LossQueueFull, nil)
			return
		}
	}
	r.stateMu.RUnlock()
}

func (r *AsyncRecorder) Lost() uint64 {
	return r.lost.Load()
}

func (r *AsyncRecorder) Stats() RecorderStats {
	return RecorderStats{
		Accepted:      r.accepted.Load(),
		Persisted:     r.persisted.Load(),
		Pending:       r.pending.Load(),
		Lost:          r.lost.Load(),
		Invalid:       r.invalid.Load(),
		QueueFull:     r.queueFull.Load(),
		Closed:        r.closedRecords.Load(),
		StoreFailures: r.storeFailures.Load(),
	}
}

// Close drains queued records. Store ownership remains with the composition
// root because database trace storage may share the usage-ledger connection.
func (r *AsyncRecorder) Close(ctx context.Context) error {
	r.closeOnce.Do(func() {
		r.stateMu.Lock()
		r.closed = true
		close(r.queue)
		r.stateMu.Unlock()
	})
	select {
	case <-r.done:
		if err := r.persistSession(ctx, true, true); err != nil {
			r.setCloseError(fmt.Errorf("complete saved trace capture session: %w", err))
		}
		r.errMu.Lock()
		defer r.errMu.Unlock()
		return r.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *AsyncRecorder) runWorker() {
	batchStore, supportsBatch := r.store.(BatchStore)
	for {
		record, open := <-r.queue
		if !open {
			return
		}
		records := []Record{record}
		if supportsBatch && r.options.BatchSize > 1 {
			timer := time.NewTimer(r.options.BatchInterval)
		collect:
			for len(records) < r.options.BatchSize {
				select {
				case record, open = <-r.queue:
					if !open {
						break collect
					}
					records = append(records, record)
				case <-timer.C:
					break collect
				}
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), r.options.WriteTimeout)
		var err error
		if supportsBatch {
			err = batchStore.AppendTraceBatch(ctx, records)
		} else {
			err = r.store.Append(ctx, record)
		}
		cancel()
		r.pending.Add(0 - uint64(len(records)))
		if err != nil {
			r.setCloseError(fmt.Errorf("persist saved trace: %w", err))
			for range records {
				r.loss(LossStoreFailure, err)
			}
			if sessionErr := r.persistSession(context.Background(), false, true); sessionErr != nil {
				r.setCloseError(fmt.Errorf("update saved trace capture session: %w", sessionErr))
			}
			continue
		}
		r.persisted.Add(uint64(len(records)))
		if sessionErr := r.persistSession(context.Background(), false, false); sessionErr != nil {
			r.setCloseError(fmt.Errorf("update saved trace capture session: %w", sessionErr))
		}
		if !open {
			return
		}
	}
}

func (r *AsyncRecorder) setCloseError(err error) {
	if err == nil {
		return
	}
	r.errMu.Lock()
	if r.closeErr == nil {
		r.closeErr = err
	}
	r.errMu.Unlock()
}

func (r *AsyncRecorder) persistSession(parent context.Context, completed, force bool) error {
	if r.sessionStore == nil {
		return nil
	}
	r.sessionMu.Lock()
	defer r.sessionMu.Unlock()
	now := time.Now().UTC()
	if !force && !r.sessionLast.IsZero() && now.Sub(r.sessionLast) < r.options.SessionInterval {
		return nil
	}
	stats := r.Stats()
	session := CaptureSession{
		ID: r.sessionID, StartedAt: r.sessionStart, UpdatedAt: now,
		Mode: string(r.options.Overflow), Accepted: stats.Accepted,
		Persisted: stats.Persisted, Pending: stats.Pending, Lost: stats.Lost,
		Invalid: stats.Invalid, QueueFull: stats.QueueFull, Closed: stats.Closed,
		StoreFailures: stats.StoreFailures,
	}
	if completed {
		session.CompletedAt = &now
	}
	ctx, cancel := context.WithTimeout(parent, r.options.WriteTimeout)
	defer cancel()
	if err := r.sessionStore.UpsertCaptureSession(ctx, session); err != nil {
		return err
	}
	r.sessionLast = now
	return nil
}

func (r *AsyncRecorder) loss(reason LossReason, err error) {
	r.lost.Add(1)
	switch reason {
	case LossInvalidRecord:
		r.invalid.Add(1)
	case LossQueueFull:
		r.queueFull.Add(1)
	case LossRecorderClosed:
		r.closedRecords.Add(1)
	case LossStoreFailure:
		r.storeFailures.Add(1)
	}
	if r.options.OnLoss == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	r.options.OnLoss(LossEvent{At: time.Now(), Reason: reason, Err: err})
}

func cloneRecord(record Record) Record {
	record.Metadata = cloneMap(record.Metadata)
	return record
}

func cloneMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

var _ Recorder = (*AsyncRecorder)(nil)
