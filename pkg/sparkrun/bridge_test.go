// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package sparkrun

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "gateway-bridge" && os.Getenv("SPARKROUTE_BRIDGE_TEST_MODE") != "" {
		var request bridgeRequest
		if json.NewDecoder(os.Stdin).Decode(&request) != nil {
			os.Exit(2)
		}
		file, err := os.OpenFile(os.Getenv("SPARKROUTE_BRIDGE_TEST_CALLS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(3)
		}
		_ = json.NewEncoder(file).Encode(request)
		_ = file.Close()
		response := bridgeResponse{SchemaVersion: request.SchemaVersion, RequestID: request.RequestID, OK: true}
		mode := os.Getenv("SPARKROUTE_BRIDGE_TEST_MODE")
		if mode == "failure" {
			response.OK = false
			response.Error = &BridgeError{Code: "activation_failed", Retryable: true}
		} else if mode == "uncorrelated" {
			response.OK = false
			response.RequestID = "wrong"
			response.Error = &BridgeError{Code: "unsupported_version"}
		} else if request.Operation == "capabilities" {
			response.Result, _ = json.Marshal(Capabilities{ProtocolVersion: request.SchemaVersion, Operations: []string{"discover", "ensure_ready", "stop"}})
		} else {
			endpoint := Endpoint{ClusterID: "opaque-job", JobID: "opaque-job"}
			if request.SchemaVersion >= 2 {
				endpoint.ClusterName = "spark-a"
			}
			response.Result, _ = json.Marshal(DiscoverResult{Endpoints: []Endpoint{endpoint}})
		}
		_ = json.NewEncoder(os.Stdout).Encode(response)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestClientUsesCurrentSchemaAndExposesNamedCluster(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("SPARKROUTE_BRIDGE_TEST_MODE", "new")
	t.Setenv("SPARKROUTE_BRIDGE_TEST_CALLS", calls)
	executable, _ := os.Executable()
	client, _ := NewClient(executable)
	capabilities, err := client.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	discovered, err := client.Discover(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := discovered.Endpoints[0]
	if endpoint.ClusterID != "opaque-job" || endpoint.JobID != "opaque-job" || endpoint.ClusterName != "spark-a" {
		t.Fatalf("endpoint = %#v", endpoint)
	}
	if capabilities.ProtocolVersion != ProtocolVersion {
		t.Fatal("wrong schema")
	}
	raw, _ := os.ReadFile(calls)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("unexpected retry: %s", raw)
	}
	for _, line := range lines {
		var request bridgeRequest
		_ = json.Unmarshal([]byte(line), &request)
		if request.SchemaVersion != ProtocolVersion {
			t.Fatalf("calls: %s", raw)
		}
	}
}

func TestClientNeverRetriesActivationFailuresOrUncorrelatedRefusals(t *testing.T) {
	for _, mode := range []string{"failure", "uncorrelated"} {
		t.Run(mode, func(t *testing.T) {
			calls := filepath.Join(t.TempDir(), "calls")
			t.Setenv("SPARKROUTE_BRIDGE_TEST_MODE", mode)
			t.Setenv("SPARKROUTE_BRIDGE_TEST_CALLS", calls)
			executable, _ := os.Executable()
			client, _ := NewClient(executable)
			_, err := client.EnsureReady(context.Background(), Binding{Recipe: "recipe"}, 5*time.Second)
			if err == nil {
				t.Fatal("expected refusal")
			}
			if mode == "failure" {
				var refusal *BridgeError
				if !errors.As(err, &refusal) || refusal.Code != "activation_failed" {
					t.Fatal(err)
				}
			}
			raw, _ := os.ReadFile(calls)
			if strings.Count(string(raw), "\n") != 1 {
				t.Fatalf("operation was retried: %s", raw)
			}
		})
	}
}
