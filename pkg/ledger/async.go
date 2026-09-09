// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package ledger

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type LossReason string

const (
	LossInvalidRecord LossReason = "invalid_record"
	LossQueueFull     LossReason = "queue_full"
	LossWriterClosed  LossReason = "writer_closed"
	LossStoreFailure  LossReason = "store_failure"
)

type LossEvent struct {
	At     time.Time
	Reason LossReason
	Count  uint64
	Err    error
}

type AsyncOptions struct {
	QueueCapacity     int
	MaxBatchRecords   int
	MaxBatchBytes     int
	FlushInterval     time.Duration
	WriteTimeout      time.Duration
	MaxRetries        int
	InitialRetryDelay time.Duration
	OnLoss            func(LossEvent)
}

type Stats struct {
	QueueDepth int
	Enqueued   uint64
	Written    uint64
	Lost       uint64
	Batches    uint64
	Retries    uint64
}

type AsyncWriter struct {
	store     Store
	options   AsyncOptions
	queue     chan Record
	stop      chan struct{}
	done      chan struct{}
	lossQueue chan LossEvent
	lossStop  chan struct{}
	lossDone  chan struct{}

	stateMu   sync.RWMutex
	closed    bool
	closeOnce sync.Once
	resultMu  sync.Mutex
	closeErr  error

	enqueued atomic.Uint64
	written  atomic.Uint64
	lost     atomic.Uint64
	batches  atomic.Uint64
	retries  atomic.Uint64
}

func NewAsyncWriter(store Store, options AsyncOptions) (*AsyncWriter, error) {
	if store == nil {
		return nil, fmt.Errorf("ledger store is required")
	}
	options = withAsyncDefaults(options)
	if err := validateAsyncOptions(options); err != nil {
		return nil, err
	}
	writer := &AsyncWriter{
		store:     store,
		options:   options,
		queue:     make(chan Record, options.QueueCapacity),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		lossQueue: make(chan LossEvent, 64),
		lossStop:  make(chan struct{}),
		lossDone:  make(chan struct{}),
	}
	go writer.reportLosses()
	go writer.run()
	return writer, nil
}

func withAsyncDefaults(options AsyncOptions) AsyncOptions {
	if options.QueueCapacity == 0 {
		options.QueueCapacity = 4096
	}
	if options.MaxBatchRecords == 0 {
		options.MaxBatchRecords = 256
	}
	if options.MaxBatchBytes == 0 {
		options.MaxBatchBytes = 1 << 20
	}
	if options.FlushInterval == 0 {
		options.FlushInterval = time.Second
	}
	if options.WriteTimeout == 0 {
		options.WriteTimeout = 5 * time.Second
	}
	if options.MaxRetries == 0 {
		options.MaxRetries = 3
	}
	if options.InitialRetryDelay == 0 {
		options.InitialRetryDelay = 50 * time.Millisecond
	}
	return options
}

func validateAsyncOptions(options AsyncOptions) error {
	if options.QueueCapacity <= 0 {
		return fmt.Errorf("queue capacity must be positive")
	}
	if options.MaxBatchRecords <= 0 || options.MaxBatchBytes <= 0 {
		return fmt.Errorf("batch record and byte limits must be positive")
	}
	if options.FlushInterval <= 0 || options.WriteTimeout <= 0 {
		return fmt.Errorf("flush interval and write timeout must be positive")
	}
	if options.MaxRetries < 0 || options.MaxRetries > 16 {
		return fmt.Errorf("max retries must be between 0 and 16")
	}
	if options.InitialRetryDelay <= 0 {
		return fmt.Errorf("initial retry delay must be positive")
	}
	return nil
}

// Record never waits for storage or queue capacity. Invalid, late, and
// saturated records are counted as lost under the initial fail-open policy.
func (w *AsyncWriter) Record(record Record) {
	if err := record.Validate(); err != nil {
		w.recordLoss(LossInvalidRecord, 1, err)
		return
	}
	w.stateMu.RLock()
	if w.closed {
		w.stateMu.RUnlock()
		w.recordLoss(LossWriterClosed, 1, nil)
		return
	}
	select {
	case w.queue <- record:
		w.enqueued.Add(1)
		w.stateMu.RUnlock()
	default:
		w.stateMu.RUnlock()
		w.recordLoss(LossQueueFull, 1, nil)
	}
}

func (w *AsyncWriter) Stats() Stats {
	return Stats{
		QueueDepth: len(w.queue),
		Enqueued:   w.enqueued.Load(),
		Written:    w.written.Load(),
		Lost:       w.lost.Load(),
		Batches:    w.batches.Load(),
		Retries:    w.retries.Load(),
	}
}

func (w *AsyncWriter) Health(ctx context.Context) error {
	return w.store.Health(ctx)
}

func (w *AsyncWriter) Close(ctx context.Context) error {
	w.closeOnce.Do(func() {
		w.stateMu.Lock()
		w.closed = true
		close(w.stop)
		w.stateMu.Unlock()
	})
	select {
	case <-w.done:
		w.resultMu.Lock()
		defer w.resultMu.Unlock()
		return w.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *AsyncWriter) run() {
	defer close(w.done)
	ticker := time.NewTicker(w.options.FlushInterval)
	defer ticker.Stop()

	batch := make([]Record, 0, w.options.MaxBatchRecords)
	batchBytes := 0
	flush := func() {
		if len(batch) == 0 {
			return
		}
		w.flush(batch)
		batch = batch[:0]
		batchBytes = 0
	}
	add := func(record Record) {
		recordBytes := record.EstimatedBytes()
		if len(batch) > 0 &&
			(len(batch) >= w.options.MaxBatchRecords ||
				batchBytes+recordBytes > w.options.MaxBatchBytes) {
			flush()
		}
		batch = append(batch, record)
		batchBytes += recordBytes
		if len(batch) >= w.options.MaxBatchRecords ||
			batchBytes >= w.options.MaxBatchBytes {
			flush()
		}
	}

	for {
		select {
		case record := <-w.queue:
			add(record)
		case <-ticker.C:
			flush()
		case <-w.stop:
			for {
				select {
				case record := <-w.queue:
					add(record)
				default:
					flush()
					w.resultMu.Lock()
					w.closeErr = w.store.Close()
					w.resultMu.Unlock()
					close(w.lossStop)
					<-w.lossDone
					return
				}
			}
		}
	}
}

func (w *AsyncWriter) flush(records []Record) {
	var err error
	for attempt := 0; attempt <= w.options.MaxRetries; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), w.options.WriteTimeout)
		err = w.store.AppendBatch(ctx, records)
		cancel()
		if err == nil {
			w.written.Add(uint64(len(records)))
			w.batches.Add(1)
			return
		}
		if attempt == w.options.MaxRetries {
			break
		}
		w.retries.Add(1)
		delay := w.options.InitialRetryDelay << attempt
		if delay <= 0 || delay > time.Minute {
			delay = time.Minute
		}
		timer := time.NewTimer(delay)
		<-timer.C
	}
	w.recordLoss(LossStoreFailure, uint64(len(records)), err)
}

func (w *AsyncWriter) recordLoss(reason LossReason, count uint64, err error) {
	w.lost.Add(count)
	if w.options.OnLoss == nil {
		return
	}
	select {
	case w.lossQueue <- LossEvent{
		At:     time.Now(),
		Reason: reason,
		Count:  count,
		Err:    err,
	}:
	default:
	}
}

func (w *AsyncWriter) reportLosses() {
	defer close(w.lossDone)
	report := func(event LossEvent) {
		defer func() {
			_ = recover()
		}()
		w.options.OnLoss(event)
	}
	for {
		select {
		case event := <-w.lossQueue:
			report(event)
		case <-w.lossStop:
			for {
				select {
				case event := <-w.lossQueue:
					report(event)
				default:
					return
				}
			}
		}
	}
}

var _ Recorder = (*AsyncWriter)(nil)
