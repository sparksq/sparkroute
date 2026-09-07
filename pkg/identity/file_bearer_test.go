package identity

import (
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFileBearerAuthenticatorSwitchesWithoutReconstruction(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "admin-token")
	authenticator := FileBearerAuthenticator{
		Path: path,
		Principal: Principal{
			ID: "token-operator", Roles: []string{"config_write"},
		},
		Optional: true,
		FallbackPrincipal: Principal{
			ID: "open-operator", Roles: []string{"config_write"},
		},
	}
	request := httptest.NewRequest("GET", "http://gateway.test/v1/status", nil)

	principal, err := authenticator.Authenticate(request.Context(), request)
	if err != nil || principal.ID != "open-operator" {
		t.Fatalf("open authentication = %#v, %v", principal, err)
	}

	if err := os.WriteFile(path, []byte("secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := authenticator.Authenticate(request.Context(), request); !errors.Is(err, ErrMissingCredentials) {
		t.Fatalf("missing bearer error = %v", err)
	}
	request.Header.Set("Authorization", "Bearer wrong-token")
	if _, err := authenticator.Authenticate(request.Context(), request); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong bearer error = %v", err)
	}
	request.Header.Set("Authorization", "Bearer secret-token")
	principal, err = authenticator.Authenticate(request.Context(), request)
	if err != nil || principal.ID != "token-operator" {
		t.Fatalf("token authentication = %#v, %v", principal, err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	principal, err = authenticator.Authenticate(request.Context(), request)
	if err != nil || principal.ID != "open-operator" {
		t.Fatalf("reopened authentication = %#v, %v", principal, err)
	}
}

func TestFileBearerAuthenticatorRequiredModeFailsClosed(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "master-token")
	authenticator := FileBearerAuthenticator{
		Path: path, Principal: Principal{ID: "caller"},
	}
	request := httptest.NewRequest("GET", "http://gateway.test/v1/models", nil)
	if _, err := authenticator.Authenticate(request.Context(), request); !errors.Is(err, ErrMissingCredentials) {
		t.Fatalf("missing token file error = %v", err)
	}
	if err := os.WriteFile(path, []byte("invalid token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := authenticator.Authenticate(request.Context(), request); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("invalid token file error = %v", err)
	}
}
