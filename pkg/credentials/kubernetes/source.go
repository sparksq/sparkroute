// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Package kubernetes resolves credentials from the Kubernetes Secret API
// without pulling the full client-go dependency graph into the gateway.
package kubernetes

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/sparksq/sparkroute/pkg/credentials"
)

var ErrSecretNotFound = errors.New("the Kubernetes Secret was not found")

// Secret is one immutable Kubernetes Secret API projection. Callers should
// treat Data as sensitive and clear values when they are no longer needed.
type Secret struct {
	Data            map[string][]byte
	ResourceVersion string
}

const (
	defaultAPIURL            = "https://kubernetes.default.svc"
	defaultServiceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	defaultMaxResponseBytes  = int64(2 << 20)
	maxMaxResponseBytes      = int64(16 << 20)
	maxTokenBytes            = int64(1 << 20)
)

type Options struct {
	APIURL            string
	AllowedNamespaces []string
	DefaultNamespace  string
	NamespaceFile     string
	TokenFile         string
	CAFile            string
	HTTPClient        *http.Client
	MaxResponseBytes  int64
	AllowInsecureHTTP bool
}

type Source struct {
	baseURL           *url.URL
	allowedNamespaces map[string]struct{}
	tokenFile         string
	client            *http.Client
	maxResponseBytes  int64
}

func New(options Options) (*Source, error) {
	if options.APIURL == "" {
		options.APIURL = defaultAPIURL
	}
	if options.NamespaceFile == "" {
		options.NamespaceFile = path.Join(defaultServiceAccountDir, "namespace")
	}
	if options.TokenFile == "" {
		options.TokenFile = path.Join(defaultServiceAccountDir, "token")
	}
	if options.CAFile == "" {
		options.CAFile = path.Join(defaultServiceAccountDir, "ca.crt")
	}
	if options.MaxResponseBytes == 0 {
		options.MaxResponseBytes = defaultMaxResponseBytes
	}
	if options.MaxResponseBytes < 0 ||
		options.MaxResponseBytes > maxMaxResponseBytes {
		return nil, fmt.Errorf(
			"the Kubernetes Secret maximum response bytes must be between 1 and %d",
			maxMaxResponseBytes,
		)
	}
	baseURL, err := url.Parse(options.APIURL)
	if err != nil {
		return nil, fmt.Errorf("parse Kubernetes API URL: %w", err)
	}
	if baseURL.Scheme != "https" &&
		(baseURL.Scheme != "http" || !options.AllowInsecureHTTP) {
		return nil, fmt.Errorf("the Kubernetes API URL must use HTTPS")
	}
	if baseURL.Host == "" ||
		baseURL.User != nil ||
		baseURL.RawQuery != "" ||
		baseURL.Fragment != "" {
		return nil, fmt.Errorf("the Kubernetes API URL must be an origin URL")
	}
	baseURL.Path = strings.TrimSuffix(baseURL.Path, "/")

	allowed := make(map[string]struct{}, len(options.AllowedNamespaces)+1)
	for _, namespace := range options.AllowedNamespaces {
		if err := validateDNSLabel("namespace", namespace, 63, false); err != nil {
			return nil, err
		}
		allowed[namespace] = struct{}{}
	}
	if options.DefaultNamespace != "" {
		if err := validateDNSLabel(
			"default namespace",
			options.DefaultNamespace,
			63,
			false,
		); err != nil {
			return nil, err
		}
		allowed[options.DefaultNamespace] = struct{}{}
	}
	if len(allowed) == 0 {
		rawNamespace, err := os.ReadFile(options.NamespaceFile)
		if err != nil {
			return nil, fmt.Errorf("read Kubernetes service-account namespace: %w", err)
		}
		namespace := strings.TrimSpace(string(rawNamespace))
		if err := validateDNSLabel("service-account namespace", namespace, 63, false); err != nil {
			return nil, err
		}
		allowed[namespace] = struct{}{}
	}

	client := options.HTTPClient
	if client == nil {
		certificate, err := os.ReadFile(options.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read Kubernetes service-account CA: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(certificate) {
			return nil, fmt.Errorf("the Kubernetes service-account CA contains no certificates")
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
		}
		client = &http.Client{Transport: transport}
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(
		_ *http.Request,
		_ []*http.Request,
	) error {
		return fmt.Errorf("the Kubernetes Secret API redirects are disabled")
	}

	return &Source{
		baseURL:           baseURL,
		allowedNamespaces: allowed,
		tokenFile:         options.TokenFile,
		client:            &clientCopy,
		maxResponseBytes:  options.MaxResponseBytes,
	}, nil
}

func (s *Source) Resolve(
	ctx context.Context,
	ref credentials.Ref,
) (credentials.Material, error) {
	namespace, secret, key, err := parseReference(ref)
	if err != nil {
		return credentials.Material{}, err
	}
	payload, err := s.ReadSecret(ctx, namespace, secret)
	if err != nil {
		return credentials.Material{}, err
	}
	value, exists := payload.Data[key]
	if !exists {
		return credentials.Material{}, fmt.Errorf(
			"the Kubernetes Secret data key %q is not present",
			key,
		)
	}
	if len(value) == 0 {
		return credentials.Material{}, fmt.Errorf(
			"the Kubernetes Secret data key %q is empty",
			key,
		)
	}
	return credentials.Material{
		Value:   append([]byte(nil), value...),
		Version: payload.ResourceVersion,
	}, nil
}

// ReadSecret fetches one allowlisted Secret in one API request. It exists for
// consumers such as mTLS client factories that must observe a certificate,
// key, and CA from the same Kubernetes resource version.
func (s *Source) ReadSecret(
	ctx context.Context,
	namespace string,
	secret string,
) (Secret, error) {
	if err := validateDNSLabel("namespace", namespace, 63, false); err != nil {
		return Secret{}, err
	}
	if err := validateDNSLabel("Secret name", secret, 253, true); err != nil {
		return Secret{}, err
	}
	if _, allowed := s.allowedNamespaces[namespace]; !allowed {
		return Secret{}, fmt.Errorf(
			"the Kubernetes Secret namespace %q is not allowed",
			namespace,
		)
	}
	token, err := readToken(s.tokenFile)
	if err != nil {
		return Secret{}, err
	}
	endpoint := *s.baseURL
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") +
		"/api/v1/namespaces/" + url.PathEscape(namespace) +
		"/secrets/" + url.PathEscape(secret)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return Secret{}, fmt.Errorf("construct Kubernetes Secret request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := s.client.Do(request)
	if err != nil {
		return Secret{}, fmt.Errorf("read Kubernetes Secret: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound {
		return Secret{}, ErrSecretNotFound
	}
	if response.StatusCode != http.StatusOK {
		return Secret{}, fmt.Errorf(
			"read Kubernetes Secret: API returned status %d",
			response.StatusCode,
		)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, s.maxResponseBytes+1))
	if err != nil {
		return Secret{}, fmt.Errorf("read Kubernetes Secret response: %w", err)
	}
	if int64(len(raw)) > s.maxResponseBytes {
		return Secret{}, fmt.Errorf(
			"the Kubernetes Secret response exceeds %d bytes",
			s.maxResponseBytes,
		)
	}
	var payload struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Data map[string][]byte `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Secret{}, fmt.Errorf("decode Kubernetes Secret response: %w", err)
	}
	data := make(map[string][]byte, len(payload.Data))
	for key, value := range payload.Data {
		data[key] = append([]byte(nil), value...)
	}
	return Secret{Data: data, ResourceVersion: payload.Metadata.ResourceVersion}, nil
}

func parseReference(ref credentials.Ref) (string, string, string, error) {
	if err := ref.Validate(); err != nil {
		return "", "", "", err
	}
	parsed, err := url.Parse(string(ref))
	if err != nil {
		return "", "", "", fmt.Errorf("parse Kubernetes Secret reference: %w", err)
	}
	if !strings.EqualFold(parsed.Scheme, "k8s") ||
		parsed.User != nil ||
		parsed.RawQuery != "" ||
		parsed.Opaque != "" {
		return "", "", "", fmt.Errorf("unsupported Kubernetes Secret reference")
	}
	namespace := parsed.Host
	secret := strings.TrimPrefix(parsed.Path, "/")
	key := parsed.Fragment
	if err := validateDNSLabel("namespace", namespace, 63, false); err != nil {
		return "", "", "", err
	}
	if err := validateDNSLabel("Secret name", secret, 253, true); err != nil {
		return "", "", "", err
	}
	if err := validateDataKey(key); err != nil {
		return "", "", "", err
	}
	return namespace, secret, key, nil
}

func readToken(tokenFile string) (string, error) {
	handle, err := os.Open(tokenFile)
	if err != nil {
		return "", fmt.Errorf("open Kubernetes service-account token: %w", err)
	}
	defer func() { _ = handle.Close() }()
	raw, err := io.ReadAll(io.LimitReader(handle, maxTokenBytes+1))
	if err != nil {
		return "", fmt.Errorf("read Kubernetes service-account token: %w", err)
	}
	if len(raw) > int(maxTokenBytes) {
		return "", fmt.Errorf("the Kubernetes service-account token is too large")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("the Kubernetes service-account token is empty")
	}
	return token, nil
}

func validateDNSLabel(name, value string, limit int, allowDot bool) error {
	if value == "" || len(value) > limit {
		return fmt.Errorf("%s is invalid", name)
	}
	labels := []string{value}
	if allowDot {
		labels = strings.Split(value, ".")
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 ||
			!isASCIILowerOrDigit(label[0]) ||
			!isASCIILowerOrDigit(label[len(label)-1]) {
			return fmt.Errorf("%s %q is invalid", name, value)
		}
		for _, character := range []byte(label) {
			if isASCIILowerOrDigit(character) || character == '-' {
				continue
			}
			return fmt.Errorf("%s %q is invalid", name, value)
		}
	}
	return nil
}

func validateDataKey(value string) error {
	if value == "" || len(value) > 253 {
		return fmt.Errorf("the Kubernetes Secret data key is invalid")
	}
	for _, character := range []byte(value) {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '-' ||
			character == '_' ||
			character == '.' {
			continue
		}
		return fmt.Errorf("the Kubernetes Secret data key %q is invalid", value)
	}
	return nil
}

func isASCIILowerOrDigit(character byte) bool {
	return character >= 'a' && character <= 'z' ||
		character >= '0' && character <= '9'
}

var _ credentials.Source = (*Source)(nil)
