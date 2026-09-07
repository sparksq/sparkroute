package gateway

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultUpstreamHTTPClientIsSharedAndTuned(t *testing.T) {
	t.Parallel()

	first := defaultUpstreamHTTPClient()
	second := defaultUpstreamHTTPClient()
	if first != second {
		t.Fatal("default upstream HTTP client is not process-wide")
	}
	transport, ok := first.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("default upstream transport type = %T, want *http.Transport", first.Transport)
	}
	if transport == http.DefaultTransport {
		t.Fatal("default upstream transport mutates the process-global HTTP transport")
	}
	if transport.MaxIdleConns != defaultUpstreamMaxIdleConns {
		t.Fatalf("MaxIdleConns = %d, want %d", transport.MaxIdleConns, defaultUpstreamMaxIdleConns)
	}
	if transport.MaxIdleConnsPerHost != defaultUpstreamMaxIdleConnsPerHost {
		t.Fatalf(
			"MaxIdleConnsPerHost = %d, want %d",
			transport.MaxIdleConnsPerHost,
			defaultUpstreamMaxIdleConnsPerHost,
		)
	}
	if !transport.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 = false, want inherited standard-library default true")
	}
}

func TestOpenAIHandlersShareDefaultUpstreamTransport(t *testing.T) {
	t.Parallel()

	first := newOpenAIHandler(
		openAIOperationChatCompletions,
		nil,
		nil,
		nil,
		DataOptions{},
		nil,
	)
	second := newOpenAIHandler(
		openAIOperationEmbeddings,
		nil,
		nil,
		nil,
		DataOptions{},
		nil,
	)
	if first.client == second.client {
		t.Fatal("handlers share a mutable HTTP client instead of private client copies")
	}
	if first.client.Transport == nil || first.client.Transport != second.client.Transport {
		t.Fatal("default handlers do not share the process-wide upstream transport")
	}
	if first.client.CheckRedirect == nil || second.client.CheckRedirect == nil {
		t.Fatal("handler redirect policy is not installed")
	}
}

func TestDefaultUpstreamTransportRetainsConcurrentIdleConnections(t *testing.T) {
	t.Parallel()

	const concurrency = 32
	var newConnections atomic.Int64
	var blockFirstWave atomic.Bool
	blockFirstWave.Store(true)
	firstWaveArrived := make(chan struct{}, concurrency)
	releaseFirstWave := make(chan struct{})
	var releaseFirstWaveOnce sync.Once
	release := func() {
		releaseFirstWaveOnce.Do(func() { close(releaseFirstWave) })
	}
	defer release()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if blockFirstWave.Load() {
			firstWaveArrived <- struct{}{}
			<-releaseFirstWave
		}
		_, _ = io.WriteString(w, "ok")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			newConnections.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)

	transport := newDefaultUpstreamTransport()
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport}

	runWave := func() {
		t.Helper()
		var waitGroup sync.WaitGroup
		errors := make(chan error, concurrency)
		for range concurrency {
			waitGroup.Add(1)
			go func() {
				defer waitGroup.Done()
				response, err := client.Get(server.URL)
				if err != nil {
					errors <- err
					return
				}
				_, err = io.Copy(io.Discard, response.Body)
				closeErr := response.Body.Close()
				if err != nil {
					errors <- err
				} else if closeErr != nil {
					errors <- closeErr
				}
			}()
		}
		waitGroup.Wait()
		close(errors)
		for err := range errors {
			t.Errorf("upstream request failed: %v", err)
		}
	}

	firstWaveDone := make(chan struct{})
	go func() {
		runWave()
		close(firstWaveDone)
	}()
	for range concurrency {
		select {
		case <-firstWaveArrived:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for concurrent upstream connections")
		}
	}
	blockFirstWave.Store(false)
	release()
	<-firstWaveDone

	if got := newConnections.Load(); got != concurrency {
		t.Fatalf("first-wave connections = %d, want %d", got, concurrency)
	}
	runWave()
	if got := newConnections.Load(); got != concurrency {
		t.Fatalf("connections after reuse wave = %d, want %d", got, concurrency)
	}
}
