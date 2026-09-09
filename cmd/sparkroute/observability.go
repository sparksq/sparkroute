// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sparksq/sparkroute/pkg/config"
	ledgersqlite "github.com/sparksq/sparkroute/pkg/ledger/sqlite"
	"github.com/sparksq/sparkroute/pkg/savedtrace"
	tracefilesystem "github.com/sparksq/sparkroute/pkg/savedtrace/filesystem"
	"github.com/sparksq/sparkroute/pkg/telemetry/otlpexport"
	"github.com/sparksq/sparkroute/pkg/version"
	"go.opentelemetry.io/otel/trace/noop"
)

// One store per path across overlapping runtime generations. Retired requests
// finish recording before the last recorder and its store are drained/closed.
type traceStores struct {
	mu     sync.Mutex
	stores map[string]*traceStoreEntry
}
type traceStoreEntry struct {
	store        savedtrace.Store
	refs         int
	processOwned bool
}

func newTraceStores(mode, path string, store savedtrace.Store) *traceStores {
	p := &traceStores{stores: map[string]*traceStoreEntry{}}
	if store != nil && path != "" {
		absolute, _ := filepath.Abs(path)
		p.stores[mode+":"+absolute] = &traceStoreEntry{store: store, processOwned: true}
	}
	return p
}
func (p *traceStores) acquire(ctx context.Context, setting config.SavedTraceConfig) (savedtrace.Store, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := setting.Storage + ":" + filepath.Clean(setting.Path)
	entry := p.stores[key]
	if entry == nil {
		var store savedtrace.Store
		var err error
		if setting.Storage == "filesystem" {
			store, err = tracefilesystem.Open(setting.Path)
		} else {
			store, err = ledgersqlite.Open(ctx, ledgersqlite.Options{Path: setting.Path})
		}
		if err != nil {
			return nil, nil, err
		}
		entry = &traceStoreEntry{store: store}
		p.stores[key] = entry
	}
	entry.refs++
	return entry.store, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		entry.refs--
		if entry.refs == 0 && !entry.processOwned {
			_ = entry.store.Close()
			delete(p.stores, key)
		}
	}, nil
}
func validateTraceEnvironment(o *config.Observability) error {
	if err := o.Validate(); err != nil {
		return err
	}
	if o == nil || o.OTLPTraces == nil || !o.OTLPTraces.Enabled {
		return nil
	}
	for _, name := range o.OTLPTraces.HeaderEnv {
		value, ok := os.LookupEnv(name)
		if !ok || value == "" || len(value) > 8192 || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("trace header environment variable %s must contain a nonempty, valid header value on the gateway host", name)
		}
	}
	return nil
}
func configureGenerationObservability(document config.Document, options *runtimeBuildOptions) (func(), error) {
	if err := validateTraceEnvironment(document.Observability); err != nil {
		return nil, err
	}
	var cleanup []func()
	closeAll := func() {
		for i := len(cleanup) - 1; i >= 0; i-- {
			cleanup[i]()
		}
	}
	o := document.Observability
	if o == nil {
		return closeAll, nil
	}
	if s := o.SavedTraces; s != nil {
		options.SavedTraces = nil
		options.TraceReader = nil
		if s.Enabled {
			if options.TraceStores == nil {
				return nil, fmt.Errorf("managed trace storage is unavailable")
			}
			store, release, err := options.TraceStores.acquire(options.Context, *s)
			if err != nil {
				return nil, fmt.Errorf("open saved trace store: %w", err)
			}
			cleanup = append(cleanup, release)
			overflow, _ := savedtrace.ParseOverflowPolicy(s.Overflow)
			recorder, err := savedtrace.NewAsyncRecorder(store, savedtrace.AsyncOptions{QueueCapacity: s.QueueCapacity, Overflow: overflow, Workers: 1, OnLoss: func(event savedtrace.LossEvent) {
				options.Logger.Error("saved trace lost", slog.String("reason", string(event.Reason)), slog.Any("err", event.Err))
			}})
			if err != nil {
				closeAll()
				return nil, err
			}
			cleanup = append(cleanup, func() { _ = closeSavedTraceRecorder(options.Logger, recorder) })
			options.SavedTraces = recorder
			options.TraceReader = store
			options.MaxSavedTraceBytes = s.MaxBodyBytes
			if options.MaxSavedTraceBytes == 0 {
				options.MaxSavedTraceBytes = savedtrace.DefaultMaxBodyBytes
			}
		}
	}
	if t := o.OTLPTraces; t != nil {
		runtime := &otlpexport.Runtime{TracerProvider: noop.NewTracerProvider()}
		if options.Telemetry != nil {
			runtime.MeterProvider = options.Telemetry.MeterProvider
			runtime.Propagator = options.Telemetry.Propagator
		}
		if t.Enabled {
			headers := map[string]string{}
			for header, name := range t.HeaderEnv {
				headers[header] = os.Getenv(name)
			}
			service := t.ServiceName
			if service == "" {
				service = "sparkroute"
			}
			exporter, err := otlpexport.New(options.Context, otlpexport.Options{ServiceName: service, ServiceVersion: version.Version, TraceEndpoint: t.Endpoint, TraceHeaders: headers, SampleRatio: t.SampleRatio})
			if err != nil {
				closeAll()
				return nil, err
			}
			cleanup = append(cleanup, func() { _ = closeTelemetry(options.Logger, exporter) })
			runtime.TracerProvider = exporter.TracerProvider
			runtime.Propagator = exporter.Propagator
		}
		options.Telemetry = runtime
	}
	return closeAll, nil
}
