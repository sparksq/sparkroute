package kubernetes

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/sparksq/sparkroute/pkg/credentials"
)

func TestReadSecretFetchesOneVersionAndClassifiesNotFound(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.URL.Path != "/api/v1/namespaces/mt/secrets/aether-creds" {
			http.NotFound(writer, request)
			return
		}
		_, _ = fmt.Fprint(writer, `{"metadata":{"resourceVersion":"7"},"data":{"tls.crt":"Y2VydA==","tls.key":"a2V5","ca.crt":"Y2E="}}`)
	}))
	defer server.Close()
	tokenPath := filepath.Join(t.TempDir(), "token")
	writeToken(t, tokenPath, "token")
	source, err := New(Options{
		APIURL: server.URL, AllowedNamespaces: []string{"mt"}, TokenFile: tokenPath,
		HTTPClient: server.Client(), AllowInsecureHTTP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	secret, err := source.ReadSecret(t.Context(), "mt", "aether-creds")
	if err != nil || secret.ResourceVersion != "7" || string(secret.Data["tls.key"]) != "key" || requests.Load() != 1 {
		t.Fatalf("ReadSecret() = %#v, %v; requests=%d", secret, err, requests.Load())
	}
	if _, err := source.ReadSecret(t.Context(), "mt", "missing"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("missing ReadSecret() error = %v", err)
	}
}

func TestSourceResolvesRotationAndTokenReplacement(t *testing.T) {
	t.Parallel()

	var generation atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		current := generation.Load()
		expectedToken := fmt.Sprintf("token-%d", current)
		if request.Header.Get("Authorization") != "Bearer "+expectedToken {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if request.URL.Path != "/api/v1/namespaces/gateway/secrets/providers" {
			t.Errorf("path = %q", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = fmt.Fprintf(
			writer,
			`{"metadata":{"resourceVersion":"%d"},"data":{"api-key":"%s"}}`,
			current,
			base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("secret-%d", current))),
		)
	}))
	defer server.Close()

	tokenPath := filepath.Join(t.TempDir(), "token")
	writeToken(t, tokenPath, "token-1")
	generation.Store(1)
	source, err := New(Options{
		APIURL:            server.URL,
		AllowedNamespaces: []string{"gateway"},
		TokenFile:         tokenPath,
		HTTPClient:        server.Client(),
		AllowInsecureHTTP: true,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ref := credentials.Ref("k8s://gateway/providers#api-key")
	first, err := source.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve() first error = %v", err)
	}
	writeToken(t, tokenPath, "token-2")
	generation.Store(2)
	second, err := source.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve() second error = %v", err)
	}
	if string(first.Value) != "secret-1" ||
		first.Version != "1" ||
		string(second.Value) != "secret-2" ||
		second.Version != "2" {
		t.Fatalf("rotation = first:%#v second:%#v", first, second)
	}
}

func TestSourceRejectsNamespaceAndRedirects(t *testing.T) {
	t.Parallel()

	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		redirected.Store(true)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		http.Redirect(writer, request, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	tokenPath := filepath.Join(t.TempDir(), "token")
	writeToken(t, tokenPath, "token")
	source, err := New(Options{
		APIURL:            redirector.URL,
		AllowedNamespaces: []string{"gateway"},
		TokenFile:         tokenPath,
		HTTPClient:        redirector.Client(),
		AllowInsecureHTTP: true,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := source.Resolve(
		context.Background(),
		credentials.Ref("k8s://other/providers#api-key"),
	); err == nil {
		t.Fatal("Resolve() namespace error = nil")
	}
	if _, err := source.Resolve(
		context.Background(),
		credentials.Ref("k8s://gateway/providers#api-key"),
	); err == nil {
		t.Fatal("Resolve() redirect error = nil")
	}
	if redirected.Load() {
		t.Fatal("Kubernetes client followed redirect")
	}
}

func TestSourceRejectsMalformedReferences(t *testing.T) {
	t.Parallel()

	for _, ref := range []credentials.Ref{
		"k8s://Gateway/providers#api-key",
		"k8s://gateway/bad/name#api-key",
		"k8s://gateway/providers",
		"k8s://gateway/providers#bad/key",
	} {
		if _, _, _, err := parseReference(ref); err == nil {
			t.Fatalf("parseReference(%q) error = nil", ref)
		}
	}
}

func writeToken(t *testing.T, path, value string) {
	t.Helper()
	replacement := path + ".next"
	if err := os.WriteFile(replacement, []byte(value), 0o600); err != nil {
		t.Fatalf("WriteFile() token error = %v", err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("Rename() token error = %v", err)
	}
}
