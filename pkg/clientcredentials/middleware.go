package clientcredentials

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sparksq/sparkroute/pkg/identity"
)

// WrapDataPlane authenticates a standalone data-plane request. Standalone has one implicit
// tenant and no caller-controlled attribution vocabulary, so the resulting
// identity contains only the credential-bound principal.
func WrapDataPlane(authenticator identity.Authenticator, next http.Handler) http.Handler {
	if authenticator == nil {
		panic("client credential data-plane middleware requires an authenticator")
	}
	if next == nil {
		panic("client credential data-plane middleware requires a next handler")
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		principal, err := authenticator.Authenticate(request.Context(), request)
		if err != nil {
			challenge := `Bearer realm="sparkroute"`
			if provider, ok := authenticator.(identity.ChallengeAuthenticator); ok {
				challenge = provider.AuthenticationChallenge("sparkroute")
			}
			if challenge != "" {
				writer.Header().Set("WWW-Authenticate", challenge)
			}
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("X-Content-Type-Options", "nosniff")
			writer.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"error": map[string]string{
					"code": "invalid_api_key", "message": "caller authentication failed",
				},
			})
			return
		}
		attribution := identity.Attribution{}
		if threadID := strings.TrimSpace(request.Header.Get("X-SparkRoute-Thread-Id")); threadID != "" && len(threadID) <= 256 && !strings.ContainsAny(threadID, "\r\n\x00") {
			// A standalone caller may name a conversation, but mappings remain
			// isolated by its authenticated principal.
			attribution[identity.AttributeThreadID] = threadID
		}
		nextRequest := request.Clone(identity.WithContext(
			request.Context(), identity.Identity{Principal: principal, Attribution: attribution},
		))
		nextRequest.Header = request.Header.Clone()
		for _, name := range []string{
			"Authorization", "Cookie", "Proxy-Authorization", "X-Api-Key",
			"X-Goog-Api-Key",
		} {
			nextRequest.Header.Del(name)
		}
		for name := range nextRequest.Header {
			lower := strings.ToLower(name)
			if strings.HasPrefix(lower, "x-sparkroute-") ||
				strings.HasPrefix(lower, "x-auth-") ||
				lower == "x-mlflow-experiment-id" {
				nextRequest.Header.Del(name)
			}
		}
		next.ServeHTTP(writer, nextRequest)
	})
}
