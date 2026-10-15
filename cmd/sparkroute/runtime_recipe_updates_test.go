// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.
package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/config/managed"
	configsqlite "github.com/sparksq/sparkroute/pkg/config/sqlite"
	"github.com/sparksq/sparkroute/pkg/sparkrun"
	"github.com/sparksq/sparkroute/pkg/telemetry/otlpexport"
)

func TestRecipeAutoUpdateReconcilerAndOptOut(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		name := "enabled"
		if !enabled {
			name = "disabled"
		}
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"recipe-update","choices":[]}`)
			}))
			defer upstream.Close()
			path := newAcceptanceBridgeFixture(t, upstream.URL, acceptanceBridgeState{CatalogRevision: "updated00001"})
			document, _ := acceptanceRoutingDocument(false, 32, 1<<20, 5*time.Second, 0)
			document.ModelRouting = nil
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store, err := configsqlite.Open(ctx, configsqlite.Options{Path: filepath.Join(root, "config.sqlite")})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			}()
			if _, err = store.Initialize(ctx, document, "test", ""); err != nil {
				t.Fatal(err)
			}
			document, revision, err := store.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			workloads := sparkrun.NewWorkloads()
			defer workloads.Close()
			options := runtimeBuildOptions{Context: ctx, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Telemetry: &otlpexport.Runtime{}, ManagedConfig: store, SparkrunEnabled: true, SparkrunAutoUpdateRecipes: enabled, SparkrunCommand: executable, SparkrunEndpointTTL: 10 * time.Second, SparkrunReconcile: time.Second, SparkrunProfileRefresh: 25 * time.Millisecond, SparkrunStopTimeout: time.Second, SparkrunWorkloads: workloads}
			initial, err := buildRuntimeGeneration(document, revision, options)
			if err != nil {
				t.Fatal(err)
			}
			slot := newRuntimeSlot(initial)
			defer slot.Close()
			done := make(chan struct{})
			go func() { defer close(done); reconcileManagedConfiguration(ctx, store, slot, options, options.Logger) }()
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("reconciler did not stop")
				}
			}()
			if enabled {
				waitAcceptance(t, 10*time.Second, func() bool {
					doc, rev, e := store.Load(ctx)
					return e == nil && doc.Deployments[0].EndpointSource.RecipeRevision == "updated00001" && slot.current.Load().revision == rev
				}, "automatic recipe update applied")
				response := serveAcceptanceJSON(t, slot.dataHandler(), "/v1/chat/completions", `{"model":"cold","messages":[]}`)
				if response.Code != 200 {
					t.Fatalf("%d %s", response.Code, response.Body)
				}
				state := mustAcceptanceBridgeState(t, path)
				if state.LastRecipeRevision != "updated00001" || state.LaunchCalls != 1 || state.StopCalls != 0 {
					t.Fatalf("wrong launch after update: %+v", state)
				}
			} else {
				// Allow multiple refresh ticks; recipe contents stay pinned.
				time.Sleep(150 * time.Millisecond)
				set, err := store.GetSet(ctx, managed.OwnerOperator)
				if err != nil {
					t.Fatal(err)
				}
				if set.Document.Deployments[0].EndpointSource.RecipeRevision != "coldrev00001" {
					t.Fatal("opt-out updated recipe")
				}
				if state := mustAcceptanceBridgeState(t, path); state.EnsureCalls != 0 || state.StopCalls != 0 {
					t.Fatal("refresh changed workloads")
				}
			}
		})
	}
}
