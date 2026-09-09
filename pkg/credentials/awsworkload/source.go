// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Package awsworkload resolves refreshable AWS workload credentials for
// Bedrock SigV4 authentication. It intentionally implements only non-interactive
// workload sources: environment, EKS web identity, ECS/EKS container
// credentials, and EC2 IMDSv2.
package awsworkload

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/sparksq/sparkroute/pkg/credentials"
)

const (
	maxCredentialResponseBytes = 1 << 20
	maxWorkloadTokenBytes      = 1 << 20
	refreshBeforeExpiry        = 5 * time.Minute
	defaultCredentialTimeout   = 5 * time.Second
)

// SigningMaterial is the private, structured value returned through the
// provider-neutral credential contract. Its JSON encoding is internal to the
// gateway and must never be logged.
type SigningMaterial struct {
	AccessKeyID     string    `json:"access_key_id"`
	SecretAccessKey string    `json:"secret_access_key"`
	SessionToken    string    `json:"session_token,omitempty"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
	Source          string    `json:"source,omitempty"`
}

// DecodeSigningMaterial validates credential material before it reaches the
// signer.
func DecodeSigningMaterial(raw []byte) (SigningMaterial, error) {
	var material SigningMaterial
	if err := json.Unmarshal(raw, &material); err != nil {
		return SigningMaterial{}, fmt.Errorf("decode AWS signing material")
	}
	if material.AccessKeyID == "" || material.SecretAccessKey == "" {
		return SigningMaterial{}, fmt.Errorf("AWS signing material is incomplete")
	}
	if strings.ContainsAny(
		material.AccessKeyID+
			material.SecretAccessKey+
			material.SessionToken,
		"\r\n",
	) {
		return SigningMaterial{}, fmt.Errorf(
			"AWS signing material contains a newline",
		)
	}
	return material, nil
}

type getenvFunc func(string) string
type readFileFunc func(string) ([]byte, error)

// Options exposes deterministic seams for tests. Production callers should use
// zero values so metadata destinations remain fixed and SSRF-resistant.
type Options struct {
	HTTPClient        *http.Client
	Now               func() time.Time
	Getenv            func(string) string
	ReadFile          func(string) ([]byte, error)
	STSEndpoint       string
	ContainerEndpoint string
	IMDSEndpoint      string
}

// Source caches temporary credentials until their refresh window and
// serializes refreshes so concurrent requests do not stampede metadata
// services.
type Source struct {
	client            *http.Client
	metadataClient    *http.Client
	now               func() time.Time
	getenv            getenvFunc
	readFile          readFileFunc
	stsEndpoint       string
	containerEndpoint string
	imdsEndpoint      string

	mu     sync.Mutex
	cached SigningMaterial
}

func New(options Options) *Source {
	client := options.HTTPClient
	metadataClient := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultCredentialTimeout}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		metadataClient = &http.Client{
			Timeout:   defaultCredentialTimeout,
			Transport: transport,
		}
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	metadataClientCopy := *metadataClient
	metadataClientCopy.CheckRedirect = func(
		_ *http.Request,
		_ []*http.Request,
	) error {
		return http.ErrUseLastResponse
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	getenv := options.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	return &Source{
		client:            &clientCopy,
		metadataClient:    &metadataClientCopy,
		now:               now,
		getenv:            getenv,
		readFile:          options.ReadFile,
		stsEndpoint:       options.STSEndpoint,
		containerEndpoint: options.ContainerEndpoint,
		imdsEndpoint:      options.IMDSEndpoint,
	}
}

func (s *Source) Resolve(
	ctx context.Context,
	ref credentials.Ref,
) (credentials.Material, error) {
	if err := validateReference(ref); err != nil {
		return credentials.Material{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	if s.cached.AccessKeyID != "" &&
		(s.cached.ExpiresAt.IsZero() ||
			now.Add(refreshBeforeExpiry).Before(s.cached.ExpiresAt)) {
		return encodeMaterial(s.cached)
	}
	material, err := s.resolve(ctx, now)
	if err != nil {
		return credentials.Material{}, err
	}
	if !material.ExpiresAt.IsZero() &&
		!now.Before(material.ExpiresAt) {
		return credentials.Material{}, fmt.Errorf(
			"AWS workload credentials are expired",
		)
	}
	s.cached = material
	return encodeMaterial(material)
}

func validateReference(ref credentials.Ref) error {
	parsed, err := url.Parse(string(ref))
	if err != nil {
		return fmt.Errorf("parse AWS workload credential reference: %w", err)
	}
	profile := strings.Trim(parsed.Path, "/")
	if !strings.EqualFold(parsed.Scheme, "workload") ||
		!strings.EqualFold(parsed.Host, "aws") ||
		(profile != "" && profile != "default") ||
		parsed.RawQuery != "" ||
		parsed.Fragment != "" {
		return fmt.Errorf(
			"AWS workload credential reference must be workload://aws",
		)
	}
	return nil
}

func encodeMaterial(
	material SigningMaterial,
) (credentials.Material, error) {
	encoded, err := json.Marshal(material)
	if err != nil {
		return credentials.Material{}, fmt.Errorf(
			"encode AWS signing material",
		)
	}
	version := material.Source
	if !material.ExpiresAt.IsZero() {
		version += ":" + material.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return credentials.Material{Value: encoded, Version: version}, nil
}

func (s *Source) resolve(
	ctx context.Context,
	now time.Time,
) (SigningMaterial, error) {
	if material, found, err := s.environment(); found || err != nil {
		return material, err
	}
	if configured(s.getenv("AWS_ROLE_ARN")) ||
		configured(s.getenv("AWS_WEB_IDENTITY_TOKEN_FILE")) {
		return s.webIdentity(ctx, now)
	}
	if configured(s.getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI")) ||
		configured(s.getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI")) {
		return s.container(ctx)
	}
	if strings.EqualFold(
		strings.TrimSpace(s.getenv("AWS_EC2_METADATA_DISABLED")),
		"true",
	) {
		return SigningMaterial{}, fmt.Errorf(
			"AWS workload credentials are unavailable",
		)
	}
	material, err := s.imds(ctx)
	if err != nil {
		return SigningMaterial{}, fmt.Errorf(
			"AWS workload credentials are unavailable: %w",
			err,
		)
	}
	return material, nil
}

func (s *Source) environment() (
	SigningMaterial,
	bool,
	error,
) {
	accessKey := firstConfigured(
		s.getenv("AWS_ACCESS_KEY_ID"),
		s.getenv("AWS_ACCESS_KEY"),
	)
	secretKey := firstConfigured(
		s.getenv("AWS_SECRET_ACCESS_KEY"),
		s.getenv("AWS_SECRET_KEY"),
	)
	sessionToken := strings.TrimSpace(s.getenv("AWS_SESSION_TOKEN"))
	if accessKey == "" && secretKey == "" {
		return SigningMaterial{}, false, nil
	}
	if accessKey == "" || secretKey == "" {
		return SigningMaterial{}, true, fmt.Errorf(
			"AWS environment credentials are incomplete",
		)
	}
	material, err := validateMaterial(SigningMaterial{
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
		SessionToken:    sessionToken,
		Source:          "environment",
	})
	return material, true, err
}

func (s *Source) webIdentity(
	ctx context.Context,
	now time.Time,
) (SigningMaterial, error) {
	roleARN := strings.TrimSpace(s.getenv("AWS_ROLE_ARN"))
	tokenPath := strings.TrimSpace(
		s.getenv("AWS_WEB_IDENTITY_TOKEN_FILE"),
	)
	if roleARN == "" || tokenPath == "" {
		return SigningMaterial{}, fmt.Errorf(
			"AWS web identity requires AWS_ROLE_ARN and AWS_WEB_IDENTITY_TOKEN_FILE",
		)
	}
	token, err := s.readBoundedToken(tokenPath)
	if err != nil {
		return SigningMaterial{}, fmt.Errorf(
			"read AWS web identity token: %w",
			err,
		)
	}
	sessionName := strings.TrimSpace(s.getenv("AWS_ROLE_SESSION_NAME"))
	if sessionName == "" {
		sessionName = "sparkroute-" + now.Format("20060102T150405")
	}
	endpoint := s.stsEndpoint
	if endpoint == "" {
		region := firstConfigured(
			s.getenv("AWS_REGION"),
			s.getenv("AWS_DEFAULT_REGION"),
		)
		if region == "" {
			endpoint = "https://sts.amazonaws.com"
		} else {
			if !validAWSRegion(region) {
				return SigningMaterial{}, fmt.Errorf(
					"AWS region is invalid",
				)
			}
			endpoint = "https://sts." + region + ".amazonaws.com"
		}
	}
	if err := validateHTTPSOrigin(endpoint); err != nil {
		return SigningMaterial{}, fmt.Errorf("AWS STS endpoint: %w", err)
	}
	form := make(url.Values)
	form.Set("Action", "AssumeRoleWithWebIdentity")
	form.Set("Version", "2011-06-15")
	form.Set("RoleArn", roleARN)
	form.Set("RoleSessionName", sessionName)
	form.Set("WebIdentityToken", token)
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		endpoint,
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return SigningMaterial{}, fmt.Errorf(
			"construct AWS STS request: %w",
			err,
		)
	}
	request.Header.Set(
		"Content-Type",
		"application/x-www-form-urlencoded",
	)
	response, err := s.client.Do(request)
	if err != nil {
		return SigningMaterial{}, fmt.Errorf(
			"request AWS STS credentials: %w",
			err,
		)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := readBounded(response.Body, maxCredentialResponseBytes)
	if err != nil {
		return SigningMaterial{}, fmt.Errorf(
			"read AWS STS credentials: %w",
			err,
		)
	}
	if response.StatusCode != http.StatusOK {
		return SigningMaterial{}, fmt.Errorf(
			"AWS STS returned status %d",
			response.StatusCode,
		)
	}
	var result struct {
		Result struct {
			Credentials struct {
				AccessKeyID     string `xml:"AccessKeyId"`
				SecretAccessKey string `xml:"SecretAccessKey"`
				SessionToken    string `xml:"SessionToken"`
				Expiration      string `xml:"Expiration"`
			} `xml:"Credentials"`
		} `xml:"AssumeRoleWithWebIdentityResult"`
	}
	if err := xml.Unmarshal(body, &result); err != nil {
		return SigningMaterial{}, fmt.Errorf(
			"decode AWS STS credentials",
		)
	}
	source := result.Result.Credentials
	expiration, err := time.Parse(time.RFC3339, source.Expiration)
	if err != nil {
		return SigningMaterial{}, fmt.Errorf(
			"decode AWS STS credential expiration",
		)
	}
	return validateMaterial(SigningMaterial{
		AccessKeyID:     source.AccessKeyID,
		SecretAccessKey: source.SecretAccessKey,
		SessionToken:    source.SessionToken,
		ExpiresAt:       expiration.UTC(),
		Source:          "web_identity",
	})
}

func (s *Source) container(
	ctx context.Context,
) (SigningMaterial, error) {
	endpoint := s.containerEndpoint
	if endpoint == "" {
		relative := strings.TrimSpace(
			s.getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"),
		)
		if relative != "" {
			if !strings.HasPrefix(relative, "/") {
				return SigningMaterial{}, fmt.Errorf(
					"AWS container relative URI must start with /",
				)
			}
			endpoint = "http://169.254.170.2" + relative
		} else {
			endpoint = strings.TrimSpace(
				s.getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI"),
			)
			if err := validateContainerEndpoint(endpoint); err != nil {
				return SigningMaterial{}, err
			}
		}
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		endpoint,
		nil,
	)
	if err != nil {
		return SigningMaterial{}, fmt.Errorf(
			"construct AWS container credential request: %w",
			err,
		)
	}
	authorization := strings.TrimSpace(
		s.getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN"),
	)
	tokenFile := strings.TrimSpace(
		s.getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"),
	)
	if authorization != "" && tokenFile != "" {
		return SigningMaterial{}, fmt.Errorf(
			"AWS container authorization token is ambiguous",
		)
	}
	if tokenFile != "" {
		authorization, err = s.readBoundedToken(tokenFile)
		if err != nil {
			return SigningMaterial{}, fmt.Errorf(
				"read AWS container authorization token: %w",
				err,
			)
		}
	}
	if authorization != "" {
		if strings.ContainsAny(authorization, "\r\n") {
			return SigningMaterial{}, fmt.Errorf(
				"AWS container authorization token contains a newline",
			)
		}
		request.Header.Set("Authorization", authorization)
	}
	return s.retrieveJSONCredentials(
		s.metadataClient,
		request,
		"container",
		"request AWS container credentials",
	)
}

func validateContainerEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("parse AWS container credential endpoint: %w", err)
	}
	if parsed.User != nil || parsed.Host == "" || parsed.Fragment != "" {
		return fmt.Errorf("AWS container credential endpoint is invalid")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if parsed.Scheme != "http" {
		return fmt.Errorf(
			"AWS container credential endpoint must use HTTP or HTTPS",
		)
	}
	host := parsed.Hostname()
	address := net.ParseIP(host)
	if strings.EqualFold(host, "localhost") ||
		address != nil &&
			(address.IsLoopback() ||
				address.Equal(net.ParseIP("169.254.170.2")) ||
				address.Equal(net.ParseIP("169.254.170.23"))) {
		return nil
	}
	return fmt.Errorf(
		"HTTP AWS container credential endpoint host is not allowed",
	)
}

func (s *Source) imds(ctx context.Context) (SigningMaterial, error) {
	endpoint := s.imdsEndpoint
	if endpoint == "" {
		endpoint = "http://169.254.169.254"
	}
	tokenRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPut,
		strings.TrimRight(endpoint, "/")+"/latest/api/token",
		nil,
	)
	if err != nil {
		return SigningMaterial{}, fmt.Errorf(
			"construct IMDSv2 token request: %w",
			err,
		)
	}
	tokenRequest.Header.Set(
		"X-Aws-Ec2-Metadata-Token-Ttl-Seconds",
		"21600",
	)
	response, err := s.metadataClient.Do(tokenRequest)
	if err != nil {
		return SigningMaterial{}, fmt.Errorf(
			"request IMDSv2 token: %w",
			err,
		)
	}
	token, readErr := readBounded(response.Body, 8192)
	_ = response.Body.Close()
	if readErr != nil {
		return SigningMaterial{}, fmt.Errorf(
			"read IMDSv2 token: %w",
			readErr,
		)
	}
	if response.StatusCode != http.StatusOK ||
		len(bytes.TrimSpace(token)) == 0 {
		return SigningMaterial{}, fmt.Errorf(
			"IMDSv2 token endpoint returned status %d",
			response.StatusCode,
		)
	}
	tokenValue := string(bytes.TrimSpace(token))
	roleURL := strings.TrimRight(endpoint, "/") +
		"/latest/meta-data/iam/security-credentials/"
	roleRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		roleURL,
		nil,
	)
	if err != nil {
		return SigningMaterial{}, fmt.Errorf(
			"construct IMDSv2 role request: %w",
			err,
		)
	}
	roleRequest.Header.Set("X-Aws-Ec2-Metadata-Token", tokenValue)
	response, err = s.metadataClient.Do(roleRequest)
	if err != nil {
		return SigningMaterial{}, fmt.Errorf(
			"request IMDSv2 role: %w",
			err,
		)
	}
	roleBody, readErr := readBounded(response.Body, 8192)
	_ = response.Body.Close()
	if readErr != nil {
		return SigningMaterial{}, fmt.Errorf(
			"read IMDSv2 role: %w",
			readErr,
		)
	}
	if response.StatusCode != http.StatusOK {
		return SigningMaterial{}, fmt.Errorf(
			"IMDSv2 role endpoint returned status %d",
			response.StatusCode,
		)
	}
	role := strings.TrimSpace(strings.SplitN(string(roleBody), "\n", 2)[0])
	if role == "" || role != path.Base(role) ||
		strings.ContainsAny(role, "\r\n") {
		return SigningMaterial{}, fmt.Errorf(
			"IMDSv2 returned an invalid role name",
		)
	}
	credentialRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		roleURL+url.PathEscape(role),
		nil,
	)
	if err != nil {
		return SigningMaterial{}, fmt.Errorf(
			"construct IMDSv2 credential request: %w",
			err,
		)
	}
	credentialRequest.Header.Set(
		"X-Aws-Ec2-Metadata-Token",
		tokenValue,
	)
	return s.retrieveJSONCredentials(
		s.metadataClient,
		credentialRequest,
		"imds",
		"request IMDSv2 credentials",
	)
}

func (s *Source) retrieveJSONCredentials(
	client *http.Client,
	request *http.Request,
	sourceName string,
	action string,
) (SigningMaterial, error) {
	response, err := client.Do(request)
	if err != nil {
		return SigningMaterial{}, fmt.Errorf("%s: %w", action, err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := readBounded(response.Body, maxCredentialResponseBytes)
	if err != nil {
		return SigningMaterial{}, fmt.Errorf("%s: %w", action, err)
	}
	if response.StatusCode != http.StatusOK {
		return SigningMaterial{}, fmt.Errorf(
			"%s: endpoint returned status %d",
			action,
			response.StatusCode,
		)
	}
	var source struct {
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string `json:"SecretAccessKey"`
		Token           string `json:"Token"`
		Expiration      string `json:"Expiration"`
	}
	if err := json.Unmarshal(body, &source); err != nil {
		return SigningMaterial{}, fmt.Errorf(
			"%s: decode credential response",
			action,
		)
	}
	var expiration time.Time
	if source.Expiration != "" {
		expiration, err = time.Parse(time.RFC3339, source.Expiration)
		if err != nil {
			return SigningMaterial{}, fmt.Errorf(
				"%s: decode credential expiration",
				action,
			)
		}
	}
	return validateMaterial(SigningMaterial{
		AccessKeyID:     source.AccessKeyID,
		SecretAccessKey: source.SecretAccessKey,
		SessionToken:    source.Token,
		ExpiresAt:       expiration.UTC(),
		Source:          sourceName,
	})
}

func validateMaterial(
	material SigningMaterial,
) (SigningMaterial, error) {
	if material.AccessKeyID == "" || material.SecretAccessKey == "" {
		return SigningMaterial{}, fmt.Errorf(
			"AWS workload credential response is incomplete",
		)
	}
	if strings.ContainsAny(
		material.AccessKeyID+
			material.SecretAccessKey+
			material.SessionToken,
		"\r\n",
	) {
		return SigningMaterial{}, fmt.Errorf(
			"AWS workload credential response contains a newline",
		)
	}
	return material, nil
}

func (s *Source) readBoundedToken(fileName string) (string, error) {
	var (
		raw []byte
		err error
	)
	if s.readFile != nil {
		raw, err = s.readFile(fileName)
		if err != nil {
			return "", err
		}
		if len(raw) > maxWorkloadTokenBytes {
			return "", fmt.Errorf(
				"token exceeds %d bytes",
				maxWorkloadTokenBytes,
			)
		}
	} else {
		file, openErr := os.Open(fileName)
		if openErr != nil {
			return "", openErr
		}
		defer func() { _ = file.Close() }()
		raw, err = readBounded(file, maxWorkloadTokenBytes)
		if err != nil {
			return "", err
		}
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("token is empty")
	}
	return token, nil
}

func readBounded(source io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(source, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return body, nil
}

func validateHTTPSOrigin(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return err
	}
	if parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" ||
		parsed.Fragment != "" ||
		parsed.Path != "" && parsed.Path != "/" {
		return fmt.Errorf("must be an HTTPS origin URL")
	}
	return nil
}

func validAWSRegion(value string) bool {
	if value == "" || len(value) > 64 ||
		value != strings.ToLower(value) {
		return false
	}
	for index, char := range value {
		if char >= 'a' && char <= 'z' ||
			char >= '0' && char <= '9' ||
			char == '-' && index > 0 && index < len(value)-1 {
			continue
		}
		return false
	}
	return true
}

func configured(value string) bool {
	return strings.TrimSpace(value) != ""
}

func firstConfigured(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

var _ credentials.Source = (*Source)(nil)
