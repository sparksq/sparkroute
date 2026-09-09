package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
)

func TestStatusDeploymentInventoryMetadata(t *testing.T) {
	document := config.Document{
		Deployments: []config.Deployment{
			{Name: "cold", Provider: "sparkrun", Model: "deepseek/flash", EndpointSource: config.EndpointSource{Type: config.EndpointSourceActivatable, Controller: "sparkrun", Recipe: "private-recipe-path", Overrides: map[string]string{"private-option": "private-value"}}},
			{Name: "manual", Provider: "sparkrun", Model: "deepseek/flash", EndpointSource: config.EndpointSource{Type: config.EndpointSourceActivatable, Controller: "sparkrun", ColdStart: config.ColdStartReject}},
			{Name: "hosted", Provider: "hosted", Model: "hosted-model"},
		},
		VirtualModels: []config.VirtualModel{
			{Name: "flash", Aliases: []string{"ds4f"}, Pools: []config.RoutingPool{{Targets: []config.WeightedTarget{{Deployment: "cold"}, {Deployment: "manual"}, {Deployment: "cold"}}}}},
			{Name: "coding", Pools: []config.RoutingPool{{Targets: []config.WeightedTarget{{Deployment: "cold"}}}}},
		},
	}
	handler := NewHandler(document, "revision", Options{Targets: staticTargetStatuses{{Deployment: "cold"}, {Deployment: "manual"}, {Deployment: "hosted"}}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	var result Status
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil {
		t.Fatalf("status: %d %s", response.Code, response.Body.String())
	}
	byID := make(map[string]TargetStatus)
	for _, target := range result.Targets {
		byID[target.Deployment] = target
	}
	cold := byID["cold"]
	if cold.Provider != "sparkrun" || cold.Model != "deepseek/flash" || cold.EndpointSource != config.EndpointSourceActivatable || cold.Controller != "sparkrun" || cold.ColdStart != config.ColdStartWait {
		t.Fatalf("cold: %#v", cold)
	}
	if !reflect.DeepEqual(cold.ModelNames, []string{"coding", "ds4f", "flash"}) {
		t.Fatalf("model names: %v", cold.ModelNames)
	}
	if byID["manual"].ColdStart != config.ColdStartReject || byID["hosted"].EndpointSource != config.EndpointSourceStatic || byID["hosted"].ColdStart != "" {
		t.Fatalf("activation policies: %#v", byID)
	}
	if strings.Contains(response.Body.String(), "private-") {
		t.Fatal("status exposed recipe or override data")
	}
}
