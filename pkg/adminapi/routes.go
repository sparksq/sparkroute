package adminapi

import "strings"

// IsPath reports whether path belongs to the administration surface. The
// classification is profile-neutral so a co-located listener can route both
// standalone and cluster administration APIs without passing them through the
// data-plane handler.
func IsPath(path string) bool {
	for _, prefix := range []string{
		"/admin",
		"/traces",
		"/v1/ui",
		"/v1/status",
		"/v1/mm-projection",
		"/v1/config",
		"/v1/model-routing",
		"/v1/ledger",
		"/v1/saved-traces",
		"/v1/runtime",
		"/v1/client-credentials",
		"/v1/provider-auth",
		"/v1/sparkrun",
	} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}
