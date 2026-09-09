// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Package adminui serves a compiled single-page administration application on
// a caller-selected URL prefix.
package adminui

import (
	"bytes"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

type handler struct {
	assets fs.FS
	mount  string
	index  []byte
}

// New constructs a static SPA handler. Assets must expose index.html at their
// root. Mount is an absolute URL path such as /admin.
func New(assets fs.FS, mount string) (http.Handler, error) {
	mount = strings.TrimRight(mount, "/")
	if mount == "" || mount[0] != '/' || strings.Contains(mount, "..") {
		return nil, fmt.Errorf("admin UI mount must be a non-root absolute path")
	}
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		return nil, fmt.Errorf("read admin UI index: %w", err)
	}
	return &handler{assets: assets, mount: mount, index: index}, nil
}

func (h *handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD")
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if request.URL.Path == h.mount {
		target := h.mount + "/"
		if request.URL.RawQuery != "" {
			target += "?" + request.URL.RawQuery
		}
		http.Redirect(writer, request, target, http.StatusPermanentRedirect)
		return
	}
	prefix := h.mount + "/"
	if !strings.HasPrefix(request.URL.Path, prefix) {
		http.NotFound(writer, request)
		return
	}
	relative := strings.TrimPrefix(request.URL.Path, prefix)
	clean := path.Clean("/" + relative)
	if strings.Contains(relative, "\\") || strings.Contains(clean, "..") {
		http.NotFound(writer, request)
		return
	}
	name := strings.TrimPrefix(clean, "/")
	if name == "" {
		h.serveIndex(writer, request)
		return
	}
	content, err := fs.ReadFile(h.assets, name)
	if err == nil {
		h.serveAsset(writer, request, name, content)
		return
	}
	if path.Ext(name) != "" {
		http.NotFound(writer, request)
		return
	}
	h.serveIndex(writer, request)
}

func (h *handler) serveIndex(
	writer http.ResponseWriter,
	request *http.Request,
) {
	h.securityHeaders(writer.Header())
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.serveContent(writer, request, "index.html", h.index)
}

func (h *handler) serveAsset(
	writer http.ResponseWriter,
	request *http.Request,
	name string,
	content []byte,
) {
	h.securityHeaders(writer.Header())
	if strings.HasPrefix(name, "assets/") {
		writer.Header().Set(
			"Cache-Control",
			"public, max-age=31536000, immutable",
		)
	} else {
		writer.Header().Set("Cache-Control", "no-cache")
	}
	if contentType := mime.TypeByExtension(path.Ext(name)); contentType != "" {
		writer.Header().Set("Content-Type", contentType)
	}
	h.serveContent(writer, request, name, content)
}

func (h *handler) serveContent(
	writer http.ResponseWriter,
	request *http.Request,
	name string,
	content []byte,
) {
	http.ServeContent(
		writer,
		request,
		name,
		time.Time{},
		bytes.NewReader(content),
	)
}

func (h *handler) securityHeaders(header http.Header) {
	header.Set(
		"Content-Security-Policy",
		"default-src 'self'; base-uri 'none'; connect-src 'self'; "+
			"font-src 'self'; form-action 'self'; frame-ancestors 'none'; "+
			"img-src 'self' data:; object-src 'none'; script-src 'self'; "+
			"style-src 'self'",
	)
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
}
