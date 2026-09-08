// Package lifecycle defines on-demand model-serving lifecycle contracts.
package lifecycle

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/sparksq/sparkroute/pkg/endpointregistry"
)

type Binding struct {
	IdleAction         string
	Controller         string
	Revision           string
	VirtualModel       string
	Deployment         string
	Recipe             string
	RecipeRevision     string
	ClusterCandidates  []string
	Overrides          map[string]string
	ActivationTimeout  time.Duration
	IdleTTL            time.Duration
	MaxQueuedWaiters   int
	MaxQueuedBodyBytes int64
	ColdStart          ColdStartBehavior
	// FencingToken is populated only for the activation owner. Controllers
	// publishing shared state must reject writes from an older token.
	FencingToken int64
}

type ColdStartBehavior string

const (
	ColdStartWait   ColdStartBehavior = "wait"
	ColdStartReject ColdStartBehavior = "reject"
)

type RequestFeatures struct {
	VirtualModel string
	Protocol     string
	Streaming    bool
}

type Lease struct {
	ID       string
	Endpoint endpointregistry.Endpoint
}

type RequestOutcome struct {
	Success bool
	Error   string
}

type Status struct {
	PluginsInUse     []string
	LifecycleActions []string
	Owned            *bool
	Phase            string
	JobID            string
	State            endpointregistry.State
	Endpoint         *endpointregistry.Endpoint
	UpdatedAt        time.Time
	Reason           string
}

type StopReason string

const (
	StopIdleTTL  StopReason = "idle_ttl"
	StopOperator StopReason = "operator"
	StopShutdown StopReason = "shutdown"
)

type Controller interface {
	EnsureReady(ctx context.Context, binding Binding, request RequestFeatures) (Lease, error)
	Release(ctx context.Context, lease Lease, outcome RequestOutcome) error
	Status(ctx context.Context, binding Binding) (Status, error)
	Stop(ctx context.Context, binding Binding, reason StopReason) error
}

type ActivationDisposition string

const (
	ActivationOwner    ActivationDisposition = "owner"
	ActivationObserver ActivationDisposition = "observer"
)

type ActivationClaim struct {
	Key          string
	Holder       string
	FencingToken int64
	Disposition  ActivationDisposition
	ExpiresAt    time.Time
}

type ActivationCoordinator interface {
	Begin(ctx context.Context, key, holder string, ttl time.Duration) (ActivationClaim, error)
	Renew(ctx context.Context, claim ActivationClaim, ttl time.Duration) (ActivationClaim, error)
	Complete(ctx context.Context, claim ActivationClaim) error
}

var (
	ErrInvalidActivationClaim = errors.New("invalid activation claim")
	ErrActivationClaimLost    = errors.New("activation claim was lost")
)

// MemoryActivationCoordinator serializes activation ownership in one process.
// It is intentionally not suitable for coordinating multiple replicas.
type MemoryActivationCoordinator struct {
	mu     sync.Mutex
	claims map[string]ActivationClaim
	next   int64
	now    func() time.Time
}

func NewMemoryActivationCoordinator() *MemoryActivationCoordinator {
	return &MemoryActivationCoordinator{
		claims: make(map[string]ActivationClaim),
		now:    time.Now,
	}
}

func (c *MemoryActivationCoordinator) Begin(
	ctx context.Context,
	key string,
	holder string,
	ttl time.Duration,
) (ActivationClaim, error) {
	if err := ctx.Err(); err != nil {
		return ActivationClaim{}, err
	}
	if key == "" || holder == "" || ttl <= 0 {
		return ActivationClaim{}, ErrInvalidActivationClaim
	}
	now := c.now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, exists := c.claims[key]; exists && existing.ExpiresAt.After(now) {
		if existing.Holder == holder {
			existing.Disposition = ActivationOwner
			return existing, nil
		}
		existing.Disposition = ActivationObserver
		return existing, nil
	}
	c.next++
	claim := ActivationClaim{
		Key:          key,
		Holder:       holder,
		FencingToken: c.next,
		Disposition:  ActivationOwner,
		ExpiresAt:    now.Add(ttl),
	}
	c.claims[key] = claim
	return claim, nil
}

func (c *MemoryActivationCoordinator) Renew(
	ctx context.Context,
	claim ActivationClaim,
	ttl time.Duration,
) (ActivationClaim, error) {
	if err := ctx.Err(); err != nil {
		return ActivationClaim{}, err
	}
	if ttl <= 0 {
		return ActivationClaim{}, ErrInvalidActivationClaim
	}
	now := c.now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	current, exists := c.claims[claim.Key]
	if !exists || !sameActivationOwner(current, claim) || !current.ExpiresAt.After(now) {
		return ActivationClaim{}, ErrActivationClaimLost
	}
	current.ExpiresAt = now.Add(ttl)
	current.Disposition = ActivationOwner
	c.claims[claim.Key] = current
	return current, nil
}

func (c *MemoryActivationCoordinator) Complete(
	ctx context.Context,
	claim ActivationClaim,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	current, exists := c.claims[claim.Key]
	if !exists || !sameActivationOwner(current, claim) ||
		!current.ExpiresAt.After(c.now().UTC()) {
		return ErrActivationClaimLost
	}
	delete(c.claims, claim.Key)
	return nil
}

func sameActivationOwner(left, right ActivationClaim) bool {
	return left.Key == right.Key && left.Holder == right.Holder &&
		left.FencingToken == right.FencingToken &&
		right.Disposition == ActivationOwner
}

func ActivationKey(binding Binding) string {
	return strconv.Itoa(len(binding.Controller)) + ":" + binding.Controller + binding.Revision
}

var _ ActivationCoordinator = (*MemoryActivationCoordinator)(nil)
