// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Package promptcache provides content-free, best-effort prompt-prefix route
// affinity. It stores keyed fingerprints and route identifiers, never prompt
// content, and is deliberately separate from hard provider-owned state.
package promptcache

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type Prefix struct {
	Digest   string
	Bytes    int
	Segments int
}

type Route struct {
	Provider         string
	Deployment       string
	UpstreamModel    string
	UpstreamProtocol string
}

type Query struct {
	ScopeDigest    string
	VirtualModel   string
	Operation      string
	ConfigRevision string
	Prefixes       []Prefix // longest first
	Candidates     []Route
	Now            time.Time
}

type Match struct {
	Prefix      Prefix
	Route       Route
	LastSuccess time.Time
	ExpiresAt   time.Time
}

type Observation struct {
	ScopeDigest    string
	VirtualModel   string
	Operation      string
	ConfigRevision string
	Prefixes       []Prefix
	Route          Route
	LastSuccess    time.Time
	ExpiresAt      time.Time
}

type Backend interface {
	Lookup(context.Context, Query) (Match, bool, error)
	Record(context.Context, Observation) error
}

type DirectoryOptions struct {
	QueueSize     int
	LookupTimeout time.Duration
	WriteTimeout  time.Duration
	OnError       func(error)
	OnLoss        func()
}

// Directory makes successful-route observations non-blocking while preserving
// synchronous, fail-open lookups on the request planning path.
type Directory struct {
	backend       Backend
	queue         chan Observation
	onError       func(error)
	onLoss        func()
	done          chan struct{}
	lookupTimeout time.Duration
	writeTimeout  time.Duration
	once          sync.Once
	stateMu       sync.RWMutex
	closed        bool
}

func NewDirectory(backend Backend, options DirectoryOptions) (*Directory, error) {
	if backend == nil {
		return nil, fmt.Errorf("prompt-cache backend is required")
	}
	queueSize := options.QueueSize
	if queueSize == 0 {
		queueSize = 1024
	}
	if queueSize < 1 {
		return nil, fmt.Errorf("prompt-cache queue size must be positive")
	}
	lookupTimeout := options.LookupTimeout
	if lookupTimeout == 0 {
		lookupTimeout = 50 * time.Millisecond
	}
	writeTimeout := options.WriteTimeout
	if writeTimeout == 0 {
		writeTimeout = 5 * time.Second
	}
	if lookupTimeout < 0 || writeTimeout < 0 {
		return nil, fmt.Errorf("prompt-cache timeouts must be positive")
	}
	directory := &Directory{
		backend:       backend,
		queue:         make(chan Observation, queueSize),
		lookupTimeout: lookupTimeout,
		writeTimeout:  writeTimeout,
		onError:       options.OnError,
		onLoss:        options.OnLoss,
		done:          make(chan struct{}),
	}
	go directory.run()
	return directory, nil
}

func (d *Directory) Lookup(ctx context.Context, query Query) (Match, bool) {
	if d == nil || len(query.Prefixes) == 0 || len(query.Candidates) == 0 {
		return Match{}, false
	}
	lookupContext, cancel := context.WithTimeout(ctx, d.lookupTimeout)
	defer cancel()
	match, found, err := d.backend.Lookup(lookupContext, query)
	if err != nil {
		if d.onError != nil {
			d.onError(fmt.Errorf("prompt-cache affinity lookup: %w", err))
		}
		return Match{}, false
	}
	return match, found
}

func (d *Directory) Record(observation Observation) {
	if d == nil || len(observation.Prefixes) == 0 {
		return
	}
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	if d.closed {
		if d.onLoss != nil {
			d.onLoss()
		}
		return
	}
	select {
	case d.queue <- observation:
	default:
		if d.onLoss != nil {
			d.onLoss()
		}
	}
}

func (d *Directory) Close(ctx context.Context) error {
	if d == nil {
		return nil
	}
	d.once.Do(func() {
		d.stateMu.Lock()
		d.closed = true
		close(d.queue)
		d.stateMu.Unlock()
	})
	select {
	case <-d.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *Directory) run() {
	defer close(d.done)
	for observation := range d.queue {
		writeContext, cancel := context.WithTimeout(context.Background(), d.writeTimeout)
		err := d.backend.Record(writeContext, observation)
		cancel()
		if err != nil &&
			d.onError != nil {
			d.onError(fmt.Errorf("prompt-cache affinity record: %w", err))
		}
	}
}
