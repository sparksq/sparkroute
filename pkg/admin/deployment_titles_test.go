package admin

import (
	"context"
	"encoding/json"
	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/endpointregistry"
	"net/http"
	"net/http/httptest"
	"testing"
)

type titleEndpoints map[string][]endpointregistry.Endpoint

func (s titleEndpoints) Ready(_ context.Context, target string) ([]endpointregistry.Endpoint, error) {
	return s[target], nil
}

func TestStatusPrefersLiveNamedClustersWithoutChangingStoredConfig(t *testing.T) {
	deployment := config.Deployment{Name: "sparkrun:5813420d8cac", Title: "sparkrun:unassigned:deepseek", Model: "deepseek", EndpointSource: config.EndpointSource{Type: config.EndpointSourceActivatable, Controller: "sparkrun"}}
	document := config.Document{Deployments: []config.Deployment{deployment}}
	endpoints := titleEndpoints{deployment.Name: {
		{Controller: "sparkrun", ClusterID: "opaque-b", Metadata: map[string]string{"cluster_name": "spark-b"}},
		{Controller: "sparkrun", ClusterID: "opaque-a", Metadata: map[string]string{"cluster_name": "spark-a"}},
		{Controller: "sparkrun", ClusterID: "duplicate", Metadata: map[string]string{"cluster_name": "spark-a"}},
		{Controller: "other", Metadata: map[string]string{"cluster_name": "wrong-controller"}},
	}}
	handler := NewHandler(document, "unchanged", Options{Endpoints: endpoints, Targets: staticTargetStatuses{{Deployment: deployment.Name}}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	var status Status
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Targets[0].Title != "sparkrun:spark-a,spark-b:deepseek" || status.Targets[0].Deployment != deployment.Name || status.ConfigRevision != "unchanged" {
		t.Fatalf("status = %#v", status)
	}
	if document.Deployments[0].Title != deployment.Title {
		t.Fatal("runtime presentation changed config")
	}
	delete(endpoints, deployment.Name)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Targets[0].Title != deployment.Title {
		t.Fatal("missing runtime metadata did not fall back")
	}
}
