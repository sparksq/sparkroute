// Package sparkrun integrates the standalone gateway with Sparkrun's hidden
// one-shot JSON bridge command.
package sparkrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

const (
	ProtocolVersion        = 1
	MaxBridgeResponseBytes = 1 << 20
	MaxBridgeStderrBytes   = 64 << 10
)

type Binding struct {
	Recipe            string            `json:"recipe"`
	RecipeRevision    string            `json:"recipe_revision,omitempty"`
	ClusterCandidates []string          `json:"cluster_candidates,omitempty"`
	Overrides         map[string]string `json:"overrides,omitempty"`
}

type Endpoint struct {
	State          string                   `json:"state"`
	ClusterID      string                   `json:"cluster_id"`
	JobID          string                   `json:"job_id"`
	Host           string                   `json:"host"`
	Port           int                      `json:"port"`
	Protocol       string                   `json:"protocol"`
	ServedModels   []string                 `json:"served_models"`
	Recipe         string                   `json:"recipe"`
	RecipeRevision string                   `json:"recipe_revision"`
	Runtime        string                   `json:"runtime"`
	ModelMetadata  map[string]ModelMetadata `json:"model_metadata,omitempty"`
}

// ModelMetadata is the optional public model-card subset returned by newer
// Sparkrun bridges. Older bridges may omit it without changing protocol
// version; all values remain bounded and validated by Controller.
type ModelMetadata struct {
	SizeB       *float64 `json:"size_b,omitempty"`
	InputPrice  *float64 `json:"input_price,omitempty"`
	OutputPrice *float64 `json:"output_price,omitempty"`
	Context     *int     `json:"context,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

type Capabilities struct {
	ProtocolVersion int      `json:"protocol_version"`
	Operations      []string `json:"operations"`
	SparkrunVersion string   `json:"sparkrun_version"`
}

type EnsureResult struct {
	State    string    `json:"state"`
	Endpoint *Endpoint `json:"endpoint"`
	Adopted  bool      `json:"adopted"`
}

type DiscoverResult struct {
	Endpoints []Endpoint `json:"endpoints"`
}

type StopResult struct {
	State      string   `json:"state"`
	ClusterIDs []string `json:"cluster_ids"`
}

type Bridge interface {
	Capabilities(context.Context) (Capabilities, error)
	EnsureReady(context.Context, Binding, time.Duration) (EnsureResult, error)
	Discover(context.Context, *Binding) (DiscoverResult, error)
	Stop(context.Context, Binding, string) (StopResult, error)
}

type BridgeError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func (e *BridgeError) Error() string {
	if e == nil || e.Code == "" {
		return "sparkrun bridge operation failed"
	}
	return "sparkrun bridge operation failed: " + e.Code
}

type Client struct {
	command string
	nextID  atomic.Uint64
}

func NewClient(command string) (*Client, error) {
	if strings.TrimSpace(command) == "" || strings.IndexFunc(command, func(r rune) bool {
		return r < 0x20
	}) >= 0 {
		return nil, fmt.Errorf("sparkrun command is invalid")
	}
	return &Client{command: command}, nil
}

func (c *Client) Capabilities(ctx context.Context) (Capabilities, error) {
	var result Capabilities
	err := c.invoke(ctx, bridgeRequest{Operation: "capabilities"}, &result)
	return result, err
}

func (c *Client) EnsureReady(
	ctx context.Context,
	binding Binding,
	timeout time.Duration,
) (EnsureResult, error) {
	var result EnsureResult
	err := c.invoke(ctx, bridgeRequest{
		Operation: "ensure_ready", Binding: &binding,
		TimeoutSeconds: timeout.Seconds(),
	}, &result)
	return result, err
}

func (c *Client) Discover(
	ctx context.Context,
	binding *Binding,
) (DiscoverResult, error) {
	var result DiscoverResult
	err := c.invoke(ctx, bridgeRequest{Operation: "discover", Binding: binding}, &result)
	return result, err
}

func (c *Client) Stop(
	ctx context.Context,
	binding Binding,
	clusterID string,
) (StopResult, error) {
	var result StopResult
	err := c.invoke(ctx, bridgeRequest{
		Operation: "stop", Binding: &binding, ClusterID: clusterID,
	}, &result)
	return result, err
}

type bridgeRequest struct {
	SchemaVersion  int      `json:"schema_version"`
	RequestID      string   `json:"request_id"`
	Operation      string   `json:"operation"`
	Binding        *Binding `json:"binding,omitempty"`
	ClusterID      string   `json:"cluster_id,omitempty"`
	TimeoutSeconds float64  `json:"timeout_seconds,omitempty"`
}

type bridgeResponse struct {
	SchemaVersion int             `json:"schema_version"`
	RequestID     string          `json:"request_id"`
	OK            bool            `json:"ok"`
	Result        json.RawMessage `json:"result,omitempty"`
	Error         *BridgeError    `json:"error,omitempty"`
}

func (c *Client) invoke(ctx context.Context, request bridgeRequest, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	request.SchemaVersion = ProtocolVersion
	request.RequestID = fmt.Sprintf("gateway-%d", c.nextID.Add(1))
	input, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode sparkrun bridge request: %w", err)
	}
	command := exec.CommandContext(ctx, c.command, "gateway-bridge")
	command.Stdin = bytes.NewReader(input)
	stdout := &boundedBuffer{maximum: MaxBridgeResponseBytes}
	stderr := &boundedBuffer{maximum: MaxBridgeStderrBytes}
	command.Stdout = stdout
	command.Stderr = stderr
	commandErr := command.Run()
	if err := ctx.Err(); err != nil {
		return err
	}

	var response bridgeResponse
	decodeErr := decodeStrict(stdout.Bytes(), &response)
	if decodeErr != nil {
		if commandErr != nil {
			return fmt.Errorf("sparkrun gateway bridge process failed")
		}
		return fmt.Errorf("decode sparkrun bridge response: %w", decodeErr)
	}
	if response.SchemaVersion != ProtocolVersion || response.RequestID != request.RequestID {
		return fmt.Errorf("sparkrun bridge response correlation failed")
	}
	if !response.OK {
		if response.Error == nil || response.Error.Code == "" || len(response.Error.Code) > 128 {
			return fmt.Errorf("sparkrun bridge returned an invalid error")
		}
		return response.Error
	}
	if commandErr != nil {
		return fmt.Errorf("sparkrun gateway bridge process failed")
	}
	if response.Error != nil || len(response.Result) == 0 || string(response.Result) == "null" {
		return fmt.Errorf("sparkrun bridge returned an invalid success response")
	}
	if err := decodeStrict(response.Result, result); err != nil {
		return fmt.Errorf("decode sparkrun bridge result: %w", err)
	}
	return nil
}

func decodeStrict(input []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

type boundedBuffer struct {
	bytes.Buffer
	maximum int
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	remaining := b.maximum - b.Len()
	if remaining <= 0 {
		return 0, fmt.Errorf("subprocess output exceeds limit")
	}
	if len(value) > remaining {
		_, _ = b.Buffer.Write(value[:remaining])
		return remaining, fmt.Errorf("subprocess output exceeds limit")
	}
	return b.Buffer.Write(value)
}

var _ Bridge = (*Client)(nil)
