// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sparksq/sparkroute/internal/privatepath"
	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/pii"
	piisqlite "github.com/sparksq/sparkroute/pkg/pii/sqlite"
)

// One encrypted mapping directory is shared by runtime generations. Opening
// it lazily keeps request-scoped policies and configuration checks disk-free.
type privacyRuntime struct {
	mu            sync.Mutex
	ctx           context.Context
	path, keyPath string
	store         *piisqlite.Store
	directory     *pii.Directory
	cancel        context.CancelFunc
}

func (p *privacyRuntime) ForDocument(document config.Document) (*pii.Provider, error) {
	resolved, err := document.ResolveModelPolicies()
	if err != nil {
		return nil, err
	}
	document = resolved
	if p == nil {
		return pii.NewProvider(document, pii.NewBuiltinDetector(), nil)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	needsDirectory := false
	for _, model := range document.VirtualModels {
		if model.Privacy != nil {
			policy := model.Privacy.PII.Effective()
			needsDirectory = needsDirectory || policy.Mode != config.PIIModeDisabled && policy.Scope == config.PIIScopeConversation
		}
	}
	if needsDirectory && p.directory == nil {
		if p.path == "" || p.path == ":memory:" {
			return nil, fmt.Errorf("conversation-scoped PII requires a persistent -pii-sqlite path")
		}
		keyPath := p.keyPath
		if keyPath == "" {
			keyPath = filepath.Join(filepath.Dir(p.path), "keyring.json")
		}
		keyring, err := loadPIIKeyring(keyPath, p.path, p.keyPath == "")
		if err != nil {
			return nil, err
		}
		store, err := piisqlite.Open(p.ctx, piisqlite.Options{Path: p.path})
		if err != nil {
			return nil, err
		}
		directory, err := pii.NewDirectory(store, keyring, pii.DirectoryOptions{})
		if err != nil {
			_ = store.Close()
			return nil, err
		}
		p.store, p.directory = store, directory
		ctx, cancel := context.WithCancel(p.ctx)
		p.cancel = cancel
		go directory.RunPruner(ctx, time.Minute, 1000, nil)
	}
	return pii.NewProvider(document, pii.NewBuiltinDetector(), p.directory)
}

func (p *privacyRuntime) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
	}
	if p.store != nil {
		return p.store.Close()
	}
	return nil
}

func loadPIIKeyring(path, database string, create bool) (*pii.Keyring, error) {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) && create {
		// Never generate a replacement key for an existing encrypted store.
		if info, dbErr := os.Stat(database); dbErr == nil && info.Size() > 0 {
			return nil, fmt.Errorf("PII keyring is missing for an existing mapping database; restore its original keyring")
		} else if dbErr != nil && !errors.Is(dbErr, os.ErrNotExist) {
			return nil, dbErr
		}
		parent := filepath.Dir(path)
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return nil, err
		}
		info, err := os.Stat(parent)
		if err != nil {
			return nil, err
		}
		if err := privatepath.Check(parent, info.Mode()); err != nil {
			return nil, fmt.Errorf("PII key directory: %w", err)
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		defer clear(key)
		file, err := os.CreateTemp(parent, ".pii-key-*")
		if err != nil {
			return nil, err
		}
		defer func() { _ = os.Remove(file.Name()) }()
		_, writeErr := io.WriteString(file, base64.StdEncoding.EncodeToString(key))
		if writeErr == nil {
			writeErr = file.Sync()
		}
		closeErr := file.Close()
		if writeErr != nil {
			return nil, writeErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		// Linking a complete file publishes it atomically without overwriting a
		// key another process may have created concurrently.
		if err := os.Link(file.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("open PII keyring: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return nil, fmt.Errorf("PII keyring must be a regular file of at most 64 KiB")
	}
	if err := privatepath.Check(path, info.Mode()); err != nil {
		return nil, fmt.Errorf("PII keyring: %w", err)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	material, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	defer clear(material)
	if err != nil {
		return nil, err
	}
	if len(material) > 64<<10 {
		return nil, fmt.Errorf("PII keyring exceeds 64 KiB")
	}
	return pii.ParseKeyring(material)
}
