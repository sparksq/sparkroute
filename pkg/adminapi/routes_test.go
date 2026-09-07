package adminapi

import "testing"

func TestIsPath(t *testing.T) {
	t.Parallel()

	tests := map[string]bool{
		"/":                                     false,
		"/v1/models":                            false,
		"/v1/chat/completions":                  false,
		"/v1/responses":                         false,
		"/v1/embeddings":                        false,
		"/v1/traces":                            false,
		"/admin":                                true,
		"/admin/":                               true,
		"/admin/config":                         true,
		"/traces":                               true,
		"/v1/ui/bootstrap":                      true,
		"/v1/status":                            true,
		"/v1/mm-projection/probe":               true,
		"/v1/config":                            true,
		"/v1/config/managed-sets/sparkrun":      true,
		"/v1/model-routing/discovered-metadata": true,
		"/v1/ledger/requests/request-1":         true,
		"/v1/saved-traces/export":               true,
		"/v1/runtime/endpoints":                 true,
		"/v1/client-credentials/credential-id":  true,
		"/v1/client-credentials-invalid-prefix": false,
		"/v1/configuration-is-not-admin-config": false,
	}
	for path, expected := range tests {
		path, expected := path, expected
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			if actual := IsPath(path); actual != expected {
				t.Fatalf("IsPath(%q) = %t, want %t", path, actual, expected)
			}
		})
	}
}
