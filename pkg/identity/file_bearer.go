package identity

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
)

const maxBearerTokenFileBytes = 4096

// FileBearerAuthenticator authenticates requests against the current contents
// of a local token file. The file is read for every request so an owner can
// atomically enable, rotate, or disable authentication without restarting the
// listener.
//
// When Optional is true, a missing or empty file grants FallbackPrincipal.
// This is intended for the convenience-focused standalone profile only. Other
// profiles should leave Optional false or use their existing authenticator.
type FileBearerAuthenticator struct {
	Path              string
	Principal         Principal
	Optional          bool
	FallbackPrincipal Principal
}

func (a FileBearerAuthenticator) Authenticate(
	_ context.Context,
	request *http.Request,
) (Principal, error) {
	token, present, err := readBearerTokenFile(a.Path)
	if err != nil {
		return Principal{}, ErrInvalidCredentials
	}
	if !present {
		if a.Optional {
			return clonePrincipal(a.FallbackPrincipal), nil
		}
		return Principal{}, ErrMissingCredentials
	}
	provided, err := requestBearerToken(request)
	if err != nil {
		return Principal{}, err
	}
	expectedDigest := sha256.Sum256([]byte(token))
	providedDigest := sha256.Sum256([]byte(provided))
	if subtle.ConstantTimeCompare(expectedDigest[:], providedDigest[:]) != 1 {
		return Principal{}, ErrInvalidCredentials
	}
	return clonePrincipal(a.Principal), nil
}

func (FileBearerAuthenticator) AuthenticationChallenge(realm string) string {
	return `Bearer realm="` + realm + `"`
}

func readBearerTokenFile(path string) (string, bool, error) {
	if strings.TrimSpace(path) == "" {
		return "", false, errors.New("bearer token file path is empty")
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxBearerTokenFileBytes {
		return "", false, errors.New("bearer token file is invalid")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxBearerTokenFileBytes+1))
	if err != nil {
		return "", false, err
	}
	if len(raw) > maxBearerTokenFileBytes {
		return "", false, errors.New("bearer token file is too large")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", false, nil
	}
	if strings.ContainsAny(token, " \t\r\n\x00") {
		return "", false, errors.New("bearer token contains invalid whitespace")
	}
	return token, true, nil
}

func requestBearerToken(request *http.Request) (string, error) {
	values := request.Header.Values("Authorization")
	if len(values) == 0 {
		return "", ErrMissingCredentials
	}
	if len(values) != 1 {
		return "", ErrInvalidCredentials
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", ErrInvalidCredentials
	}
	return parts[1], nil
}

func clonePrincipal(value Principal) Principal {
	value.Roles = append([]string(nil), value.Roles...)
	value.AllowedAttribution = append([]string(nil), value.AllowedAttribution...)
	value.FixedAttribution = cloneMap(value.FixedAttribution)
	return value
}
