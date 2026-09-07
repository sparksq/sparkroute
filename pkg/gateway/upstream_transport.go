package gateway

import "net/http"

const (
	defaultUpstreamMaxIdleConns        = 1024
	defaultUpstreamMaxIdleConnsPerHost = 256
)

var sharedDefaultUpstreamHTTPClient = &http.Client{
	Transport: newDefaultUpstreamTransport(),
}

// defaultUpstreamHTTPClient returns the process-wide client used when callers do
// not inject one. Sharing the transport across immutable routing snapshots lets
// connections survive configuration reloads as well as ordinary requests.
func defaultUpstreamHTTPClient() *http.Client {
	return sharedDefaultUpstreamHTTPClient
}

func newDefaultUpstreamTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = defaultUpstreamMaxIdleConns
	transport.MaxIdleConnsPerHost = defaultUpstreamMaxIdleConnsPerHost
	return transport
}
