// Package file resolves credentials from read-only files below allowlisted
// roots. Each resolution opens the current path, supporting atomic replacement
// and Kubernetes-style projected-volume symlink rotation.
package file

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/sparksq/sparkroute/pkg/credentials"
)

const (
	defaultMaxBytes = int64(1 << 20)
	maxMaxBytes     = int64(16 << 20)
)

type Options struct {
	Roots          []string
	MaxBytes       int64
	AllowGroupRead bool
}

type Source struct {
	roots          []string
	maxBytes       int64
	allowGroupRead bool
}

func New(options Options) (*Source, error) {
	if len(options.Roots) == 0 {
		return nil, fmt.Errorf("at least one credential file root is required")
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = defaultMaxBytes
	}
	if options.MaxBytes < 0 || options.MaxBytes > maxMaxBytes {
		return nil, fmt.Errorf(
			"credential file maximum bytes must be between 1 and %d",
			maxMaxBytes,
		)
	}
	roots := make([]string, 0, len(options.Roots))
	seen := make(map[string]struct{}, len(options.Roots))
	for _, root := range options.Roots {
		if root == "" {
			return nil, fmt.Errorf("credential file root must not be empty")
		}
		absolute, err := filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("resolve credential file root %q: %w", root, err)
		}
		resolved, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return nil, fmt.Errorf("resolve credential file root %q: %w", root, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, fmt.Errorf("inspect credential file root %q: %w", root, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("credential file root %q is not a directory", root)
		}
		if info.Mode().Perm()&0o022 != 0 {
			return nil, fmt.Errorf(
				"credential file root %q must not be group- or other-writable",
				root,
			)
		}
		resolved = filepath.Clean(resolved)
		if _, exists := seen[resolved]; exists {
			continue
		}
		seen[resolved] = struct{}{}
		roots = append(roots, resolved)
	}
	return &Source{
		roots:          roots,
		maxBytes:       options.MaxBytes,
		allowGroupRead: options.AllowGroupRead,
	}, nil
}

func (s *Source) Resolve(
	ctx context.Context,
	ref credentials.Ref,
) (credentials.Material, error) {
	if err := ctx.Err(); err != nil {
		return credentials.Material{}, err
	}
	path, field, err := parseReference(ref)
	if err != nil {
		return credentials.Material{}, err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return credentials.Material{}, fmt.Errorf("resolve credential file path: %w", err)
	}
	resolved = filepath.Clean(resolved)
	if !s.allowed(resolved) {
		return credentials.Material{}, fmt.Errorf(
			"credential file path is outside configured roots",
		)
	}
	handle, err := os.Open(resolved)
	if err != nil {
		return credentials.Material{}, fmt.Errorf("open credential file: %w", err)
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		return credentials.Material{}, fmt.Errorf("inspect credential file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return credentials.Material{}, fmt.Errorf("credential file is not a regular file")
	}
	if err := s.validatePermissions(info.Mode().Perm()); err != nil {
		return credentials.Material{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(handle, s.maxBytes+1))
	if err != nil {
		return credentials.Material{}, fmt.Errorf("read credential file: %w", err)
	}
	if int64(len(raw)) > s.maxBytes {
		return credentials.Material{}, fmt.Errorf(
			"credential file exceeds %d bytes",
			s.maxBytes,
		)
	}
	if err := ctx.Err(); err != nil {
		return credentials.Material{}, err
	}
	value := raw
	if field != "" {
		value, err = selectField(raw, field)
		if err != nil {
			return credentials.Material{}, err
		}
	}
	if len(value) == 0 {
		return credentials.Material{}, fmt.Errorf("credential file value is empty")
	}
	digest := sha256.Sum256(raw)
	return credentials.Material{
		Value:   append([]byte(nil), value...),
		Version: hex.EncodeToString(digest[:]),
	}, nil
}

func parseReference(ref credentials.Ref) (string, string, error) {
	if err := ref.Validate(); err != nil {
		return "", "", err
	}
	parsed, err := url.Parse(string(ref))
	if err != nil {
		return "", "", fmt.Errorf("parse credential file reference: %w", err)
	}
	if !strings.EqualFold(parsed.Scheme, "file") ||
		parsed.Host != "" ||
		parsed.User != nil ||
		parsed.RawQuery != "" ||
		parsed.Opaque != "" {
		return "", "", fmt.Errorf("unsupported credential file reference")
	}
	if !filepath.IsAbs(parsed.Path) {
		return "", "", fmt.Errorf("credential file path must be absolute")
	}
	if strings.ContainsRune(parsed.Path, 0) {
		return "", "", fmt.Errorf("credential file path contains a null byte")
	}
	if len(parsed.Fragment) > 256 || strings.ContainsRune(parsed.Fragment, 0) {
		return "", "", fmt.Errorf("credential file field is invalid")
	}
	return filepath.Clean(parsed.Path), parsed.Fragment, nil
}

func (s *Source) allowed(path string) bool {
	for _, root := range s.roots {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			continue
		}
		if relative != ".." &&
			!strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func (s *Source) validatePermissions(mode os.FileMode) error {
	if mode&0o007 != 0 || mode&0o020 != 0 || mode&0o010 != 0 {
		return fmt.Errorf(
			"credential file must not grant access to others or group write/execute",
		)
	}
	if !s.allowGroupRead && mode&0o040 != 0 {
		return fmt.Errorf("credential file must not grant group read permission")
	}
	return nil
}

func selectField(raw []byte, field string) ([]byte, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("decode credential file JSON: %w", err)
	}
	selected, exists := document[field]
	if !exists {
		return nil, fmt.Errorf("credential file field %q is not present", field)
	}
	var value string
	if err := json.Unmarshal(selected, &value); err != nil {
		return nil, fmt.Errorf("credential file field %q must be a string", field)
	}
	return []byte(value), nil
}

var _ credentials.Source = (*Source)(nil)
