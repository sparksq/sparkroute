// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package adminui

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
)

//go:embed all:ui
var embeddedUI embed.FS

// NewEmbedded serves the standalone OSS SparkRoute console.
func NewEmbedded(mount string) (http.Handler, error) {
	assets, err := fs.Sub(embeddedUI, "ui")
	if err != nil {
		return nil, fmt.Errorf("sub embedded admin UI assets: %w", err)
	}
	return New(assets, mount)
}
