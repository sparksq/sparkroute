// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC and Fox Engine Ltd.

package providerauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	llmauth "github.com/scitrera/go-llm/auth"
	openaiauth "github.com/scitrera/go-llm/auth/openai"
	"github.com/sparksq/sparkroute/pkg/config"
)

const loginTimeout = 10 * time.Minute

// Status deliberately excludes access, refresh, ID tokens, and device-auth IDs.
type Status struct {
	Profile         string     `json:"profile"`
	State           string     `json:"state"`
	Email           string     `json:"email,omitempty"`
	Plan            string     `json:"plan,omitempty"`
	AccountID       string     `json:"account_id,omitempty"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	VerificationURL string     `json:"verification_url,omitempty"`
	UserCode        string     `json:"user_code,omitempty"`
	LoginExpiresAt  *time.Time `json:"login_expires_at,omitempty"`
	Message         string     `json:"message,omitempty"`
}

type profile struct {
	manager *openaiauth.Manager
	op      sync.Mutex
	mu      sync.Mutex
	owner   string
	status  Status
	cancel  context.CancelFunc
	done    chan struct{}
}

// Service survives runtime configuration reloads. Managers and pending login
// operations are shared per profile, preserving refresh/logout serialization.
type Service struct {
	ctx      context.Context
	stop     context.CancelFunc
	store    llmauth.Store
	config   openaiauth.Config
	mu       sync.Mutex
	profiles map[string]*profile
}

func New(ctx context.Context, store llmauth.Store, options openaiauth.Config) *Service {
	options.Originator = "sparkroute"
	options.DevicePollTimeout = loginTimeout
	if options.HTTPClient == nil {
		options.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	client := *options.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	options.HTTPClient = &client
	ctx, stop := context.WithCancel(ctx)
	return &Service{ctx: ctx, stop: stop, store: store, config: options, profiles: map[string]*profile{}}
}

func (s *Service) get(name string) (*profile, error) {
	if err := config.ValidateSubscriptionProfile(name); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	if p := s.profiles[name]; p != nil {
		return p, nil
	}
	if len(s.profiles) >= 128 {
		return nil, fmt.Errorf("provider credential profile limit reached; restart to clear unused profiles")
	}
	manager, err := openaiauth.NewManager(name, s.store, s.config)
	if err != nil {
		return nil, err
	}
	p := &profile{manager: manager, status: Status{Profile: name, State: "signed_out"}}
	s.profiles[name] = p
	return p, nil
}

func (s *Service) Status(ctx context.Context, name, actor string) (Status, error) {
	p, err := s.get(name)
	if err != nil {
		return Status{}, err
	}
	p.mu.Lock()
	status := p.status
	if p.owner != actor {
		status.UserCode = ""
		status.VerificationURL = ""
	}
	p.mu.Unlock()
	credential, err := p.manager.Status(ctx)
	if errors.Is(err, llmauth.ErrCredentialNotFound) {
		return status, nil
	}
	if err != nil {
		return Status{}, fmt.Errorf("could not read provider sign-in status")
	}
	// A login may have persisted tokens just before its worker clears pending
	// state. Never carry one-time login guidance into a connected projection.
	status = Status{Profile: name, State: "connected"}
	status.Email, status.Plan, status.AccountID = credential.Email, credential.Plan, credential.AccountID
	if !credential.ExpiresAt.IsZero() {
		expiry := credential.ExpiresAt
		status.ExpiresAt = &expiry
	}
	return status, nil
}

func (s *Service) Begin(ctx context.Context, name, actor string) (Status, error) {
	p, err := s.get(name)
	if err != nil {
		return Status{}, err
	}
	p.op.Lock()
	defer p.op.Unlock()
	if err := s.ctx.Err(); err != nil {
		return Status{}, err
	}
	if _, err := p.manager.Status(ctx); err == nil {
		return Status{}, fmt.Errorf("sign out before connecting a different subscription")
	} else if !errors.Is(err, llmauth.ErrCredentialNotFound) {
		return Status{}, fmt.Errorf("could not read provider sign-in status")
	}
	p.mu.Lock()
	if p.cancel != nil {
		p.mu.Unlock()
		return Status{}, fmt.Errorf("a sign-in is already in progress for this profile")
	}
	loginCtx, cancel := context.WithTimeout(s.ctx, loginTimeout)
	expiry, _ := loginCtx.Deadline()
	p.owner = actor
	p.status = Status{Profile: name, State: "starting", LoginExpiresAt: &expiry}
	p.cancel, p.done = cancel, make(chan struct{})
	initial, done := p.status, p.done
	p.mu.Unlock()
	go func() {
		defer close(done)
		defer cancel()
		_, err := p.manager.LoginDevice(loginCtx, func(code openaiauth.DeviceCode) {
			p.mu.Lock()
			p.status.State, p.status.VerificationURL, p.status.UserCode = "pending", code.VerificationURL, code.UserCode
			p.mu.Unlock()
		})
		p.mu.Lock()
		defer p.mu.Unlock()
		p.cancel = nil
		p.status = Status{Profile: name, State: "connected"}
		if err != nil {
			p.status.State = "failed"
			p.status.Message = "Sign-in failed. Check that device-code login is enabled in ChatGPT security settings or workspace permissions, then try again."
			if errors.Is(loginCtx.Err(), context.DeadlineExceeded) {
				p.status.State, p.status.Message = "expired", "The sign-in code expired. Start again to request a new code."
			} else if loginCtx.Err() != nil {
				p.status.State, p.status.Message = "signed_out", "Sign-in cancelled."
			}
		}
	}()
	return initial, nil
}

// Cancel joins the login worker before returning, so a completed token exchange
// cannot recreate credentials after a subsequent logout.
func (s *Service) Cancel(ctx context.Context, name, actor string, logout bool) error {
	p, err := s.get(name)
	if err != nil {
		return err
	}
	p.op.Lock()
	defer p.op.Unlock()
	p.mu.Lock()
	if p.cancel != nil && p.owner != actor && !logout {
		p.mu.Unlock()
		return fmt.Errorf("sign-in belongs to another administrator")
	}
	cancel, done := p.cancel, p.done
	if cancel != nil {
		cancel()
	}
	p.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if logout {
		err = p.manager.Logout(ctx)
		// The library removes local credentials even if remote revocation fails.
		if err != nil {
			err = fmt.Errorf("sign-out could not complete remote revocation; check status and retry if still connected")
		}
	}
	p.mu.Lock()
	p.status = Status{Profile: name, State: "signed_out"}
	p.mu.Unlock()
	return err
}

func (s *Service) Apply(ctx context.Context, name string, request *http.Request) error {
	p, err := s.get(name)
	if err != nil {
		return err
	}
	if err := p.manager.Apply(ctx, request); err != nil {
		return fmt.Errorf("subscription authentication failed; reconnect the provider")
	}
	request.Header.Set("originator", "sparkroute")
	return nil
}

func (s *Service) RecoverUnauthorized(ctx context.Context, name string) error {
	p, err := s.get(name)
	if err != nil {
		return err
	}
	if err := p.manager.RecoverUnauthorized(ctx); err != nil {
		return fmt.Errorf("subscription authentication could not be renewed; reconnect the provider")
	}
	return nil
}

// Close stops and joins sign-in workers before the credential store is closed.
func (s *Service) Close() {
	s.stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.profiles {
		p.op.Lock()
		p.mu.Lock()
		done := p.done
		p.mu.Unlock()
		if done != nil {
			<-done
		}
		p.op.Unlock()
	}
}
