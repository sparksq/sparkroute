package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

const (
	defaultReadHeaderTimeout = 5 * time.Second
	defaultShutdownTimeout   = 10 * time.Second
)

type ListenerSpec struct {
	Name      string
	Address   string
	Handler   http.Handler
	TLSConfig *tls.Config
}

type BoundListener struct {
	Name    string
	Address string
}

type runningServer struct {
	server   *http.Server
	listener net.Listener
	tls      bool
}

// ServerGroup owns independent HTTP listeners and shuts all of them down when
// the parent context ends or any listener fails.
type ServerGroup struct {
	Listeners         []ListenerSpec
	ReadHeaderTimeout time.Duration
	ShutdownTimeout   time.Duration
	OnReady           func([]BoundListener)
}

func (g ServerGroup) Run(ctx context.Context) error {
	if len(g.Listeners) == 0 {
		return fmt.Errorf("at least one listener is required")
	}
	readHeaderTimeout := g.ReadHeaderTimeout
	if readHeaderTimeout <= 0 {
		readHeaderTimeout = defaultReadHeaderTimeout
	}
	shutdownTimeout := g.ShutdownTimeout
	if shutdownTimeout <= 0 {
		shutdownTimeout = defaultShutdownTimeout
	}
	// A process signal starts graceful shutdown but must not cancel active
	// request contexts first. http.Server.Shutdown closes listeners and waits for
	// handlers; any request still running at the deadline is canceled by Close.
	// Preserve caller-installed values without inheriting parent cancellation.
	requestBaseContext, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()

	running := make([]runningServer, 0, len(g.Listeners))
	bound := make([]BoundListener, 0, len(g.Listeners))
	names := make(map[string]struct{}, len(g.Listeners))

	for _, spec := range g.Listeners {
		if spec.Name == "" {
			closeListeners(running)
			return fmt.Errorf("listener name is required")
		}
		if _, exists := names[spec.Name]; exists {
			closeListeners(running)
			return fmt.Errorf("duplicate listener name %q", spec.Name)
		}
		names[spec.Name] = struct{}{}
		if spec.Address == "" {
			closeListeners(running)
			return fmt.Errorf("listener %q address is required", spec.Name)
		}
		if spec.Handler == nil {
			closeListeners(running)
			return fmt.Errorf("listener %q handler is required", spec.Name)
		}
		var tlsConfig *tls.Config
		if spec.TLSConfig != nil {
			tlsConfig = spec.TLSConfig.Clone()
			if len(tlsConfig.Certificates) == 0 &&
				tlsConfig.GetCertificate == nil &&
				tlsConfig.GetConfigForClient == nil {
				closeListeners(running)
				return fmt.Errorf(
					"listener %q TLS configuration has no server certificate",
					spec.Name,
				)
			}
		}

		listener, err := net.Listen("tcp", spec.Address)
		if err != nil {
			closeListeners(running)
			return fmt.Errorf("listen %s on %s: %w", spec.Name, spec.Address, err)
		}
		server := &http.Server{
			Handler:           spec.Handler,
			ReadHeaderTimeout: readHeaderTimeout,
			TLSConfig:         tlsConfig,
			BaseContext: func(net.Listener) context.Context {
				return requestBaseContext
			},
		}
		running = append(running, runningServer{
			server:   server,
			listener: listener,
			tls:      tlsConfig != nil,
		})
		bound = append(bound, BoundListener{Name: spec.Name, Address: listener.Addr().String()})
	}

	serveErrors := make(chan error, len(running))
	for _, item := range running {
		item := item
		go func() {
			if item.tls {
				serveErrors <- item.server.ServeTLS(item.listener, "", "")
				return
			}
			serveErrors <- item.server.Serve(item.listener)
		}()
	}
	if g.OnReady != nil {
		g.OnReady(append([]BoundListener(nil), bound...))
	}

	var runErr error
	received := 0
	select {
	case <-ctx.Done():
	case err := <-serveErrors:
		received++
		if !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			runErr = fmt.Errorf("serve: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	shutdownErrors := make(chan error, len(running))
	for _, item := range running {
		item := item
		go func() {
			err := item.server.Shutdown(shutdownCtx)
			if err != nil {
				_ = item.server.Close()
			}
			shutdownErrors <- err
		}()
	}
	for range running {
		if err := <-shutdownErrors; err != nil && runErr == nil {
			runErr = fmt.Errorf("shutdown: %w", err)
		}
	}
	for received < len(running) {
		err := <-serveErrors
		received++
		if !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) && runErr == nil {
			runErr = fmt.Errorf("serve: %w", err)
		}
	}
	return runErr
}

func closeListeners(running []runningServer) {
	for _, item := range running {
		_ = item.listener.Close()
	}
}
