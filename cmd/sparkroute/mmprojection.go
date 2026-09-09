// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/credentials"
	"github.com/sparksq/sparkroute/pkg/mmprojection"
)

func configureGenerationMMProjection(document config.Document, options *runtimeBuildOptions, source credentials.Source) (func(), error) {
	settings := document.MMProjection
	if settings == nil {
		return func() {}, nil // Startup client is owned by the process.
	}
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	options.MMProjection = nil
	options.MMProjectionCredentials = nil // Document references use the generation registry.
	if !settings.Enabled {
		return func() {}, nil
	}
	client, err := mmprojection.New(mmprojection.Options{
		URL: settings.URL, TokenReference: settings.TokenRef, Credentials: source,
		DefaultAnalyzerModel: settings.AnalyzerModel,
		DefaultTimeout:       time.Duration(settings.TimeoutMS) * time.Millisecond,
	})
	if err != nil {
		return nil, err
	}
	options.MMProjection = client
	return client.CloseIdleConnections, nil
}
