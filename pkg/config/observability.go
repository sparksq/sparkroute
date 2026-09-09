// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

// Observability overrides process defaults only for explicitly configured signals.
// Header values remain in the gateway environment, never in configuration.
type Observability struct {
	SavedTraces *SavedTraceConfig `json:"saved_traces,omitempty"`
	OTLPTraces  *OTLPTraceConfig  `json:"otlp_traces,omitempty"`
}
type SavedTraceConfig struct {
	Enabled       bool   `json:"enabled"`
	Storage       string `json:"storage,omitempty"`
	Path          string `json:"path,omitempty"`
	MaxBodyBytes  int64  `json:"max_body_bytes,omitempty"`
	QueueCapacity int    `json:"queue_capacity,omitempty"`
	Overflow      string `json:"overflow,omitempty"`
}
type OTLPTraceConfig struct {
	Enabled     bool              `json:"enabled"`
	Endpoint    string            `json:"endpoint,omitempty"`
	ServiceName string            `json:"service_name,omitempty"`
	SampleRatio *float64          `json:"sample_ratio,omitempty"`
	HeaderEnv   map[string]string `json:"header_env,omitempty"`
}

var traceEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var traceHeaderName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

func (o *Observability) Validate() error {
	if o == nil {
		return nil
	}
	if s := o.SavedTraces; s != nil {
		if s.Storage != "" && s.Storage != "database" && s.Storage != "filesystem" {
			return fmt.Errorf("saved_traces.storage must be database or filesystem")
		}
		if s.Enabled && (s.Storage == "" || !filepath.IsAbs(s.Path)) {
			return fmt.Errorf("saved_traces requires a storage type and an absolute path on the gateway host")
		}
		if s.MaxBodyBytes < 0 || s.MaxBodyBytes > 256<<20 {
			return fmt.Errorf("saved_traces.max_body_bytes must be between 0 (default) and 268435456")
		}
		if s.QueueCapacity < 0 || s.QueueCapacity > 65536 {
			return fmt.Errorf("saved_traces.queue_capacity must be between 0 (default) and 65536")
		}
		if s.Overflow != "" && s.Overflow != "block" && s.Overflow != "drop" {
			return fmt.Errorf("saved_traces.overflow must be block or drop")
		}
	}
	if t := o.OTLPTraces; t != nil {
		if t.Enabled || t.Endpoint != "" {
			u, err := url.Parse(t.Endpoint)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return fmt.Errorf("otlp_traces.endpoint must be an HTTP(S) URL without credentials, query, or fragment")
			}
		}
		if t.SampleRatio != nil && (*t.SampleRatio < 0 || *t.SampleRatio > 1) {
			return fmt.Errorf("otlp_traces.sample_ratio must be between 0 and 1")
		}
		if len(t.ServiceName) > 256 || strings.ContainsAny(t.ServiceName, "\r\n\x00") {
			return fmt.Errorf("otlp_traces.service_name is invalid")
		}
		if len(t.HeaderEnv) > 32 {
			return fmt.Errorf("otlp_traces.header_env accepts at most 32 headers")
		}
		seen := map[string]bool{}
		for name, env := range t.HeaderEnv {
			lower := strings.ToLower(name)
			if !traceHeaderName.MatchString(name) || !traceEnvName.MatchString(env) || seen[lower] {
				return fmt.Errorf("otlp_traces.header_env requires distinct HTTP headers mapped to environment variable names")
			}
			switch lower {
			case "host", "content-length", "transfer-encoding", "connection", "content-type":
				return fmt.Errorf("otlp_traces.header_env cannot override transport headers")
			}
			seen[lower] = true
		}
	}
	return nil
}
