// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.

package modelrouter

import "time"

// This file owns concurrency-safe runtime model observations. Durable audit
// records, rather than a duplicate in-memory decision ring, retain route traces.

func (r *RouterManager) Begin(model string) *RouteHandle {
	r.observationMu.Lock()
	o := r.observations[model]
	o.Inflight++
	o.Requests++
	r.observations[model] = o
	r.observationMu.Unlock()
	return &RouteHandle{Model: model, started: time.Now()}
}
func (r *RouterManager) End(h *RouteHandle, status int) {
	if h == nil {
		return
	}
	h.once.Do(func() { r.Observe(h.Model, time.Since(h.started), status) })
}
func (r *RouterManager) Observe(model string, latency time.Duration, status int) {
	r.observationMu.Lock()
	defer r.observationMu.Unlock()
	o := r.observations[model]
	if o.Inflight > 0 {
		o.Inflight--
	}
	ms := float64(latency) / float64(time.Millisecond)
	if o.EWMALatencyMS == 0 {
		o.EWMALatencyMS = ms
	} else {
		o.EWMALatencyMS = .8*o.EWMALatencyMS + .2*ms
	}
	o.LastStatus = status
	if status >= 400 || status == 0 {
		o.Errors++
	}
	r.observations[model] = o
}

// ObserveRoute records one complete gateway request for fastest/balanced
// selection. It does not conflate individual provider retries with the logical
// model's caller-visible result.
func (r *RouterManager) ObserveRoute(model string, latency time.Duration, status int) {
	r.observationMu.Lock()
	o := r.observations[model]
	o.Requests++
	ms := float64(latency) / float64(time.Millisecond)
	if o.EWMALatencyMS == 0 {
		o.EWMALatencyMS = ms
	} else {
		o.EWMALatencyMS = .8*o.EWMALatencyMS + .2*ms
	}
	o.LastStatus = status
	if status >= 400 || status == 0 {
		o.Errors++
	}
	r.observations[model] = o
	r.observationMu.Unlock()
}
func (r *RouterManager) State() RouterState {
	return RouterState{Policy: r.Policy(), Models: r.Observations()}
}

func (r *RouterManager) Observations() map[string]ModelObservation {
	r.observationMu.RLock()
	obs := make(map[string]ModelObservation, len(r.observations))
	for k, v := range r.observations {
		obs[k] = v
	}
	r.observationMu.RUnlock()
	return obs
}
