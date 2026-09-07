package gateway

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestServerGroupStartsAndStops(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan []BoundListener, 1)
	done := make(chan error, 1)
	group := ServerGroup{
		Listeners: []ListenerSpec{{
			Name:    "test",
			Address: "127.0.0.1:0",
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}),
		}},
		ShutdownTimeout: time.Second,
		OnReady: func(listeners []BoundListener) {
			ready <- listeners
		},
	}
	go func() {
		done <- group.Run(ctx)
	}()

	select {
	case listeners := <-ready:
		if len(listeners) != 1 || listeners[0].Address == "" {
			t.Fatalf("listeners = %#v", listeners)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not become ready")
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop")
	}
}

func TestServerGroupDrainsActiveRequestBeforeCancelingItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan []BoundListener, 1)
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	requestContextCanceled := make(chan struct{}, 1)
	done := make(chan error, 1)
	group := ServerGroup{
		Listeners: []ListenerSpec{{
			Name:    "test",
			Address: "127.0.0.1:0",
			Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				close(requestStarted)
				select {
				case <-releaseRequest:
					writer.WriteHeader(http.StatusNoContent)
				case <-request.Context().Done():
					requestContextCanceled <- struct{}{}
				}
			}),
		}},
		ShutdownTimeout: 2 * time.Second,
		OnReady: func(listeners []BoundListener) {
			ready <- listeners
		},
	}
	go func() {
		done <- group.Run(ctx)
	}()

	var listeners []BoundListener
	select {
	case listeners = <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not become ready")
	}
	responseDone := make(chan error, 1)
	go func() {
		response, err := http.Get("http://" + listeners[0].Address)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			err = response.Body.Close()
			if err == nil && response.StatusCode != http.StatusNoContent {
				err = fmt.Errorf("status = %d", response.StatusCode)
			}
		}
		responseDone <- err
	}()
	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach handler")
	}
	cancel()

	select {
	case <-requestContextCanceled:
		t.Fatal("active request context was canceled before graceful drain")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseRequest)
	select {
	case err := <-responseDone:
		if err != nil {
			t.Fatalf("active request error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active request did not finish")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not finish graceful shutdown")
	}
}

func TestServerGroupRejectsTLSWithoutServerCertificate(t *testing.T) {
	t.Parallel()

	err := (ServerGroup{Listeners: []ListenerSpec{{
		Name:      "tls",
		Address:   "127.0.0.1:0",
		Handler:   http.NotFoundHandler(),
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}}}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no server certificate") {
		t.Fatalf("Run() error = %v", err)
	}
}
