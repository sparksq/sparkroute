package gateway

import (
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const (
	idleBodyOpen int32 = iota
	idleBodyClosed
	idleBodyTimedOut
)

// streamIdleReadCloser bounds each blocking upstream read independently. The
// timer is stopped while the gateway writes downstream, so a slow caller and
// normal backpressure are not misclassified as an idle upstream.
type streamIdleReadCloser struct {
	source  io.ReadCloser
	timeout time.Duration

	state     atomic.Int32
	closeOnce sync.Once
	closeErr  error
}

func newStreamIdleReadCloser(
	source io.ReadCloser,
	timeout time.Duration,
) *streamIdleReadCloser {
	return &streamIdleReadCloser{
		source:  source,
		timeout: timeout,
	}
}

func (r *streamIdleReadCloser) Read(buffer []byte) (int, error) {
	if r.timeout <= 0 {
		return r.source.Read(buffer)
	}
	var completed atomic.Bool
	timer := time.AfterFunc(r.timeout, func() {
		if !completed.CompareAndSwap(false, true) {
			return
		}
		if r.state.CompareAndSwap(idleBodyOpen, idleBodyTimedOut) {
			r.closeSource()
		}
	})
	count, err := r.source.Read(buffer)
	if completed.CompareAndSwap(false, true) {
		timer.Stop()
	}
	return count, err
}

func (r *streamIdleReadCloser) Close() error {
	r.state.CompareAndSwap(idleBodyOpen, idleBodyClosed)
	r.closeSource()
	return r.closeErr
}

func (r *streamIdleReadCloser) TimedOut() bool {
	return r.state.Load() == idleBodyTimedOut
}

func (r *streamIdleReadCloser) closeSource() {
	r.closeOnce.Do(func() {
		r.closeErr = r.source.Close()
	})
}
