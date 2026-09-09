// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package pii

import (
	"context"
	"fmt"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	privacycontract "github.com/sparksq/sparkroute/pkg/privacy"
)

// Provider adapts the built-in detector and encrypted mapping engine to the
// stable OSS privacy extension contract.
type Provider struct {
	detector  Detector
	directory *Directory
	monitor   *Monitor
}

func NewProvider(document config.Document, detector Detector, directory *Directory) (*Provider, error) {
	resolved, err := document.ResolveModelPolicies()
	if err != nil {
		return nil, err
	}
	document = resolved
	if detector == nil {
		return nil, fmt.Errorf("PII detector is required")
	}
	monitorOptions := MonitorOptions{Detector: detector, Directory: directory}
	for _, model := range document.VirtualModels {
		if model.Privacy == nil {
			continue
		}
		policy := model.Privacy.PII.Effective()
		if policy.Mode == config.PIIModeDisabled {
			continue
		}
		monitorOptions.ConfiguredModels++
		if policy.Scope == config.PIIScopeConversation {
			if directory == nil {
				return nil, fmt.Errorf("PII conversation scope requires an encrypted mapping directory")
			}
			monitorOptions.ConversationModels++
		}
		if policy.MediaText == config.PIIMediaTextRequired {
			monitorOptions.MediaRequiredModels++
		}
		if policy.Files.Text != config.PIIFileTextDisabled || policy.Files.Metadata {
			monitorOptions.FileInspectionModels++
		}
		for _, configured := range policy.Entities {
			entity := Entity(configured)
			if !detector.Supports(entity) {
				return nil, fmt.Errorf("PII detector does not support configured entity %q", entity)
			}
		}
	}
	return &Provider{
		detector: detector, directory: directory,
		monitor: NewMonitor(monitorOptions),
	}, nil
}

// ForDocument reuses immutable detector/key/directory state while rebuilding
// document-derived validation and status counters during config activation.
func (p *Provider) ForDocument(document config.Document) (*Provider, error) {
	if p == nil {
		return nil, fmt.Errorf("PII provider is unavailable")
	}
	return NewProvider(document, p.detector, p.directory)
}

func (p *Provider) NewSession(
	ctx context.Context,
	entities []privacycontract.Entity,
	scope privacycontract.Scope,
) (privacycontract.Session, error) {
	if p == nil || p.detector == nil {
		return nil, fmt.Errorf("PII provider is unavailable")
	}
	localEntities := make([]Entity, len(entities))
	for index, entity := range entities {
		localEntities[index] = Entity(entity)
	}
	var (
		session *Session
		err     error
	)
	if scope.Conversation == "" {
		session, err = NewSession(p.detector, localEntities, SessionOptions{})
	} else {
		if p.directory == nil {
			return nil, fmt.Errorf("PII conversation mapping directory is unavailable")
		}
		session, err = p.directory.NewSession(ctx, p.detector, localEntities, ConversationScope{
			Tenant: scope.Tenant, Principal: scope.Principal, Conversation: scope.Conversation,
		})
	}
	if err != nil {
		return nil, err
	}
	return sessionAdapter{session: session}, nil
}

func (p *Provider) Supports(entity privacycontract.Entity) bool {
	return p != nil && p.detector != nil && p.detector.Supports(Entity(entity))
}

func (p *Provider) RecordInspection() { p.monitor.RecordInspection() }

func (p *Provider) RecordFailure() { p.monitor.RecordFailure() }

func (p *Provider) PrivacyStatus(ctx context.Context) privacycontract.Status {
	if p == nil {
		return privacycontract.DisabledStatus()
	}
	return p.monitor.PrivacyStatus(ctx)
}

func (p *Provider) RunPruner(
	ctx context.Context,
	interval time.Duration,
	limit int,
	onError func(error),
) {
	if p != nil && p.directory != nil {
		p.directory.RunPruner(ctx, interval, limit, onError)
	}
}

type sessionAdapter struct{ session *Session }

func (s sessionAdapter) Substitute(ctx context.Context, text string) (string, error) {
	return s.session.Substitute(ctx, text)
}

func (s sessionAdapter) Redact(ctx context.Context, text string) (string, error) {
	return s.session.Redact(ctx, text)
}

func (s sessionAdapter) Restore(text string) string { return s.session.Restore(text) }

func (s sessionAdapter) MappingCount() int { return s.session.MappingCount() }

func (s sessionAdapter) SupportsStreaming() bool { return s.session.SupportsStreaming() }

func (s sessionAdapter) NewStreamRedactor(
	options privacycontract.StreamOptions,
) (privacycontract.StreamRedactor, error) {
	return s.session.NewStreamRedactor(StreamOptions{
		MaximumPendingBytes: options.MaximumPendingBytes,
	})
}

var _ privacycontract.Provider = (*Provider)(nil)
var _ privacycontract.Session = sessionAdapter{}
var _ privacycontract.StreamRedactor = (*StreamRedactor)(nil)
