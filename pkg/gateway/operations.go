// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"net/http"
	stdpprof "net/http/pprof"
)

// NewOperationsHandler composes process health with an explicitly enabled Go
// profiling surface. Profiling is disabled by default because profiles expose
// process internals and consume measurable CPU while being collected.
func NewOperationsHandler(health http.Handler, enablePprof bool) http.Handler {
	if health == nil {
		health = http.NotFoundHandler()
	}
	if !enablePprof {
		return health
	}
	mux := http.NewServeMux()
	mux.Handle("/", health)
	mux.HandleFunc("/debug/pprof/", stdpprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", stdpprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", stdpprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", stdpprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", stdpprof.Trace)
	return mux
}
