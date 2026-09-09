// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Package file loads immutable gateway configuration from a local file.
package file

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/sparksq/sparkroute/pkg/config"
)

const maxDocumentBytes = 8 << 20

type Source struct {
	Path string
}

func (s Source) Load(ctx context.Context) (config.Document, config.Version, error) {
	if err := ctx.Err(); err != nil {
		return config.Document{}, "", err
	}
	if s.Path == "" {
		return config.Document{}, "", fmt.Errorf("config path is required")
	}
	handle, err := os.Open(s.Path)
	if err != nil {
		return config.Document{}, "", fmt.Errorf("open config: %w", err)
	}
	defer func() { _ = handle.Close() }()

	limited := io.LimitReader(handle, maxDocumentBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return config.Document{}, "", fmt.Errorf("read config: %w", err)
	}
	if len(raw) > maxDocumentBytes {
		return config.Document{}, "", fmt.Errorf("config exceeds %d bytes", maxDocumentBytes)
	}

	document, err := config.Decode(raw)
	if err != nil {
		return config.Document{}, "", err
	}

	digest := sha256.Sum256(raw)
	return document, config.Version(hex.EncodeToString(digest[:])), nil
}
