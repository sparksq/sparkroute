package sparkrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/sparksq/sparkroute/pkg/endpointregistry"
	"github.com/sparksq/sparkroute/pkg/lifecycle"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
)

const ControllerName = "sparkrun"

type Options struct {
	Workloads         *Workloads
	Bridge            Bridge
	Registry          endpointregistry.Registry
	Targets           []lifecycle.Target
	EndpointTTL       time.Duration
	ReconcileInterval time.Duration
	StopTimeout       time.Duration
	Now               func() time.Time
	MetadataPublisher modelrouter.DiscoveredMetadataPublisher
	MetadataSource    string
}

type Controller struct {
	workloads         *Workloads
	ownedWorkloads    bool
	bridge            Bridge
	registry          endpointregistry.Registry
	targets           map[string]lifecycle.Target
	endpointTTL       time.Duration
	reconcileInterval time.Duration
	stopTimeout       time.Duration
	now               func() time.Time
	metadataPublisher modelrouter.DiscoveredMetadataPublisher
	metadataSource    string

	operationMu sync.Mutex
	mu          sync.Mutex
	states      map[string]*bindingState
	leases      map[string]string
	approved    map[string]endpointregistry.Endpoint
	metadata    map[string]modelrouter.DiscoveredModelMetadata
	tracked     map[string]string
	closed      bool
	cancel      context.CancelFunc
	background  context.Context
	cancelTasks context.CancelFunc
	wait        sync.WaitGroup
	nextLease   atomic.Uint64
}

type bindingState struct {
	phase      string
	jobID      string
	binding    lifecycle.Binding
	state      endpointregistry.State
	endpoint   *endpointregistry.Endpoint
	updatedAt  time.Time
	reason     string
	active     int
	timer      *time.Timer
	generation uint64
}

func New(options Options) (*Controller, error) {
	if options.Bridge == nil {
		return nil, fmt.Errorf("sparkrun bridge is required")
	}
	if options.Registry == nil {
		return nil, fmt.Errorf("sparkrun endpoint registry is required")
	}
	if options.EndpointTTL == 0 {
		options.EndpointTTL = 90 * time.Second
	}
	if options.ReconcileInterval == 0 {
		options.ReconcileInterval = 30 * time.Second
	}
	if options.StopTimeout == 0 {
		options.StopTimeout = 2 * time.Minute
	}
	if options.EndpointTTL < 10*time.Second || options.EndpointTTL > 10*time.Minute {
		return nil, fmt.Errorf("sparkrun endpoint TTL must be between 10s and 10m")
	}
	if options.ReconcileInterval < time.Second || options.ReconcileInterval >= options.EndpointTTL {
		return nil, fmt.Errorf("sparkrun reconcile interval must be at least 1s and less than endpoint TTL")
	}
	if options.StopTimeout < time.Second || options.StopTimeout > 10*time.Minute {
		return nil, fmt.Errorf("sparkrun stop timeout must be between 1s and 10m")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.MetadataSource == "" {
		options.MetadataSource = ControllerName
	}
	if options.MetadataPublisher != nil {
		if err := modelrouter.ValidateDiscoveredMetadataSnapshot(modelrouter.DiscoveredMetadataSnapshot{
			Source: options.MetadataSource, Models: map[string]modelrouter.DiscoveredModelMetadata{},
		}); err != nil {
			return nil, fmt.Errorf("sparkrun metadata source: %w", err)
		}
	}
	targets := make(map[string]lifecycle.Target, len(options.Targets))
	for _, target := range options.Targets {
		if target.Controller != ControllerName {
			return nil, fmt.Errorf("unsupported runtime controller %q", target.Controller)
		}
		if target.Source == lifecycle.EndpointActivatable && target.Binding.RecipeRevision == "" {
			return nil, fmt.Errorf("sparkrun deployment %q requires recipe_revision", target.Deployment)
		}
		targets[target.Deployment] = target
	}
	ownedWorkloads := options.Workloads == nil
	if ownedWorkloads {
		options.Workloads = NewWorkloads()
	}
	background, cancelTasks := context.WithCancel(context.Background())
	controller := &Controller{
		workloads: options.Workloads, ownedWorkloads: ownedWorkloads,
		bridge: options.Bridge, registry: options.Registry, targets: targets,
		endpointTTL: options.EndpointTTL, reconcileInterval: options.ReconcileInterval,
		stopTimeout: options.StopTimeout, now: options.Now,
		metadataPublisher: options.MetadataPublisher,
		metadataSource:    options.MetadataSource,
		states:            make(map[string]*bindingState), leases: make(map[string]string),
		approved:   make(map[string]endpointregistry.Endpoint),
		metadata:   make(map[string]modelrouter.DiscoveredModelMetadata),
		tracked:    make(map[string]string),
		background: background, cancelTasks: cancelTasks,
	}
	if client, ok := options.Bridge.(*Client); ok {
		client.OnProgress = func(binding Binding, operation Operation) {
			controller.mu.Lock()
			defer controller.mu.Unlock()
			for _, state := range controller.states {
				if state.binding.RecipeRevision == binding.RecipeRevision && slices.Equal(state.binding.ClusterCandidates, binding.ClusterCandidates) {
					state.phase = lifecycle.SanitizeReason(strings.ReplaceAll(operation.Phase, " ", "_"))
					state.jobID = operation.ClusterID
				}
			}
		}
	}
	return controller, nil
}

func (c *Controller) CheckCapabilities(ctx context.Context) error {
	capabilities, err := c.bridge.Capabilities(ctx)
	if err != nil {
		return err
	}
	if capabilities.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("sparkrun bridge protocol version %d is unsupported", capabilities.ProtocolVersion)
	}
	required := []string{"discover", "ensure_ready", "stop"}
	for _, operation := range required {
		if !slices.Contains(capabilities.Operations, operation) {
			return fmt.Errorf("sparkrun bridge does not support %q", operation)
		}
	}
	return nil
}

// Start runs best-effort discovery reconciliation until ctx is cancelled.
// Reconciliation failures are reported through onError and retried.
func (c *Controller) Start(ctx context.Context, onError func(error)) {
	c.mu.Lock()
	if c.cancel != nil || c.closed {
		c.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	c.wait.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wait.Done()
		if err := c.Reconcile(runCtx); err != nil && onError != nil && runCtx.Err() == nil {
			onError(err)
		}
		ticker := time.NewTicker(c.reconcileInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				if err := c.Reconcile(runCtx); err != nil && onError != nil && runCtx.Err() == nil {
					onError(err)
				}
			}
		}
	}()
}

func (c *Controller) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	if c.cancel != nil {
		c.cancel()
	}
	c.cancelTasks()
	for _, state := range c.states {
		if state.timer != nil {
			state.timer.Stop()
		}
	}
	c.mu.Unlock()
	c.wait.Wait()
	if c.ownedWorkloads {
		c.workloads.Close()
	}
	if c.metadataPublisher != nil {
		_ = c.metadataPublisher.RemoveDiscoveredMetadata(c.metadataSource)
	}
}

func (c *Controller) Reconcile(ctx context.Context) error {
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	started := c.now().UTC()
	result, err := c.bridge.Discover(ctx, nil)
	if err != nil {
		return err
	}
	if len(result.Endpoints) > 256 {
		return fmt.Errorf("sparkrun bridge returned too many endpoints")
	}
	seen := make(map[string]struct{})
	for _, target := range c.targets {
		for _, discovered := range result.Endpoints {
			if !matchesTarget(target, discovered) || !c.workloads.available(discovered.ClusterID) {
				continue
			}
			if target.Source == lifecycle.EndpointActivatable {
				c.workloads.observe(target.Binding, discovered, c.bridge, c.stopTimeout, false)
			}
			fence := int64(0)
			if target.Source == lifecycle.EndpointActivatable {
				var exists bool
				fence, exists = c.fencingFor(target, discovered)
				if !exists {
					// A configured activation must first obtain its fencing token
					// from AdmissionCoordinator. Reconciliation only refreshes an
					// activation registration that this process already approved.
					continue
				}
			}
			endpoint, metadata, err := c.endpointFromBridge(target, discovered, fence)
			if err != nil {
				continue
			}
			if err := c.approveAndRegister(ctx, endpoint, metadata); err != nil {
				return err
			}
			seen[endpoint.ID] = struct{}{}
			c.refreshBindingState(target, endpoint)
		}
	}
	return c.removeMissing(ctx, seen, started)
}

func (c *Controller) EnsureReady(
	ctx context.Context,
	binding lifecycle.Binding,
	_ lifecycle.RequestFeatures,
) (lifecycle.Lease, error) {
	unlock, err := c.workloads.lock(ctx, binding)
	if err != nil {
		return lifecycle.Lease{}, err
	}
	defer unlock()
	key := lifecycle.ActivationKey(binding)
	now := c.now().UTC()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return lifecycle.Lease{}, fmt.Errorf("sparkrun controller is closed")
	}
	state := c.states[key]
	if state != nil && state.endpoint != nil && state.state == endpointregistry.StateReady &&
		state.endpoint.ExpiresAt.After(now) && c.workloads.available(state.endpoint.ClusterID) {
		lease := c.newLeaseLocked(key, state)
		c.mu.Unlock()
		return lease, nil
	}
	if state == nil {
		state = &bindingState{}
		c.states[key] = state
	}
	c.mu.Unlock()

	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	now = c.now().UTC()
	c.mu.Lock()
	state = c.states[key]
	if state != nil && state.endpoint != nil && state.state == endpointregistry.StateReady &&
		state.endpoint.ExpiresAt.After(now) && c.workloads.available(state.endpoint.ClusterID) {
		lease := c.newLeaseLocked(key, state)
		c.mu.Unlock()
		return lease, nil
	}
	if state == nil {
		state = &bindingState{}
		c.states[key] = state
	}
	state.binding = cloneBinding(binding)
	state.state = endpointregistry.StateActivating
	state.updatedAt = now
	state.reason = ""
	c.mu.Unlock()

	timeout := binding.ActivationTimeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	result, err := c.bridge.EnsureReady(ctx, bridgeBinding(binding), timeout)
	if err != nil {
		c.setFailure(key, binding, err)
		return lifecycle.Lease{}, err
	}
	if result.State != string(endpointregistry.StateReady) || result.Endpoint == nil {
		err := fmt.Errorf("sparkrun bridge did not return a ready endpoint")
		c.setFailure(key, binding, err)
		return lifecycle.Lease{}, err
	}
	target, exists := c.targets[binding.Deployment]
	if !exists || target.Source != lifecycle.EndpointActivatable {
		return lifecycle.Lease{}, fmt.Errorf("sparkrun activation binding is not configured")
	}
	fence := binding.FencingToken
	if fence <= 0 {
		var exists bool
		fence, exists = c.fencingFor(target, *result.Endpoint)
		if !exists {
			err := fmt.Errorf("sparkrun activation is missing a fencing token")
			c.setFailure(key, binding, err)
			return lifecycle.Lease{}, err
		}
	}
	endpoint, metadata, err := c.endpointFromBridge(target, *result.Endpoint, fence)
	if err != nil {
		c.setFailure(key, binding, err)
		return lifecycle.Lease{}, err
	}
	if err := c.approveAndRegister(ctx, endpoint, metadata); err != nil {
		c.setFailure(key, binding, err)
		return lifecycle.Lease{}, err
	}
	if err := c.publishMetadata(); err != nil {
		c.setFailure(key, binding, err)
		return lifecycle.Lease{}, err
	}
	c.workloads.observe(binding, *result.Endpoint, c.bridge, c.stopTimeout, false)
	c.mu.Lock()
	state = c.states[key]
	state.binding = cloneBinding(binding)
	state.state = endpointregistry.StateReady
	state.endpoint = endpointPointer(endpoint)
	state.updatedAt = c.now().UTC()
	state.reason = ""
	lease := c.newLeaseLocked(key, state)
	c.mu.Unlock()
	return lease, nil
}

func (c *Controller) Release(
	_ context.Context,
	lease lifecycle.Lease,
	_ lifecycle.RequestOutcome,
) error {
	c.mu.Lock()
	key, exists := c.leases[lease.ID]
	if !exists {
		c.mu.Unlock()
		return nil
	}
	delete(c.leases, lease.ID)
	c.workloads.release(lease.Endpoint.ClusterID)
	state := c.states[key]
	if state == nil || state.endpoint == nil || state.endpoint.ID != lease.Endpoint.ID {
		c.mu.Unlock()
		return nil
	}
	if state.active > 0 {
		state.active--
	}
	state.updatedAt = c.now().UTC()

	c.mu.Unlock()
	return nil
}

func (c *Controller) Status(
	ctx context.Context,
	binding lifecycle.Binding,
) (lifecycle.Status, error) {
	if err := ctx.Err(); err != nil {
		return lifecycle.Status{}, err
	}
	key := lifecycle.ActivationKey(binding)
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.states[key]
	if state == nil {
		return lifecycle.Status{State: endpointregistry.StateOffline, UpdatedAt: c.now().UTC()}, nil
	}
	status := lifecycle.Status{State: state.state, UpdatedAt: state.updatedAt, Reason: state.reason, Phase: state.phase, JobID: state.jobID}
	if state.endpoint != nil && state.endpoint.ExpiresAt.After(c.now()) && c.workloads.available(state.endpoint.ClusterID) {
		status.Endpoint = endpointPointer(*state.endpoint)
		status.JobID = state.endpoint.JobID
		if value, exists := state.endpoint.Metadata["owned"]; exists {
			owned := value == "true"
			status.Owned = &owned
		}
		status.Phase = "ready"
	} else if status.State == endpointregistry.StateReady {
		status.State = endpointregistry.StateOffline
		status.Endpoint = nil
	}
	if state.endpoint != nil {
		if phase := c.workloads.phase(state.endpoint.ClusterID); phase != "" {
			status.State = endpointregistry.State(phase)
			status.Phase = phase
			status.Endpoint = nil
		}
	}
	return status, nil
}

func (c *Controller) Stop(
	ctx context.Context,
	binding lifecycle.Binding,
	_ lifecycle.StopReason,
) error {
	key := lifecycle.ActivationKey(binding)
	c.mu.Lock()
	clusterID := ""
	if state := c.states[key]; state != nil && state.endpoint != nil {
		clusterID = state.endpoint.ClusterID
	}
	c.mu.Unlock()
	return c.stop(ctx, key, binding, clusterID)
}

func (c *Controller) AuthorizeEndpoint(
	ctx context.Context,
	endpoint endpointregistry.Endpoint,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	approved, exists := c.approved[endpoint.ID]
	c.mu.Unlock()
	if !exists || !sameEndpoint(approved, endpoint) || !c.workloads.available(endpoint.ClusterID) {
		return fmt.Errorf("endpoint was not approved by the sparkrun controller")
	}
	return nil
}

func (c *Controller) newLeaseLocked(key string, state *bindingState) lifecycle.Lease {
	if state.timer != nil {
		state.timer.Stop()
		state.timer = nil
	}
	state.generation++
	state.active++
	c.workloads.acquire(state.endpoint.ClusterID)
	leaseID := fmt.Sprintf("sparkrun-lease-%d", c.nextLease.Add(1))
	c.leases[leaseID] = key
	return lifecycle.Lease{ID: leaseID, Endpoint: *state.endpoint}
}

func (c *Controller) setFailure(key string, binding lifecycle.Binding, cause error) {
	c.mu.Lock()
	state := c.states[key]
	if state == nil {
		state = &bindingState{}
		c.states[key] = state
	}
	state.binding = cloneBinding(binding)
	state.state = endpointregistry.StateFailed
	state.updatedAt = c.now().UTC()
	state.reason = failureReason(cause)
	c.mu.Unlock()
}

func failureReason(err error) string {
	var bridgeErr *BridgeError
	if errors.As(err, &bridgeErr) {
		return lifecycle.SanitizeReason(bridgeErr.Code)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "activation_timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "activation_cancelled"
	}
	return "controller_failure"
}

func (c *Controller) stop(
	ctx context.Context,
	key string,
	binding lifecycle.Binding,
	clusterID string,
) error {
	unlock, err := c.workloads.lock(ctx, binding)
	if err != nil {
		return err
	}
	defer unlock()
	if clusterID == "" {
		return fmt.Errorf("no tracked sparkrun workload to stop")
	}
	if err := c.workloads.canStop(clusterID); err != nil {
		return err
	}
	c.mu.Lock()
	if state := c.states[key]; state != nil {
		state.state = endpointregistry.StateDeactivating
		state.updatedAt = c.now().UTC()
	}
	c.mu.Unlock()
	_, err = c.bridge.Stop(ctx, bridgeBinding(binding), clusterID)
	if err != nil {
		c.setFailure(key, binding, err)
		return err
	}
	c.workloads.stopped(clusterID)
	endpointIDs := c.unapproveTarget(binding.Deployment)
	for _, endpointID := range endpointIDs {
		if removeErr := c.registry.Remove(ctx, endpointID); removeErr != nil {
			return removeErr
		}
	}
	if err := c.publishMetadata(); err != nil {
		return err
	}
	c.mu.Lock()
	state := c.states[key]
	if state == nil {
		state = &bindingState{binding: cloneBinding(binding)}
		c.states[key] = state
	}
	state.state = endpointregistry.StateOffline
	state.endpoint = nil
	state.updatedAt = c.now().UTC()
	state.reason = ""
	c.mu.Unlock()
	return nil
}

func (c *Controller) fencingFor(
	target lifecycle.Target,
	discovered Endpoint,
) (int64, bool) {
	id := endpointID(target.Deployment, discovered)
	c.mu.Lock()
	approved, exists := c.approved[id]
	c.mu.Unlock()
	if exists && approved.FencingToken > 0 {
		return approved.FencingToken, true
	}
	return 0, false
}

func (c *Controller) endpointFromBridge(
	target lifecycle.Target,
	discovered Endpoint,
	fencingToken int64,
) (endpointregistry.Endpoint, *modelrouter.DiscoveredModelMetadata, error) {
	if discovered.State != string(endpointregistry.StateReady) || discovered.Protocol != "openai" {
		return endpointregistry.Endpoint{}, nil, fmt.Errorf("sparkrun endpoint state or protocol is invalid")
	}
	if err := validateHost(discovered.Host); err != nil {
		return endpointregistry.Endpoint{}, nil, err
	}
	if discovered.Port < 1 || discovered.Port > 65535 {
		return endpointregistry.Endpoint{}, nil, fmt.Errorf("sparkrun endpoint port is invalid")
	}
	if discovered.ClusterID == "" || len(discovered.ClusterID) > 1024 ||
		discovered.JobID == "" || len(discovered.JobID) > 1024 {
		return endpointregistry.Endpoint{}, nil, fmt.Errorf("sparkrun endpoint identity is invalid")
	}
	served := append([]string(nil), discovered.ServedModels...)
	slices.Sort(served)
	served = slices.Compact(served)
	if len(served) == 0 || len(served) > 256 ||
		!slices.Contains(served, target.UpstreamModel) {
		return endpointregistry.Endpoint{}, nil, fmt.Errorf("sparkrun endpoint does not serve the configured model")
	}
	for _, model := range served {
		if model == "" || len(model) > 1024 || strings.IndexFunc(model, func(r rune) bool { return r < 0x20 }) >= 0 {
			return endpointregistry.Endpoint{}, nil, fmt.Errorf("sparkrun endpoint model identity is invalid")
		}
	}
	// Model metadata is optional enrichment. A malformed extension must not
	// make an otherwise valid inference endpoint unavailable.
	metadata := bridgeModelMetadata(discovered, served, target.UpstreamModel)
	now := c.now().UTC()
	endpoint := endpointregistry.Endpoint{
		ID: endpointID(target.Deployment, discovered), Target: target.Deployment,
		BaseURL:  "http://" + net.JoinHostPort(discovered.Host, strconv.Itoa(discovered.Port)) + "/v1",
		Protocol: "openai", ServedModels: served, Controller: ControllerName,
		ClusterID: discovered.ClusterID, JobID: discovered.JobID,
		RecipeRevision: discovered.RecipeRevision, FencingToken: fencingToken,
		State: endpointregistry.StateReady, RegisteredAt: now, HeartbeatAt: now,
		ExpiresAt: now.Add(c.endpointTTL),
	}
	// Advisory display metadata never changes endpoint identity or fencing.
	if name := discovered.ClusterName; len(name) <= 1024 && strings.TrimSpace(name) != "" && strings.IndexFunc(name, unicode.IsControl) < 0 {
		endpoint.Metadata = map[string]string{"cluster_name": name}
	}
	if endpoint.Metadata == nil {
		endpoint.Metadata = map[string]string{}
	}
	endpoint.Metadata["owned"] = strconv.FormatBool(discovered.Owned)
	if target.Source == lifecycle.EndpointActivatable {
		endpoint.BindingRevision = target.Binding.Revision
		if !matchesTarget(target, discovered) || fencingToken <= 0 {
			return endpointregistry.Endpoint{}, nil, fmt.Errorf("sparkrun endpoint recipe revision or fencing token is invalid")
		}
	}
	return endpoint, metadata, nil
}

func bridgeModelMetadata(
	discovered Endpoint,
	served []string,
	upstreamModel string,
) *modelrouter.DiscoveredModelMetadata {
	if len(discovered.ModelMetadata) > 256 {
		return nil
	}
	converted := make(map[string]modelrouter.DiscoveredModelMetadata, len(discovered.ModelMetadata))
	for model, metadata := range discovered.ModelMetadata {
		if !slices.Contains(served, model) {
			return nil
		}
		converted[model] = modelrouter.DiscoveredModelMetadata{
			SizeB: cloneFloat(metadata.SizeB), InputPrice: cloneFloat(metadata.InputPrice),
			OutputPrice: cloneFloat(metadata.OutputPrice), Context: cloneInt(metadata.Context),
			Tags: append([]string(nil), metadata.Tags...),
		}
	}
	if err := modelrouter.ValidateDiscoveredMetadataSnapshot(modelrouter.DiscoveredMetadataSnapshot{
		Source: ControllerName, Models: converted,
	}); err != nil {
		return nil
	}
	metadata, exists := converted[upstreamModel]
	if !exists {
		return nil
	}
	return &metadata
}

func (c *Controller) publishMetadata() error {
	if c.metadataPublisher == nil {
		return nil
	}
	c.mu.Lock()
	ids := make([]string, 0, len(c.metadata))
	for id := range c.metadata {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	models := make(map[string]modelrouter.DiscoveredModelMetadata)
	for _, id := range ids {
		endpoint, exists := c.approved[id]
		if !exists {
			continue
		}
		target, exists := c.targets[endpoint.Target]
		if !exists {
			continue
		}
		logicalModels := append([]string(nil), target.LogicalModels...)
		if len(logicalModels) == 0 && target.Binding.VirtualModel != "" {
			logicalModels = []string{target.Binding.VirtualModel}
		}
		for _, logicalModel := range logicalModels {
			if logicalModel == "" {
				continue
			}
			models[logicalModel] = reduceRouterMetadata(models[logicalModel], c.metadata[id])
		}
	}
	c.mu.Unlock()
	return c.metadataPublisher.ReplaceDiscoveredMetadata(modelrouter.DiscoveredMetadataSnapshot{
		Source: c.metadataSource, ObservedAt: c.now().UTC(), Models: models,
	})
}

func reduceRouterMetadata(
	current modelrouter.DiscoveredModelMetadata,
	next modelrouter.DiscoveredModelMetadata,
) modelrouter.DiscoveredModelMetadata {
	if next.Context != nil && (current.Context == nil || *next.Context < *current.Context) {
		current.Context = cloneInt(next.Context)
	}
	if next.SizeB != nil && (current.SizeB == nil || *next.SizeB > *current.SizeB) {
		current.SizeB = cloneFloat(next.SizeB)
	}
	if next.InputPrice != nil && (current.InputPrice == nil || *next.InputPrice > *current.InputPrice) {
		current.InputPrice = cloneFloat(next.InputPrice)
	}
	if next.OutputPrice != nil && (current.OutputPrice == nil || *next.OutputPrice > *current.OutputPrice) {
		current.OutputPrice = cloneFloat(next.OutputPrice)
	}
	tags := append(append([]string(nil), current.Tags...), next.Tags...)
	slices.Sort(tags)
	current.Tags = slices.Compact(tags)
	return current
}

func cloneRouterMetadata(metadata modelrouter.DiscoveredModelMetadata) modelrouter.DiscoveredModelMetadata {
	metadata.SizeB = cloneFloat(metadata.SizeB)
	metadata.InputPrice = cloneFloat(metadata.InputPrice)
	metadata.OutputPrice = cloneFloat(metadata.OutputPrice)
	metadata.Context = cloneInt(metadata.Context)
	metadata.Tags = append([]string(nil), metadata.Tags...)
	return metadata
}

func cloneFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func (c *Controller) approveAndRegister(
	ctx context.Context,
	endpoint endpointregistry.Endpoint,
	metadata *modelrouter.DiscoveredModelMetadata,
) error {
	c.mu.Lock()
	prior, existed := c.approved[endpoint.ID]
	priorMetadata, hadMetadata := c.metadata[endpoint.ID]
	c.approved[endpoint.ID] = endpoint
	if metadata == nil {
		delete(c.metadata, endpoint.ID)
	} else {
		c.metadata[endpoint.ID] = cloneRouterMetadata(*metadata)
	}
	c.tracked[endpoint.ID] = endpoint.Target
	c.mu.Unlock()
	if err := c.registry.Register(ctx, endpoint); err != nil {
		c.mu.Lock()
		if current, ok := c.approved[endpoint.ID]; ok && sameEndpoint(current, endpoint) {
			if existed {
				c.approved[endpoint.ID] = prior
			} else {
				delete(c.approved, endpoint.ID)
				delete(c.tracked, endpoint.ID)
			}
			if hadMetadata {
				c.metadata[endpoint.ID] = priorMetadata
			} else {
				delete(c.metadata, endpoint.ID)
			}
		}
		c.mu.Unlock()
		return err
	}
	return nil
}

func (c *Controller) refreshBindingState(target lifecycle.Target, endpoint endpointregistry.Endpoint) {
	if target.Source != lifecycle.EndpointActivatable {
		return
	}
	key := lifecycle.ActivationKey(target.Binding)
	c.mu.Lock()
	state := c.states[key]
	if state != nil && (state.endpoint == nil || state.endpoint.ID == endpoint.ID) {
		state.endpoint = endpointPointer(endpoint)
		state.state = endpointregistry.StateReady
		state.updatedAt = c.now().UTC()
		state.reason = ""
	}
	c.mu.Unlock()
}

func (c *Controller) removeMissing(ctx context.Context, seen map[string]struct{}, started time.Time) error {
	c.mu.Lock()
	missing := make([]string, 0)
	for endpointID := range c.tracked {
		if _, exists := seen[endpointID]; !exists && !c.approved[endpointID].HeartbeatAt.After(started) {
			missing = append(missing, endpointID)
			delete(c.tracked, endpointID)
			delete(c.approved, endpointID)
			delete(c.metadata, endpointID)
			for _, state := range c.states {
				if state.endpoint != nil && state.endpoint.ID == endpointID {
					state.endpoint = nil
					state.state = endpointregistry.StateOffline
					state.updatedAt = c.now().UTC()
					state.reason = "endpoint_not_discovered"
				}
			}
		}
	}
	c.mu.Unlock()
	for _, endpointID := range missing {
		if err := c.registry.Remove(ctx, endpointID); err != nil {
			return err
		}
	}
	return c.publishMetadata()
}

func (c *Controller) unapproveTarget(target string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]string, 0)
	for endpointID, endpointTarget := range c.tracked {
		if endpointTarget == target {
			result = append(result, endpointID)
			delete(c.tracked, endpointID)
			delete(c.approved, endpointID)
			delete(c.metadata, endpointID)
		}
	}
	return result
}

func matchesTarget(target lifecycle.Target, endpoint Endpoint) bool {
	if endpoint.State != string(endpointregistry.StateReady) || endpoint.Protocol != target.Protocol ||
		!slices.Contains(endpoint.ServedModels, target.UpstreamModel) {
		return false
	}
	if target.Source == lifecycle.EndpointActivatable {
		return endpoint.RecipeRevision == target.Binding.RecipeRevision && (len(target.Binding.ClusterCandidates) == 0 || slices.Contains(target.Binding.ClusterCandidates, endpoint.ClusterName))
	}
	return target.Source == lifecycle.EndpointDiscovered
}

func endpointID(target string, endpoint Endpoint) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		target, endpoint.ClusterID, endpoint.JobID, endpoint.Host, strconv.Itoa(endpoint.Port),
	}, "\x00")))
	return "sparkrun-" + hex.EncodeToString(digest[:16])
}

func validateHost(host string) error {
	if host == "" || len(host) > 253 || strings.IndexFunc(host, func(r rune) bool { return r < 0x20 }) >= 0 {
		return fmt.Errorf("sparkrun endpoint host is invalid")
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	host = strings.TrimSuffix(host, ".")
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("sparkrun endpoint host is invalid")
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
				(char < '0' || char > '9') && char != '-' {
				return fmt.Errorf("sparkrun endpoint host is invalid")
			}
		}
	}
	return nil
}

func bridgeBinding(binding lifecycle.Binding) Binding {
	return Binding{
		Recipe: binding.Recipe, RecipeRevision: binding.RecipeRevision,
		ClusterCandidates: append([]string(nil), binding.ClusterCandidates...),
		Overrides:         cloneOverrides(binding.Overrides),
	}
}

func cloneBinding(binding lifecycle.Binding) lifecycle.Binding {
	binding.ClusterCandidates = append([]string(nil), binding.ClusterCandidates...)
	binding.Overrides = cloneOverrides(binding.Overrides)
	return binding
}

func cloneOverrides(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func endpointPointer(endpoint endpointregistry.Endpoint) *endpointregistry.Endpoint {
	copy := endpoint
	copy.ServedModels = append([]string(nil), endpoint.ServedModels...)
	return &copy
}

func sameEndpoint(left, right endpointregistry.Endpoint) bool {
	return left.ID == right.ID && left.Target == right.Target && left.BaseURL == right.BaseURL &&
		left.Protocol == right.Protocol && slices.Equal(left.ServedModels, right.ServedModels) &&
		left.Controller == right.Controller && left.ClusterID == right.ClusterID && left.JobID == right.JobID &&
		left.BindingRevision == right.BindingRevision && left.RecipeRevision == right.RecipeRevision &&
		left.FencingToken == right.FencingToken && left.State == right.State &&
		left.ActiveRequests == right.ActiveRequests && left.MaxConcurrency == right.MaxConcurrency &&
		left.RegisteredAt.Equal(right.RegisteredAt) && left.HeartbeatAt.Equal(right.HeartbeatAt) &&
		left.ExpiresAt.Equal(right.ExpiresAt)
}

var (
	_ lifecycle.Controller         = (*Controller)(nil)
	_ lifecycle.EndpointAuthorizer = (*Controller)(nil)
)
