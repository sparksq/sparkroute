// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/sparksq/sparkroute/pkg/credentials"
)

// MMProjectionConfig overrides the process connection settings as a unit.
// An omitted object inherits startup settings; Enabled=false explicitly disables
// projection. Routing policies separately decide which requests use the bridge.
type MMProjectionConfig struct {
	Enabled       bool            `json:"enabled"`
	URL           string          `json:"url,omitempty"`
	TokenRef      credentials.Ref `json:"token_ref,omitempty"`
	AnalyzerModel string          `json:"analyzer_model,omitempty"`
	TimeoutMS     int64           `json:"timeout_ms,omitempty"`
}

func (m *MMProjectionConfig) Validate() error {
	if m == nil {
		return nil
	}
	if m.Enabled || m.URL != "" {
		u, err := url.Parse(strings.TrimSpace(m.URL))
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("url must be an HTTP(S) base URL without credentials, query, or fragment")
		}
	}
	if m.Enabled && m.TokenRef == "" {
		return fmt.Errorf("token_ref is required when enabled")
	}
	if m.TokenRef != "" {
		if err := m.TokenRef.Validate(); err != nil {
			return fmt.Errorf("token_ref must be a credential reference, such as env://MMBRIDGE_TOKEN")
		}
	}
	if len(m.AnalyzerModel) > 256 || strings.ContainsFunc(m.AnalyzerModel, func(r rune) bool { return r < 32 || r == 127 }) {
		return fmt.Errorf("analyzer_model must be at most 256 bytes without control characters")
	}
	if m.TimeoutMS != 0 && (m.TimeoutMS < 1000 || m.TimeoutMS > 900000) {
		return fmt.Errorf("timeout_ms must be 0 (10 minute default) or between 1000 and 900000")
	}
	return nil
}
