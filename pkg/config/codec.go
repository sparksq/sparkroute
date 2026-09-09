// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
)

// Decode parses one strict configuration document and validates it. JSON is a
// YAML 1.2 subset and remains the only accepted file encoding in this phase.
func Decode(raw []byte) (Document, error) {
	var document Document
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return Document{}, fmt.Errorf("decode config as JSON/YAML-subset: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Document{}, fmt.Errorf("decode config: multiple documents are not allowed")
		}
		return Document{}, fmt.Errorf("decode trailing config data: %w", err)
	}
	if err := document.Validate(); err != nil {
		return Document{}, fmt.Errorf("validate config: %w", err)
	}
	return document, nil
}

// EncodeCanonical validates a document and returns deterministic JSON plus its
// content-addressed revision. Secret references remain opaque strings.
func EncodeCanonical(document Document) ([]byte, Version, error) {
	if err := document.Validate(); err != nil {
		return nil, "", fmt.Errorf("validate config: %w", err)
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, "", fmt.Errorf("encode canonical config: %w", err)
	}
	digest := sha256.Sum256(raw)
	return raw, Version(hex.EncodeToString(digest[:])), nil
}
