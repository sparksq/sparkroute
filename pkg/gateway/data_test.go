package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
)

func TestDataHandlerModels(t *testing.T) {
	t.Parallel()

	handler, err := NewDataHandler(testDocument(), DataOptions{
		Models: ModelListOptions{IncludeAliases: true},
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body)
	}
	var body modelList
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if len(body.Data) != 2 {
		t.Fatalf("model count = %d, want 2", len(body.Data))
	}
	if body.Data[0].ID != "default" || body.Data[1].ID != "local-default" {
		t.Fatalf("model IDs = %q, %q", body.Data[0].ID, body.Data[1].ID)
	}
}

func TestDataHandlerModelsOmitsHiddenAndInternalModels(t *testing.T) {
	t.Parallel()

	document := testDocument()
	document.VirtualModels = append(
		document.VirtualModels,
		config.VirtualModel{
			Name:       "hidden-model",
			Aliases:    []string{"hidden-alias"},
			Visibility: config.ModelVisibilityHidden,
			Pools:      document.VirtualModels[0].Pools,
		},
		config.VirtualModel{
			Name:       "internal-model",
			Aliases:    []string{"internal-alias"},
			Visibility: config.ModelVisibilityInternal,
			Pools:      document.VirtualModels[0].Pools,
		},
	)
	handler, err := NewDataHandler(document, DataOptions{
		Models: ModelListOptions{IncludeAliases: true},
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/v1/models", nil),
	)
	var body modelList
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if len(body.Data) != 2 ||
		body.Data[0].ID != "default" ||
		body.Data[1].ID != "local-default" {
		t.Fatalf("listed models = %#v", body.Data)
	}
	hidden := httptest.NewRecorder()
	handler.ServeHTTP(
		hidden,
		httptest.NewRequest(http.MethodGet, "/v1/models/hidden-alias", nil),
	)
	var hiddenModel modelObject
	if hidden.Code != http.StatusOK {
		t.Fatalf("hidden retrieval status = %d; body=%s", hidden.Code, hidden.Body)
	}
	if err := json.Unmarshal(hidden.Body.Bytes(), &hiddenModel); err != nil ||
		hiddenModel.ID != "hidden-alias" {
		t.Fatalf("hidden retrieval = %#v, %v", hiddenModel, err)
	}
	for _, name := range []string{"internal-model", "internal-alias", "missing"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(
			response,
			httptest.NewRequest(http.MethodGet, "/v1/models/"+name, nil),
		)
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s retrieval status = %d", name, response.Code)
		}
	}
}

func TestDataPlaneExposesItsLiveTargetManager(t *testing.T) {
	t.Parallel()

	plane, err := NewDataPlane(testDocument(), DataOptions{})
	if err != nil {
		t.Fatalf("NewDataPlane() error = %v", err)
	}
	if plane.Handler == nil || plane.Targets == nil {
		t.Fatalf("NewDataPlane() = %#v", plane)
	}
	statuses := plane.Targets.Statuses()
	if len(statuses) != 1 || statuses[0].Deployment != "local-default" {
		t.Fatalf("target statuses = %#v", statuses)
	}
}

func TestDataHandlerRejectsMethod(t *testing.T) {
	t.Parallel()

	handler, err := NewDataHandler(testDocument(), DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/models", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
	if response.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("Allow = %q, want GET", response.Header().Get("Allow"))
	}
}

func TestOptionalOTLPTraceHandlerIsMountedOnExactPath(t *testing.T) {
	t.Parallel()

	traceHandler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/traces" {
			t.Fatalf("trace handler path = %q", request.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	handler, err := NewDataHandler(testDocument(), DataOptions{
		OTLPTraceHandler: traceHandler,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader("{}")),
	)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.Code)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodPost, "/v1/traces/other", strings.NewReader("{}")),
	)
	if response.Code != http.StatusNotFound {
		t.Fatalf("subpath status = %d, want 404", response.Code)
	}
}

func testDocument() config.Document {
	return config.Document{
		Providers: []config.Provider{{
			Name:    "local",
			Type:    "openai_compatible",
			BaseURL: "http://127.0.0.1:8000/v1",
		}},
		Deployments: []config.Deployment{{
			Name:     "local-default",
			Provider: "local",
			Model:    "upstream-model",
		}},
		VirtualModels: []config.VirtualModel{{
			Name:    "local-default",
			Aliases: []string{"default"},
			Pools: []config.RoutingPool{{
				Targets: []config.WeightedTarget{{
					Deployment: "local-default",
					Weight:     100,
				}},
			}},
		}},
	}
}
