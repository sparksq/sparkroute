package awsworkload

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/credentials"
)

func TestSourceResolvesAndCachesEnvironmentCredentials(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		"AWS_ACCESS_KEY_ID":         "AKIDEXAMPLE",
		"AWS_SECRET_ACCESS_KEY":     "secret",
		"AWS_SESSION_TOKEN":         "session",
		"AWS_EC2_METADATA_DISABLED": "true",
	}
	source := New(Options{
		Getenv: func(name string) string { return values[name] },
	})
	for index := 0; index < 2; index++ {
		resolved, err := source.Resolve(
			context.Background(),
			credentials.Ref("workload://aws"),
		)
		if err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
		material, err := DecodeSigningMaterial(resolved.Value)
		if err != nil {
			t.Fatalf("DecodeSigningMaterial() error = %v", err)
		}
		if material.AccessKeyID != "AKIDEXAMPLE" ||
			material.SecretAccessKey != "secret" ||
			material.SessionToken != "session" ||
			material.Source != "environment" {
			t.Fatalf("material = %#v", material)
		}
	}
}

func TestSourceResolvesWebIdentityAndRefreshesNearExpiry(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		calls.Add(1)
		body, _ := io.ReadAll(request.Body)
		if request.Method != http.MethodPost ||
			!strings.Contains(
				string(body),
				"Action=AssumeRoleWithWebIdentity",
			) ||
			!strings.Contains(string(body), "WebIdentityToken=jwt-token") {
			t.Errorf("STS request = %s %q", request.Method, body)
		}
		_, _ = io.WriteString(w,
			`<AssumeRoleWithWebIdentityResponse>`+
				`<AssumeRoleWithWebIdentityResult><Credentials>`+
				`<AccessKeyId>WEBKEY</AccessKeyId>`+
				`<SecretAccessKey>web-secret</SecretAccessKey>`+
				`<SessionToken>web-session</SessionToken>`+
				`<Expiration>`+
				now.Add(10*time.Minute).Format(time.RFC3339)+
				`</Expiration></Credentials>`+
				`</AssumeRoleWithWebIdentityResult>`+
				`</AssumeRoleWithWebIdentityResponse>`,
		)
	}))
	defer server.Close()
	values := map[string]string{
		"AWS_ROLE_ARN":                "arn:aws:iam::123456789012:role/gateway",
		"AWS_WEB_IDENTITY_TOKEN_FILE": "/var/run/token",
		"AWS_ROLE_SESSION_NAME":       "gateway-test",
	}
	source := New(Options{
		HTTPClient: server.Client(),
		Now:        func() time.Time { return now },
		Getenv:     func(name string) string { return values[name] },
		ReadFile: func(name string) ([]byte, error) {
			if name != "/var/run/token" {
				t.Fatalf("ReadFile(%q)", name)
			}
			return []byte("jwt-token\n"), nil
		},
		STSEndpoint: server.URL,
	})
	for index := 0; index < 2; index++ {
		resolved, err := source.Resolve(
			context.Background(),
			credentials.Ref("workload://aws/default"),
		)
		if err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
		material, err := DecodeSigningMaterial(resolved.Value)
		if err != nil || material.AccessKeyID != "WEBKEY" ||
			material.Source != "web_identity" {
			t.Fatalf("material = %#v, %v", material, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("STS calls = %d, want 1", calls.Load())
	}
	now = now.Add(6 * time.Minute)
	if _, err := source.Resolve(
		context.Background(),
		credentials.Ref("workload://aws"),
	); err != nil {
		t.Fatalf("refresh Resolve() error = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("STS calls after refresh = %d, want 2", calls.Load())
	}
}

func TestSourceResolvesContainerAuthorizationToken(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		calls.Add(1)
		if request.Header.Get("Authorization") != "Bearer pod-token" {
			t.Errorf(
				"Authorization = %q",
				request.Header.Get("Authorization"),
			)
		}
		_, _ = io.WriteString(w, `{
			"AccessKeyId":"PODKEY",
			"SecretAccessKey":"pod-secret",
			"Token":"pod-session",
			"Expiration":"2026-07-30T14:00:00Z"
		}`)
	}))
	defer server.Close()
	values := map[string]string{
		"AWS_CONTAINER_CREDENTIALS_FULL_URI":     "configured",
		"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE": "/var/run/pod-token",
	}
	source := New(Options{
		HTTPClient:        server.Client(),
		Now:               func() time.Time { return time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC) },
		Getenv:            func(name string) string { return values[name] },
		ReadFile:          func(string) ([]byte, error) { return []byte("Bearer pod-token"), nil },
		ContainerEndpoint: server.URL,
	})
	resolved, err := source.Resolve(
		context.Background(),
		credentials.Ref("workload://aws"),
	)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	material, err := DecodeSigningMaterial(resolved.Value)
	if err != nil || material.AccessKeyID != "PODKEY" ||
		material.Source != "container" {
		t.Fatalf("material = %#v, %v", material, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("container calls = %d", calls.Load())
	}
}

func TestSourceUsesIMDSv2Only(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		switch request.URL.Path {
		case "/latest/api/token":
			if request.Method != http.MethodPut ||
				request.Header.Get(
					"X-Aws-Ec2-Metadata-Token-Ttl-Seconds",
				) != "21600" {
				t.Errorf("token request = %#v", request)
			}
			_, _ = io.WriteString(w, "imds-token")
		case "/latest/meta-data/iam/security-credentials/":
			if request.Header.Get(
				"X-Aws-Ec2-Metadata-Token",
			) != "imds-token" {
				t.Error("role request omitted IMDSv2 token")
			}
			_, _ = io.WriteString(w, "gateway-role")
		case "/latest/meta-data/iam/security-credentials/gateway-role":
			if request.Header.Get(
				"X-Aws-Ec2-Metadata-Token",
			) != "imds-token" {
				t.Error("credential request omitted IMDSv2 token")
			}
			_, _ = io.WriteString(w, `{
				"AccessKeyId":"IMDSKEY",
				"SecretAccessKey":"imds-secret",
				"Token":"imds-session",
				"Expiration":"2026-07-30T14:00:00Z"
			}`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	source := New(Options{
		HTTPClient:   server.Client(),
		Now:          func() time.Time { return time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC) },
		Getenv:       func(string) string { return "" },
		IMDSEndpoint: server.URL,
	})
	resolved, err := source.Resolve(
		context.Background(),
		credentials.Ref("workload://aws"),
	)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	material, err := DecodeSigningMaterial(resolved.Value)
	if err != nil || material.AccessKeyID != "IMDSKEY" ||
		material.Source != "imds" {
		t.Fatalf("material = %#v, %v", material, err)
	}
}

func TestSourceRejectsUnsafeHTTPContainerEndpoint(t *testing.T) {
	t.Parallel()

	if err := validateContainerEndpoint(
		"http://example.com/credentials",
	); err == nil {
		t.Fatal("validateContainerEndpoint() error = nil")
	}
	if err := validateContainerEndpoint(
		"http://169.254.170.23/v1/credentials",
	); err != nil {
		t.Fatalf("pod identity endpoint error = %v", err)
	}
}
