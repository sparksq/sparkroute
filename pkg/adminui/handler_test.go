// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package adminui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestEmbeddedLicenseNotices(t *testing.T) {
	handler, err := NewEmbedded("/admin")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/admin/legal.html", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("license page status = %d", response.Code)
	}
	for _, notice := range []string{"GNU AFFERO GENERAL PUBLIC LICENSE", "npm:react", "modernc.org/libc", "github.com/scitrera/go-llm", "Permission is hereby granted"} {
		if !strings.Contains(response.Body.String(), notice) {
			t.Errorf("embedded license page missing %q", notice)
		}
	}
}

func TestHandlerServesAssetsAndSPAFallback(t *testing.T) {
	t.Parallel()

	handler, err := New(fstest.MapFS{
		"index.html":    {Data: []byte("<title>Admin</title>")},
		"assets/app.js": {Data: []byte("console.log('admin')")},
	}, "/admin")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	for _, test := range []struct {
		path        string
		status      int
		contains    string
		cachePrefix string
	}{
		{"/admin/", http.StatusOK, "<title>Admin</title>", "no-store"},
		{"/admin/requests/one", http.StatusOK, "<title>Admin</title>", "no-store"},
		{"/admin/assets/app.js", http.StatusOK, "console.log", "public"},
		{"/admin/assets/missing.js", http.StatusNotFound, "", ""},
		{"/other", http.StatusNotFound, "", ""},
	} {
		test := test
		t.Run(test.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(
				response,
				httptest.NewRequest(http.MethodGet, test.path, nil),
			)
			if response.Code != test.status ||
				(test.contains != "" &&
					!strings.Contains(response.Body.String(), test.contains)) ||
				(test.cachePrefix != "" &&
					!strings.HasPrefix(
						response.Header().Get("Cache-Control"),
						test.cachePrefix,
					)) {
				t.Fatalf(
					"response = %d headers=%#v body=%q",
					response.Code,
					response.Header(),
					response.Body,
				)
			}
			if response.Code == http.StatusOK &&
				!strings.Contains(
					response.Header().Get("Content-Security-Policy"),
					"frame-ancestors 'none'",
				) {
				t.Fatalf("security headers = %#v", response.Header())
			}
		})
	}
}

func TestHandlerRedirectsMountAndRejectsWrites(t *testing.T) {
	t.Parallel()

	assets := fstest.MapFS{
		"index.html": {Data: []byte("index")},
	}
	handler, err := New(fs.FS(assets), "/admin")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	redirect := httptest.NewRecorder()
	handler.ServeHTTP(
		redirect,
		httptest.NewRequest(http.MethodGet, "/admin?view=targets", nil),
	)
	if redirect.Code != http.StatusPermanentRedirect ||
		redirect.Header().Get("Location") != "/admin/?view=targets" {
		t.Fatalf("redirect = %d %#v", redirect.Code, redirect.Header())
	}

	write := httptest.NewRecorder()
	handler.ServeHTTP(
		write,
		httptest.NewRequest(http.MethodPost, "/admin/", nil),
	)
	if write.Code != http.StatusMethodNotAllowed ||
		write.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("write = %d %#v", write.Code, write.Header())
	}
}

func TestEmbeddedConsoleIsAvailable(t *testing.T) {
	t.Parallel()

	handler, err := NewEmbedded("/admin")
	if err != nil {
		t.Fatalf("NewEmbedded() error = %v", err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/admin/", nil),
	)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "sparkroute-admin-root") {
		t.Fatalf("embedded response = %d body=%q", response.Code, response.Body.String())
	}
}
