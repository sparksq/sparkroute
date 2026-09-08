package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sparksq/sparkroute/pkg/endpointregistry"
	"github.com/sparksq/sparkroute/pkg/ledger"
)

type EndpointSource string

const (
	EndpointStatic      EndpointSource = "static"
	EndpointDiscovered  EndpointSource = "discovered"
	EndpointActivatable EndpointSource = "activatable"
)

var (
	ErrUnknownDeployment = errors.New("unknown lifecycle deployment")
	ErrEndpointNotReady  = errors.New("no ready endpoint")
	ErrColdStartRejected = errors.New("cold start rejected")
	ErrQueueFull         = errors.New("cold-start waiter limit exceeded")
	ErrQueueBytes        = errors.New("cold-start queued body-byte limit exceeded")
	ErrInvalidEndpoint   = errors.New("controller returned an invalid endpoint")
)

type AdmissionError struct {
	Reason     string
	RetryAfter time.Duration
	Cause      error
}

func (e *AdmissionError) Error() string {
	return e.Reason
}

func (e *AdmissionError) Unwrap() error {
	return e.Cause
}

// Target binds one deployment to static, discovered, or activatable endpoint
// behavior. Activatable targets carry only published configuration values.
type Target struct {
	Deployment    string
	UpstreamModel string
	LogicalModels []string
	Protocol      string
	Source        EndpointSource
	Controller    string
	Binding       Binding
}

type AdmissionRequest struct {
	Deployment string
	Features   RequestFeatures
	BodyBytes  int64
}

type AdmissionLease struct {
	Endpoint endpointregistry.Endpoint

	coordinator     *AdmissionCoordinator
	deployment      string
	controller      Controller
	controllerLease *Lease
	released        atomic.Bool
}

type EndpointAuthorizer interface {
	AuthorizeEndpoint(ctx context.Context, endpoint endpointregistry.Endpoint) error
}

type EndpointAuthorizerFunc func(context.Context, endpointregistry.Endpoint) error

func (f EndpointAuthorizerFunc) AuthorizeEndpoint(
	ctx context.Context,
	endpoint endpointregistry.Endpoint,
) error {
	return f(ctx, endpoint)
}

type AdmissionOptions struct {
	Registry     endpointregistry.Registry
	Inspector    endpointregistry.Inspector
	Controllers  map[string]Controller
	Activations  ActivationCoordinator
	Authorizer   EndpointAuthorizer
	Recorder     ledger.Recorder
	InstanceID   string
	PollInterval time.Duration
	RetryAfter   time.Duration
	Now          func() time.Time
}

type AdmissionCoordinator struct {
	registry     endpointregistry.Registry
	inspector    endpointregistry.Inspector
	controllers  map[string]Controller
	activations  ActivationCoordinator
	authorizer   EndpointAuthorizer
	recorder     ledger.Recorder
	instanceID   string
	pollInterval time.Duration
	retryAfter   time.Duration
	now          func() time.Time

	mu             sync.Mutex
	targets        map[string]Target
	bindings       map[string]*admissionBindingState
	endpointActive map[string]int
	eventCounter   atomic.Uint64
}

type admissionBindingState struct {
	state              endpointregistry.State
	updatedAt          time.Time
	activationStarted  *time.Time
	activationDeadline *time.Time
	endpointID         string
	activeLeases       int
	queuedWaiters      int
	queuedBodyBytes    int64
	reason             string
}

func NewAdmissionCoordinator(
	targets []Target,
	options AdmissionOptions,
) (*AdmissionCoordinator, error) {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	if options.PollInterval == 0 {
		options.PollInterval = 100 * time.Millisecond
	}
	if options.PollInterval < 10*time.Millisecond || options.PollInterval > time.Minute {
		return nil, fmt.Errorf("lifecycle poll interval must be between 10ms and 1m")
	}
	if options.RetryAfter == 0 {
		options.RetryAfter = time.Second
	}
	if options.RetryAfter < 0 || options.RetryAfter > time.Minute {
		return nil, fmt.Errorf("lifecycle retry-after must be between zero and 1m")
	}
	if options.InstanceID == "" {
		value := make([]byte, 16)
		if _, err := rand.Read(value); err != nil {
			return nil, fmt.Errorf("create lifecycle instance ID: %w", err)
		}
		options.InstanceID = hex.EncodeToString(value)
	}
	if len(options.InstanceID) > 128 {
		return nil, fmt.Errorf("lifecycle instance ID exceeds 128 bytes")
	}
	if options.Activations == nil {
		options.Activations = NewMemoryActivationCoordinator()
	}
	if options.Recorder == nil {
		options.Recorder = ledger.DiscardRecorder{}
	}
	if options.Inspector == nil {
		options.Inspector, _ = options.Registry.(endpointregistry.Inspector)
	}
	coordinator := &AdmissionCoordinator{
		registry:       options.Registry,
		inspector:      options.Inspector,
		controllers:    cloneControllers(options.Controllers),
		activations:    options.Activations,
		authorizer:     options.Authorizer,
		recorder:       options.Recorder,
		instanceID:     options.InstanceID,
		pollInterval:   options.PollInterval,
		retryAfter:     options.RetryAfter,
		now:            now,
		targets:        make(map[string]Target, len(targets)),
		bindings:       make(map[string]*admissionBindingState),
		endpointActive: make(map[string]int),
	}
	controllers := make(map[string]struct{})
	activationBindings := 0
	for index, target := range targets {
		if err := coordinator.addTarget(target); err != nil {
			return nil, fmt.Errorf("lifecycle targets[%d]: %w", index, err)
		}
		configured := coordinator.targets[target.Deployment]
		if configured.Source != EndpointStatic && configured.Controller != "" {
			controllers[configured.Controller] = struct{}{}
		}
		if configured.Source == EndpointActivatable {
			activationBindings++
		}
	}
	if len(controllers) > MaxStatusControllers {
		return nil, fmt.Errorf("lifecycle targets exceed %d controllers", MaxStatusControllers)
	}
	if activationBindings > MaxStatusBindings {
		return nil, fmt.Errorf("lifecycle targets exceed %d activation bindings", MaxStatusBindings)
	}
	return coordinator, nil
}

func cloneControllers(source map[string]Controller) map[string]Controller {
	result := make(map[string]Controller, len(source))
	for name, controller := range source {
		result[name] = controller
	}
	return result
}

func (c *AdmissionCoordinator) addTarget(target Target) error {
	if target.Deployment == "" || len(target.Deployment) > 1024 {
		return fmt.Errorf("deployment is required and must not exceed 1024 bytes")
	}
	if _, exists := c.targets[target.Deployment]; exists {
		return fmt.Errorf("duplicate deployment %q", target.Deployment)
	}
	if target.Source == "" {
		target.Source = EndpointStatic
	}
	switch target.Source {
	case EndpointStatic:
	case EndpointDiscovered:
		if target.Controller == "" {
			return fmt.Errorf("discovered target controller is required")
		}
		if c.registry == nil || c.authorizer == nil {
			return fmt.Errorf("dynamic targets require a registry and endpoint authorizer")
		}
	case EndpointActivatable:
		binding := target.Binding
		if binding.Controller == "" {
			binding.Controller = target.Controller
		}
		if binding.Deployment == "" {
			binding.Deployment = target.Deployment
		}
		if binding.Controller == "" || binding.Revision == "" || binding.Recipe == "" ||
			binding.ActivationTimeout <= 0 || binding.MaxQueuedWaiters <= 0 ||
			binding.MaxQueuedBodyBytes <= 0 {
			return fmt.Errorf("activatable target has an incomplete binding")
		}
		if binding.Deployment != target.Deployment {
			return fmt.Errorf("binding deployment does not match target")
		}
		if binding.ColdStart == "" {
			binding.ColdStart = ColdStartWait
		}
		if binding.ColdStart != ColdStartWait && binding.ColdStart != ColdStartReject {
			return fmt.Errorf("unsupported cold-start behavior %q", binding.ColdStart)
		}
		if c.registry == nil || c.authorizer == nil {
			return fmt.Errorf("dynamic targets require a registry and endpoint authorizer")
		}
		if c.controllers[binding.Controller] == nil {
			return fmt.Errorf("controller %q is not configured", binding.Controller)
		}
		target.Controller = binding.Controller
		target.Binding = cloneBinding(binding)
		key := ActivationKey(binding)
		if _, exists := c.bindings[key]; exists {
			return fmt.Errorf("duplicate activation binding %q", binding.Revision)
		}
		now := c.now().UTC()
		c.bindings[key] = &admissionBindingState{
			state: endpointregistry.StateOffline, updatedAt: now,
		}
	default:
		return fmt.Errorf("unsupported endpoint source %q", target.Source)
	}
	c.targets[target.Deployment] = target
	return nil
}

func cloneBinding(binding Binding) Binding {
	binding.ClusterCandidates = append([]string(nil), binding.ClusterCandidates...)
	if binding.Overrides != nil {
		overrides := make(map[string]string, len(binding.Overrides))
		for name, value := range binding.Overrides {
			overrides[name] = value
		}
		binding.Overrides = overrides
	}
	return binding
}

func (c *AdmissionCoordinator) IsDynamic(deployment string) bool {
	target, exists := c.targets[deployment]
	return exists && target.Source != EndpointStatic
}

func (c *AdmissionCoordinator) IsActivatable(deployment string) bool {
	target, exists := c.targets[deployment]
	return exists && target.Source == EndpointActivatable
}

// Eligible is a non-mutating routing hint. Dynamic readiness and admission are
// rechecked by Acquire; activatable targets remain eligible while cold.
func (c *AdmissionCoordinator) Eligible(deployment string) bool {
	target, exists := c.targets[deployment]
	if !exists {
		return false
	}
	switch target.Source {
	case EndpointStatic:
		return true
	case EndpointDiscovered:
		return c.registry != nil
	case EndpointActivatable:
		return c.registry != nil && c.authorizer != nil &&
			c.controllers[target.Binding.Controller] != nil
	default:
		return false
	}
}

func (c *AdmissionCoordinator) Acquire(
	ctx context.Context,
	request AdmissionRequest,
) (*AdmissionLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.BodyBytes < 0 {
		return nil, fmt.Errorf("queued body bytes must not be negative")
	}
	target, exists := c.targets[request.Deployment]
	if !exists {
		return nil, ErrUnknownDeployment
	}
	switch target.Source {
	case EndpointStatic:
		return nil, nil
	case EndpointDiscovered:
		for attempts := 0; attempts < 2; attempts++ {
			endpoint, err := c.readyEndpoint(ctx, target)
			if err != nil {
				break
			}
			lease, err := c.admitEndpoint(target, endpoint, nil, nil, true)
			if err == nil {
				return lease, nil
			}
		}
		return nil, c.admissionError("endpoint_not_ready", ErrEndpointNotReady)
	case EndpointActivatable:
		return c.acquireActivatable(ctx, target, request)
	default:
		return nil, ErrUnknownDeployment
	}
}

func (c *AdmissionCoordinator) acquireActivatable(
	ctx context.Context,
	target Target,
	request AdmissionRequest,
) (*AdmissionLease, error) {
	binding := target.Binding
	ready, readyErr := c.readyEndpoint(ctx, target)
	if readyErr == nil && binding.ColdStart == ColdStartReject {
		return c.acquireControllerLease(ctx, target, request.Features, ready)
	}
	if readyErr != nil && binding.ColdStart == ColdStartReject {
		c.recordRejected(binding, request.Features.VirtualModel, "cold_start_rejected")
		return nil, c.admissionError("cold_start_rejected", ErrColdStartRejected)
	}
	if readyErr == nil {
		return c.acquireControllerLease(ctx, target, request.Features, ready)
	}
	if err := c.enqueue(binding, request.BodyBytes); err != nil {
		if endpoint, readyErr := c.readyEndpoint(ctx, target); readyErr == nil {
			return c.acquireControllerLease(ctx, target, request.Features, endpoint)
		}
		reason := "queue_waiters_exceeded"
		if errors.Is(err, ErrQueueBytes) {
			reason = "queue_bytes_exceeded"
		}
		c.recordRejected(binding, request.Features.VirtualModel, reason)
		return nil, err
	}
	defer c.dequeue(binding, request.BodyBytes)

	activationCtx, cancel := context.WithTimeout(ctx, binding.ActivationTimeout)
	defer cancel()
	holder := fmt.Sprintf("%s-%d", c.instanceID, c.eventCounter.Add(1))
	for {
		claim, err := c.activations.Begin(
			activationCtx,
			ActivationKey(binding),
			holder,
			binding.ActivationTimeout,
		)
		if err != nil {
			return nil, c.activationFailure(binding, request.Features.VirtualModel, err)
		}
		if claim.Disposition == ActivationOwner {
			return c.activateOwner(activationCtx, target, request.Features, claim)
		}
		endpoint, observed, err := c.observeActivation(activationCtx, target, binding, claim)
		if err != nil {
			return nil, c.activationFailure(binding, request.Features.VirtualModel, err)
		}
		if observed {
			return c.acquireControllerLease(activationCtx, target, request.Features, endpoint)
		}
	}
}

func (c *AdmissionCoordinator) activateOwner(
	ctx context.Context,
	target Target,
	features RequestFeatures,
	claim ActivationClaim,
) (*AdmissionLease, error) {
	binding := target.Binding
	c.transition(binding, features.VirtualModel, endpointregistry.StateActivating, "activation_started", ledger.RuntimeOutcomeSuccess, nil, nil)
	started := c.now()
	controller := c.controllers[binding.Controller]
	ownerBinding := cloneBinding(binding)
	ownerBinding.FencingToken = claim.FencingToken
	lease, err := controller.EnsureReady(ctx, ownerBinding, features)
	if err != nil {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 5*time.Second)
		_ = c.activations.Complete(cleanupCtx, claim)
		cancelCleanup()
		return nil, c.activationFailure(binding, features.VirtualModel, err)
	}
	if lease.Endpoint.BindingRevision != binding.Revision ||
		lease.Endpoint.FencingToken != claim.FencingToken {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 5*time.Second)
		_ = c.activations.Complete(cleanupCtx, claim)
		_ = controller.Release(cleanupCtx, lease, RequestOutcome{Error: "invalid_fencing_token"})
		cancelCleanup()
		return nil, c.activationFailure(binding, features.VirtualModel, ErrInvalidEndpoint)
	}
	endpoint, err := c.validateControllerLease(ctx, target, lease, endpointregistry.Endpoint{})
	if err != nil {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 5*time.Second)
		_ = c.activations.Complete(cleanupCtx, claim)
		_ = controller.Release(cleanupCtx, lease, RequestOutcome{Error: "invalid_endpoint"})
		cancelCleanup()
		return nil, c.activationFailure(binding, features.VirtualModel, err)
	}
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 5*time.Second)
	completeErr := c.activations.Complete(cleanupCtx, claim)
	cancelCleanup()
	if completeErr != nil {
		cleanupCtx, cancelCleanup = context.WithTimeout(context.Background(), 5*time.Second)
		_ = controller.Release(cleanupCtx, lease, RequestOutcome{Error: "activation_claim_lost"})
		cancelCleanup()
		return nil, c.activationFailure(binding, features.VirtualModel, completeErr)
	}
	latency := c.now().Sub(started)
	c.transition(binding, features.VirtualModel, endpointregistry.StateReady, "activation_ready", ledger.RuntimeOutcomeSuccess, &endpoint, &latency)
	return c.admitEndpoint(target, endpoint, controller, &lease, false)
}

func (c *AdmissionCoordinator) acquireControllerLease(
	ctx context.Context,
	target Target,
	features RequestFeatures,
	expected endpointregistry.Endpoint,
) (*AdmissionLease, error) {
	controller := c.controllers[target.Binding.Controller]
	lease, err := controller.EnsureReady(ctx, cloneBinding(target.Binding), features)
	if err != nil {
		return nil, c.activationFailure(target.Binding, features.VirtualModel, err)
	}
	endpoint, err := c.validateControllerLease(ctx, target, lease, expected)
	if err != nil {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 5*time.Second)
		_ = controller.Release(cleanupCtx, lease, RequestOutcome{Error: "invalid_endpoint"})
		cancelCleanup()
		return nil, c.activationFailure(target.Binding, features.VirtualModel, err)
	}
	return c.admitEndpoint(target, endpoint, controller, &lease, false)
}

func (c *AdmissionCoordinator) observeActivation(
	ctx context.Context,
	target Target,
	binding Binding,
	claim ActivationClaim,
) (endpointregistry.Endpoint, bool, error) {
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	for {
		if endpoint, err := c.readyEndpoint(ctx, target); err == nil {
			return endpoint, true, nil
		}
		status, err := c.controllers[binding.Controller].Status(ctx, cloneBinding(binding))
		if err == nil {
			if status.State == endpointregistry.StateFailed {
				return endpointregistry.Endpoint{}, false, fmt.Errorf("controller reported activation failure")
			}
			if status.State == endpointregistry.StateReady && status.Endpoint != nil {
				if endpoint, validationErr := c.validateEndpoint(ctx, target, *status.Endpoint); validationErr == nil {
					return endpoint, true, nil
				}
			}
		}
		if !c.now().Before(claim.ExpiresAt) {
			return endpointregistry.Endpoint{}, false, nil
		}
		select {
		case <-ctx.Done():
			return endpointregistry.Endpoint{}, false, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *AdmissionCoordinator) validateControllerLease(
	ctx context.Context,
	target Target,
	lease Lease,
	expected endpointregistry.Endpoint,
) (endpointregistry.Endpoint, error) {
	if lease.ID == "" {
		return endpointregistry.Endpoint{}, fmt.Errorf("%w: lease ID is empty", ErrInvalidEndpoint)
	}
	endpoint, err := c.validateEndpoint(ctx, target, lease.Endpoint)
	if err != nil {
		return endpointregistry.Endpoint{}, err
	}
	if expected.ID != "" && endpoint.ID != expected.ID {
		return endpointregistry.Endpoint{}, fmt.Errorf("%w: lease endpoint changed", ErrInvalidEndpoint)
	}
	registered, err := c.registry.Ready(ctx, target.Deployment)
	if err != nil {
		return endpointregistry.Endpoint{}, fmt.Errorf("%w: registry read failed", ErrInvalidEndpoint)
	}
	for _, candidate := range registered {
		candidate, err = c.validateEndpoint(ctx, target, candidate)
		if err == nil && sameRuntimeRegistration(candidate, endpoint) {
			return candidate, nil
		}
	}
	return endpointregistry.Endpoint{}, fmt.Errorf("%w: endpoint is not ready in registry", ErrInvalidEndpoint)
}

func (c *AdmissionCoordinator) readyEndpoint(
	ctx context.Context,
	target Target,
) (endpointregistry.Endpoint, error) {
	endpoints, err := c.registry.Ready(ctx, target.Deployment)
	if err != nil {
		return endpointregistry.Endpoint{}, err
	}
	sort.Slice(endpoints, func(i, j int) bool { return endpoints[i].ID < endpoints[j].ID })
	for _, endpoint := range endpoints {
		endpoint, err = c.validateEndpoint(ctx, target, endpoint)
		if err != nil {
			continue
		}
		c.mu.Lock()
		active := c.endpointActive[endpoint.ID]
		c.mu.Unlock()
		if endpoint.MaxConcurrency > 0 && endpoint.ActiveRequests+active >= endpoint.MaxConcurrency {
			continue
		}
		return endpoint, nil
	}
	return endpointregistry.Endpoint{}, ErrEndpointNotReady
}

func (c *AdmissionCoordinator) validateEndpoint(
	ctx context.Context,
	target Target,
	endpoint endpointregistry.Endpoint,
) (endpointregistry.Endpoint, error) {
	if err := endpointregistry.ValidateEndpoint(endpoint); err != nil {
		return endpointregistry.Endpoint{}, fmt.Errorf("%w: malformed registration", ErrInvalidEndpoint)
	}
	if endpoint.Target != target.Deployment || endpoint.State != endpointregistry.StateReady {
		return endpointregistry.Endpoint{}, fmt.Errorf("%w: target or state mismatch", ErrInvalidEndpoint)
	}
	if target.Source != EndpointStatic && endpoint.Controller != target.Controller {
		return endpointregistry.Endpoint{}, fmt.Errorf("%w: controller mismatch", ErrInvalidEndpoint)
	}
	if target.Source == EndpointActivatable {
		if endpoint.BindingRevision != target.Binding.Revision || endpoint.FencingToken <= 0 {
			return endpointregistry.Endpoint{}, fmt.Errorf("%w: activation binding mismatch", ErrInvalidEndpoint)
		}
		if target.Binding.RecipeRevision != "" &&
			endpoint.RecipeRevision != target.Binding.RecipeRevision {
			return endpointregistry.Endpoint{}, fmt.Errorf("%w: recipe revision mismatch", ErrInvalidEndpoint)
		}
	}
	if target.Protocol != "" && endpoint.Protocol != target.Protocol {
		return endpointregistry.Endpoint{}, fmt.Errorf("%w: protocol mismatch", ErrInvalidEndpoint)
	}
	if target.UpstreamModel != "" && len(endpoint.ServedModels) > 0 {
		served := false
		for _, model := range endpoint.ServedModels {
			if model == target.UpstreamModel {
				served = true
				break
			}
		}
		if !served {
			return endpointregistry.Endpoint{}, fmt.Errorf("%w: served model mismatch", ErrInvalidEndpoint)
		}
	}
	if err := c.authorizer.AuthorizeEndpoint(ctx, endpoint); err != nil {
		return endpointregistry.Endpoint{}, fmt.Errorf("%w: endpoint is not authorized", ErrInvalidEndpoint)
	}
	return endpoint, nil
}

func sameRuntimeRegistration(left, right endpointregistry.Endpoint) bool {
	return left.ID == right.ID && left.Target == right.Target &&
		left.BaseURL == right.BaseURL && left.Protocol == right.Protocol &&
		left.Controller == right.Controller && left.ClusterID == right.ClusterID &&
		left.JobID == right.JobID && left.BindingRevision == right.BindingRevision &&
		left.RecipeRevision == right.RecipeRevision &&
		left.FencingToken == right.FencingToken && left.State == right.State &&
		left.MaxConcurrency == right.MaxConcurrency &&
		slices.Equal(left.ServedModels, right.ServedModels)
}

func (c *AdmissionCoordinator) enqueue(binding Binding, bodyBytes int64) error {
	key := ActivationKey(binding)
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.bindings[key]
	if state.queuedWaiters >= binding.MaxQueuedWaiters {
		return c.admissionError("queue_waiters_exceeded", ErrQueueFull)
	}
	if bodyBytes > binding.MaxQueuedBodyBytes-state.queuedBodyBytes {
		return c.admissionError("queue_bytes_exceeded", ErrQueueBytes)
	}
	state.queuedWaiters++
	state.queuedBodyBytes += bodyBytes
	state.updatedAt = c.now().UTC()
	return nil
}

func (c *AdmissionCoordinator) dequeue(binding Binding, bodyBytes int64) {
	c.mu.Lock()
	state := c.bindings[ActivationKey(binding)]
	if state.queuedWaiters > 0 {
		state.queuedWaiters--
	}
	if bodyBytes <= state.queuedBodyBytes {
		state.queuedBodyBytes -= bodyBytes
	} else {
		state.queuedBodyBytes = 0
	}
	state.updatedAt = c.now().UTC()
	c.mu.Unlock()
}

func (c *AdmissionCoordinator) admitEndpoint(
	target Target,
	endpoint endpointregistry.Endpoint,
	controller Controller,
	controllerLease *Lease,
	enforceCapacity bool,
) (*AdmissionLease, error) {
	c.mu.Lock()
	if enforceCapacity && endpoint.MaxConcurrency > 0 &&
		endpoint.ActiveRequests+c.endpointActive[endpoint.ID] >= endpoint.MaxConcurrency {
		c.mu.Unlock()
		return nil, ErrEndpointNotReady
	}
	c.endpointActive[endpoint.ID]++
	if target.Source == EndpointActivatable {
		state := c.bindings[ActivationKey(target.Binding)]
		state.activeLeases++
		state.endpointID = endpoint.ID
		state.state = endpointregistry.StateReady
		state.updatedAt = c.now().UTC()
		state.reason = ""
	}
	c.mu.Unlock()
	return &AdmissionLease{
		Endpoint: endpoint, coordinator: c, deployment: target.Deployment,
		controller: controller, controllerLease: controllerLease,
	}, nil
}

func (c *AdmissionCoordinator) Release(
	ctx context.Context,
	lease *AdmissionLease,
	outcome RequestOutcome,
) error {
	if lease == nil || lease.coordinator != c || !lease.released.CompareAndSwap(false, true) {
		return nil
	}
	c.mu.Lock()
	if c.endpointActive[lease.Endpoint.ID] > 1 {
		c.endpointActive[lease.Endpoint.ID]--
	} else {
		delete(c.endpointActive, lease.Endpoint.ID)
	}
	target := c.targets[lease.deployment]
	if target.Source == EndpointActivatable {
		state := c.bindings[ActivationKey(target.Binding)]
		if state.activeLeases > 0 {
			state.activeLeases--
		}
		state.updatedAt = c.now().UTC()
	}
	c.mu.Unlock()
	if lease.controller != nil && lease.controllerLease != nil {
		return lease.controller.Release(ctx, *lease.controllerLease, outcome)
	}
	return nil
}

func (c *AdmissionCoordinator) transition(
	binding Binding,
	virtualModel string,
	newState endpointregistry.State,
	reason string,
	outcome ledger.RuntimeOutcome,
	endpoint *endpointregistry.Endpoint,
	latency *time.Duration,
) {
	now := c.now().UTC()
	c.mu.Lock()
	state := c.bindings[ActivationKey(binding)]
	prior := state.state
	state.state = newState
	state.updatedAt = now
	state.reason = reason
	if newState == endpointregistry.StateActivating {
		started := now
		deadline := now.Add(binding.ActivationTimeout)
		state.activationStarted = &started
		state.activationDeadline = &deadline
	} else {
		state.activationStarted = nil
		state.activationDeadline = nil
	}
	if endpoint != nil {
		state.endpointID = endpoint.ID
	}
	waiters := state.queuedWaiters
	c.mu.Unlock()
	event := ledger.RuntimeEventRecord{
		EventID:         c.eventID(now),
		OccurredAt:      now,
		Controller:      binding.Controller,
		BindingRevision: binding.Revision,
		VirtualModel:    virtualModel,
		Deployment:      binding.Deployment,
		RecipeRevision:  binding.RecipeRevision,
		PriorState:      string(prior),
		NewState:        string(newState),
		Reason:          reason,
		Latency:         latency,
		QueueDepth:      waiters,
		WaiterCount:     waiters,
		Outcome:         outcome,
	}
	if endpoint != nil {
		event.EndpointInstance = endpoint.ID
		event.ClusterID = endpoint.ClusterID
		event.JobID = endpoint.JobID
		if event.RecipeRevision == "" {
			event.RecipeRevision = endpoint.RecipeRevision
		}
	}
	c.recorder.Record(ledger.NewRuntimeEventRecord(event))
}

func (c *AdmissionCoordinator) activationFailure(
	binding Binding,
	virtualModel string,
	err error,
) error {
	reason := "controller_failure"
	outcome := ledger.RuntimeOutcomeFailure
	if errors.Is(err, context.DeadlineExceeded) {
		reason = "activation_timeout"
		outcome = ledger.RuntimeOutcomeTimeout
	} else if errors.Is(err, context.Canceled) {
		reason = "activation_cancelled"
		outcome = ledger.RuntimeOutcomeCancelled
	} else if errors.Is(err, ErrInvalidEndpoint) {
		reason = "invalid_endpoint"
	}
	c.transition(binding, virtualModel, endpointregistry.StateFailed, reason, outcome, nil, nil)
	return c.admissionError(reason, err)
}

func (c *AdmissionCoordinator) recordRejected(
	binding Binding,
	virtualModel string,
	reason string,
) {
	c.mu.Lock()
	state := c.bindings[ActivationKey(binding)]
	current := state.state
	c.mu.Unlock()
	c.transition(binding, virtualModel, current, reason, ledger.RuntimeOutcomeRejected, nil, nil)
}

func (c *AdmissionCoordinator) admissionError(reason string, cause error) *AdmissionError {
	return &AdmissionError{Reason: reason, RetryAfter: c.retryAfter, Cause: cause}
}

func (c *AdmissionCoordinator) eventID(now time.Time) string {
	return fmt.Sprintf("runtime-%s-%d-%d", c.instanceID, now.UnixNano(), c.eventCounter.Add(1))
}

func (c *AdmissionCoordinator) Snapshot(ctx context.Context) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	type controllerAggregate struct {
		status ControllerStatus
	}
	observedAt := c.now().UTC()
	c.mu.Lock()
	targets := make([]Target, 0, len(c.targets))
	for _, target := range c.targets {
		targets = append(targets, target)
	}
	bindings := make(map[string]admissionBindingState, len(c.bindings))
	for key, state := range c.bindings {
		bindings[key] = *state
	}
	c.mu.Unlock()
	aggregates := make(map[string]*controllerAggregate)
	result := Snapshot{ObservedAt: observedAt, Controllers: []ControllerStatus{}, Bindings: []BindingStatus{}}
	for _, target := range targets {
		if target.Source == EndpointStatic {
			continue
		}
		aggregate := aggregates[target.Controller]
		if aggregate == nil {
			aggregate = &controllerAggregate{status: ControllerStatus{
				Controller: target.Controller, Health: ControllerHealthy, UpdatedAt: observedAt,
			}}
			aggregates[target.Controller] = aggregate
		}
		ready, _ := c.registry.Ready(ctx, target.Deployment)
		aggregate.status.ReadyEndpoints += len(ready)
		aggregate.status.Endpoints += len(ready)
		if target.Source != EndpointActivatable {
			continue
		}
		state := bindings[ActivationKey(target.Binding)]
		aggregate.status.Bindings++
		aggregate.status.ActiveLeases += state.activeLeases
		aggregate.status.QueuedWaiters += state.queuedWaiters
		aggregate.status.QueuedBodyBytes += state.queuedBodyBytes
		observed := Status{}
		if controller := c.controllers[target.Controller]; controller != nil {
			observed, _ = controller.Status(ctx, target.Binding)
		}
		if observed.State != "" && state.queuedWaiters == 0 {
			state.state = observed.State
		}
		if state.state == "" {
			state.state = endpointregistry.StateOffline
		}
		if state.updatedAt.IsZero() {
			state.updatedAt = observedAt
		}
		if observed.Reason != "" {
			state.reason = SanitizeReason(observed.Reason)
		}
		result.Bindings = append(result.Bindings, BindingStatus{
			Controller: target.Binding.Controller, BindingRevision: target.Binding.Revision,
			VirtualModel: target.Binding.VirtualModel, Deployment: target.Deployment,
			Phase: observed.Phase, JobID: observed.JobID, Owned: observed.Owned, ClusterCandidates: append([]string(nil), target.Binding.ClusterCandidates...),
			State: state.state, UpdatedAt: state.updatedAt,
			ActivationStarted:  cloneTime(state.activationStarted),
			ActivationDeadline: cloneTime(state.activationDeadline),
			EndpointID:         state.endpointID, ActiveLeases: state.activeLeases,
			QueuedWaiters: state.queuedWaiters, QueuedBodyBytes: state.queuedBodyBytes,
			MaxQueuedWaiters:   target.Binding.MaxQueuedWaiters,
			MaxQueuedBodyBytes: target.Binding.MaxQueuedBodyBytes, Reason: state.reason,
		})
	}
	if c.inspector != nil {
		for controller, aggregate := range aggregates {
			page, err := c.inspector.List(ctx, endpointregistry.Query{
				Controller: controller, Limit: endpointregistry.MaxPageSize,
			})
			if err == nil {
				aggregate.status.Endpoints = len(page.Endpoints)
			}
		}
	}
	for _, aggregate := range aggregates {
		result.Controllers = append(result.Controllers, aggregate.status)
	}
	sort.Slice(result.Controllers, func(i, j int) bool {
		return result.Controllers[i].Controller < result.Controllers[j].Controller
	})
	sort.Slice(result.Bindings, func(i, j int) bool {
		return result.Bindings[i].Deployment < result.Bindings[j].Deployment
	})
	return result, nil
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	result := *value
	return &result
}

var _ StatusSource = (*AdmissionCoordinator)(nil)
