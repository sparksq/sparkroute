import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AdminApp } from "./App";

function jsonResponse(value: unknown, status = 200) {
  return new Response(JSON.stringify(value), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

const bootstrap = {
  schema_version: 1,
  edition: "standalone",
  gateway_version: "1.2.3",
  build: { name: "sparkroute", version: "1.2.3", commit: "abcd1234abcd1234", source: "https://github.com/sparksq/sparkroute/tree/abcd1234abcd1234", license: "AGPL-3.0-only" },
  config_revision: "abcdef1234567890",
  features: { status: true },
};

const status = {
  config_revision: "abcdef1234567890",
  providers: 2,
  deployments: 3,
  virtual_models: 4,
  served_model_names: ["default", "fast"],
  credentials: {
    observed_at: "2026-08-02T12:00:00Z",
    status: "healthy",
    configured_references: 1,
    resolved_references: 1,
    failing_references: 0,
    unresolved_references: 0,
    sources: [{
      scheme: "env",
      status: "healthy",
      configured_references: 1,
      resolved_references: 1,
      failing_references: 0,
      unresolved_references: 0,
      resolution_attempts: 12,
      resolution_failures: 0,
      rotations: 1,
      last_resolved_at: "2026-08-02T12:00:00Z",
      last_rotated_at: "2026-08-02T11:00:00Z",
    }],
  },
  privacy: {
    observed_at: "2026-08-02T12:00:00Z",
    state: "healthy",
    enabled: true,
    configured_models: 2,
    conversation_models: 1,
    media_required_models: 1,
    file_inspection_models: 1,
    detector_entities: ["credit_card", "email", "ipv4", "phone", "ssn"],
    backend: "sqlite",
    live_mappings: 7,
    live_conversations: 1,
    live_original_bytes: 128,
    expired_pending_prune: 0,
    maximum_mappings: 4096,
    maximum_original_bytes: 4194304,
    maximum_conversations: 100000,
    sliding_ttl_seconds: 86400,
    absolute_ttl_seconds: 2592000,
    inspections: 9,
    failures: 1,
  },
  targets: [
    {
      deployment: "openai-primary",
      circuit_state: "closed",
      admission_available: true,
      active_requests: 2,
      max_concurrency: 8,
      consecutive_failures: 0,
      samples: 12,
      failures: 1,
      ejection_count: 0,
      half_open_probe_active: false,
      recent_failures: [{
        occurred_at: "2026-08-02T12:00:00Z",
        failure_class: "upstream_http_503",
      }],
    },
  ],
};

afterEach(() => {
  cleanup();
  window.history.replaceState({}, "", "/admin/");
  localStorage.clear();
  sessionStorage.clear();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("AdminApp", () => {
  it("renders the live standalone overview", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse(bootstrap))
      .mockResolvedValueOnce(jsonResponse(status));
    vi.stubGlobal("fetch", fetchMock);

    render(
      <AdminApp productName="SparkRoute" />,
    );

    expect(await screen.findByText("openai-primary")).toBeInTheDocument();
    expect(screen.getByText("4")).toBeInTheDocument();
    expect(screen.getByText("Admitting")).toBeInTheDocument();
    expect(screen.getByText("1.2.3")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "AGPL-3.0-only · Source abcd1234abcd" })).toHaveAttribute("href", bootstrap.build.source);
    expect(screen.queryByRole("region", { name: "Credential source status" })).not.toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "PII privacy status" })).not.toBeInTheDocument();
  });

  it("does not infer enterprise presentation from a cluster edition", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse({
        ...bootstrap,
        edition: "cluster",
        principal: {
          id: "admin-a",
          tenant: "tenant-a",
          roles: ["status_read"],
        },
      }))
      .mockResolvedValueOnce(jsonResponse(status));
    vi.stubGlobal("fetch", fetchMock);

    render(<AdminApp productName="SparkRoute" />);

    expect(await screen.findByText("openai-primary")).toBeInTheDocument();
    expect(screen.getByText("cluster profile")).toBeInTheDocument();
    expect(screen.queryByText("Shared control plane enabled")).not.toBeInTheDocument();
  });

  it("shows and runs the redacted multimedia projection probe", async () => {
    const projectionStatus = {
      configured: true,
      state: "unknown",
      provider: "pilco-mmbridge",
      analyzer_model: "vision-analyzer",
      models: 0,
      consecutive_failures: 0,
      circuit_open: false,
    };
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse({
        ...bootstrap,
        features: { status: true, config_read: true, mm_projection_probe: true },
      }))
      .mockResolvedValueOnce(jsonResponse({ ...status, mm_projection: projectionStatus }))
      .mockResolvedValueOnce(jsonResponse({
        ...projectionStatus,
        state: "healthy",
        projection_api: 1,
        models: 2,
        last_attempt_at: "2026-08-13T06:00:00Z",
        last_success_at: "2026-08-13T06:00:00Z",
        last_http_status: 200,
        last_latency_ms: 12,
      }));
    vi.stubGlobal("fetch", fetchMock);

    render(<AdminApp productName="SparkRoute" />);
    await screen.findByRole("link", { name: "Advanced Options" });
    expect(screen.queryByText("MMBridge projection")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("link", { name: "Advanced Options" }));
    const projectionPanel = (await screen.findByText("MMBridge projection")).closest("section");
    expect(projectionPanel).not.toBeNull();
    expect(within(projectionPanel!).getByText("Unknown")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Test bridge" }));
    expect(await within(projectionPanel!).findByText("Healthy")).toBeInTheDocument();
    expect(within(projectionPanel!).getByText("v1")).toBeInTheDocument();
    expect(within(projectionPanel!).getByText("12 ms")).toBeInTheDocument();
    expect(fetchMock.mock.calls[2]?.[0]).toBe("/v1/mm-projection/probe");
    expect(fetchMock.mock.calls[2]?.[1]?.method).toBe("POST");
  });

  it("filters target health and drills into bounded content-free failures", async () => {
    const healthStatus = {
      ...status,
      targets: [
        {
          ...status.targets[0],
          admission_available: false,
          circuit_state: "open",
          consecutive_failures: 3,
          ejection_count: 2,
          ejected_until: "2026-08-02T12:05:00Z",
        },
        {
          deployment: "backup-healthy",
          circuit_state: "closed",
          admission_available: true,
          active_requests: 0,
          max_concurrency: 0,
          consecutive_failures: 0,
          samples: 4,
          failures: 0,
          ejection_count: 0,
          half_open_probe_active: false,
          recent_failures: [],
        },
      ],
    };
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse(bootstrap))
      .mockResolvedValueOnce(jsonResponse(healthStatus));
    vi.stubGlobal("fetch", fetchMock);

    render(<AdminApp productName="SparkRoute" />);
    fireEvent.click(await screen.findByRole("button", { name: "openai-primary" }));

    const detail = screen.getByRole("region", { name: "openai-primary target health" });
    expect(within(detail).getByText("upstream_http_503")).toBeInTheDocument();
    expect(within(detail).getByText(/maximum eight retained/)).toBeInTheDocument();
    expect(within(detail).getByText(/excludes request content, raw errors/)).toBeInTheDocument();
    expect(within(detail).getByText("2")).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Availability"), {
      target: { value: "admitting" },
    });
    expect(screen.queryByRole("button", { name: "openai-primary" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "backup-healthy" })).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Recent failures"), {
      target: { value: "recent" },
    });
    expect(screen.getByText("No targets match these filters")).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Availability"), {
      target: { value: "all" },
    });
    fireEvent.change(screen.getByLabelText("Filter deployments"), {
      target: { value: "openai" },
    });
    expect(screen.getByRole("button", { name: "openai-primary" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "backup-healthy" })).not.toBeInTheDocument();
  });

  it("keeps a bearer token in memory while authenticating", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        jsonResponse(
          { error: { message: "admin authentication failed" } },
          401,
        ),
      )
      .mockResolvedValueOnce(jsonResponse(bootstrap))
      .mockResolvedValueOnce(jsonResponse(status));
    vi.stubGlobal("fetch", fetchMock);

    render(
      <AdminApp productName="SparkRoute" />,
    );
    expect(await screen.findByText("Connect to admin")).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Bearer token"), {
      target: { value: "secret-admin-token" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Connect securely" }));

    expect(await screen.findByText("openai-primary")).toBeInTheDocument();
    await waitFor(() => {
      const headers = fetchMock.mock.calls[1]?.[1]?.headers as Headers;
      expect(headers.get("Authorization")).toBe("Bearer secret-admin-token");
    });
    expect(sessionStorage.length).toBe(0);
    expect(localStorage.length).toBe(0);
  });

  it("shows capability-gated runtime controllers, endpoints, and transitions", async () => {
    const runtimeBootstrap = {
      ...bootstrap,
      features: {
        status: false,
        lifecycle: true,
        lifecycle_status: true,
        endpoint_inventory: true,
        runtime_events: true,
      },
    };
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input);
      if (path === "/v1/ui/bootstrap") return jsonResponse(runtimeBootstrap);
      if (path.startsWith("/v1/runtime/status")) return jsonResponse({
        observed_at: "2026-08-03T12:00:00Z",
        controllers: [{
          controller: "sparkrun",
          health: "healthy",
          updated_at: "2026-08-03T12:00:00Z",
          bindings: 1,
          endpoints: 1,
          ready_endpoints: 1,
          active_leases: 2,
          queued_waiters: 3,
          queued_body_bytes: 1024,
        }],
        bindings: [{
          controller: "sparkrun",
          binding_revision: "binding-a",
          virtual_model: "public-model",
          deployment: "deployment-a",
          state: "activating",
          updated_at: "2026-08-03T12:00:00Z",
          active_leases: 0,
          queued_waiters: 3,
          queued_body_bytes: 1024,
          max_queued_waiters: 10,
          max_queued_body_bytes: 4096,
        }],
      });
      if (path.startsWith("/v1/runtime/endpoints")) return jsonResponse({
        endpoints: [{
          endpoint_id: "endpoint-a",
          target: "deployment-a",
          protocol: "openai",
          served_models: ["upstream-a"],
          controller: "sparkrun",
          state: "ready",
          active_requests: 2,
          max_concurrency: 8,
          heartbeat_at: "2026-08-03T12:00:00Z",
          expired: false,
          base_url: "https://must-not-render.invalid",
          metadata: { secret: "must-not-render" },
        }],
      });
      if (path.startsWith("/v1/runtime/events")) return jsonResponse({
        records: [{
          event_id: "event-a",
          occurred_at: "2026-08-03T12:00:00Z",
          controller: "sparkrun",
          binding_revision: "binding-a",
          virtual_model: "public-model",
          deployment: "deployment-a",
          prior_state: "activating",
          new_state: "ready",
          reason: "readiness_succeeded",
          latency: 4_000_000_000,
          queue_depth: 3,
          waiter_count: 2,
          outcome: "success",
        }],
      });
      return jsonResponse({ error: { message: "not found" } }, 404);
    });
    vi.stubGlobal("fetch", fetchMock);

    render(<AdminApp productName="SparkRoute" />);
    fireEvent.click(await screen.findByText("Runtime"));
    expect(await screen.findByText("Runtime controllers")).toBeInTheDocument();
    expect(screen.getByText("3 · 1.0 KiB")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("tab", { name: "Endpoints" }));
    expect(screen.getByText("endpoint-a")).toBeInTheDocument();
    expect(screen.queryByText(/must-not-render/)).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("tab", { name: "Transitions" }));
    expect(screen.getByText("readiness_succeeded")).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Transition controller"), {
      target: { value: "sparkrun" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Apply filters" }));
    await waitFor(() => expect(fetchMock.mock.calls.some(
      ([path]) => String(path).startsWith("/v1/runtime/events?") &&
        String(path).includes("controller=sparkrun"),
    )).toBe(true));
  });

  it("validates an standalone configuration draft without loading active secrets", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        jsonResponse({
          ...bootstrap,
          features: { status: false, config_validate: true },
        }),
      )
      .mockResolvedValueOnce(
        jsonResponse({
          valid: true,
          revision: "1234567890abcdef1234567890abcdef",
        }),
      );
    vi.stubGlobal("fetch", fetchMock);

    render(<AdminApp productName="SparkRoute" />);
    fireEvent.click(await screen.findByText("Configuration"));
    expect(screen.getByText("Local validation draft")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));

    expect(
      await screen.findByText(/Configuration is valid/),
    ).toBeInTheDocument();
    const validationCall = fetchMock.mock.calls[1];
    expect(validationCall?.[0]).toBe("/v1/config/validate");
    expect(validationCall?.[1]?.method).toBe("POST");
    expect(validationCall?.[1]?.body).toContain('"providers":[]');
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("edits and simulates model-routing policy through the runtime-faithful admin API", async () => {
    const document = {
      providers: [{ name: "local", type: "openai_compatible", base_url: "http://127.0.0.1:8000/v1" }],
      deployments: [
        { name: "text-deployment", provider: "local", model: "text" },
        { name: "vision-deployment", provider: "local", model: "vision", capabilities: ["vision"] },
      ],
      virtual_models: [
        { name: "text-model", pools: [{ targets: [{ deployment: "text-deployment", weight: 100 }] }] },
        { name: "vision-model", pools: [{ targets: [{ deployment: "vision-deployment", weight: 100 }] }] },
      ],
      model_routing: {
        version: 2,
        revision: 1,
        default_virtual_model: "auto",
        virtual_models: {
          auto: {
            strategy: "balanced",
            models: ["text-model", "vision-model"],
            aliases: ["smart"],
            kwargs: { private: { provider_priority: ["vision-provider", "fallback"] } },
          },
        },
        models: {
          "text-model": { enabled: true, weight: 100, size_b: 8 },
          "vision-model": { enabled: true, weight: 100, size_b: 12 },
        },
        keyword_rules: [],
      },
    };
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input);
      if (path === "/v1/ui/bootstrap") return jsonResponse({
        ...bootstrap,
        features: {
          status: false,
          config_read: true,
          config_validate: true,
          config_history: true,
          config_routing_simulation: true,
        },
      });
      if (path === "/v1/config") return jsonResponse({ revision: bootstrap.config_revision, document });
      if (path === "/v1/config/simulate-routing" && init?.method === "POST") return jsonResponse({
        decision: {
          requested_model: "auto",
          resolved_model: "vision-model",
          virtual_model: "auto",
          strategy: "stage_router",
          matched_rule: "vision-rule",
          provider_priority: ["vision-provider", "fallback"],
          reason: "matched vision-rule; selected by stage_router",
          revision: 1,
          candidates: [{ model: "vision-model", eligible: true, score: 0.5 }],
          at: "2026-08-13T00:00:00Z",
        },
        available_models: ["vision-model"],
        required_capabilities: ["vision"],
        state_mode: "stateless",
      });
      return jsonResponse({ error: { message: `Unexpected request ${path}` } }, 500);
    });
    vi.stubGlobal("fetch", fetchMock);
    window.history.replaceState({}, "", "/admin/configuration");

    render(<AdminApp productName="SparkRoute" />);
    expect(await screen.findByText("Current configuration")).toBeInTheDocument();
    expect(screen.queryByText("Revision history")).not.toBeInTheDocument();
    expect(screen.queryByText("Activation history")).not.toBeInTheDocument();
    expect(fetchMock.mock.calls.some(([path]) => String(path).includes("/v1/config/revisions"))).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: "Model routing" }));
    fireEvent.change(screen.getByLabelText("Routing strategy"), { target: { value: "stage_router" } });
    fireEvent.change(screen.getByLabelText("Capable model"), { target: { value: "vision-model" } });
    fireEvent.change(screen.getByLabelText("Ambiguous-signal default"), { target: { value: "capable_first" } });
    fireEvent.change(screen.getByLabelText("Stage confidence threshold"), { target: { value: "0.7" } });
    fireEvent.change(screen.getByLabelText("Stage recent turn window"), { target: { value: "5" } });
    fireEvent.change(screen.getByLabelText("Latest user routing text"), { target: { value: "inspect this image" } });
    fireEvent.click(screen.getByText("Required capabilities"));
    fireEvent.click(screen.getByLabelText("Vision"));
    fireEvent.click(screen.getByRole("button", { name: "Simulate" }));

    const result = await screen.findByRole("region", { name: "Routing simulation result" });
    expect(result).toHaveTextContent("vision-model");
    expect(result).toHaveTextContent("vision-rule");
    expect(result).toHaveTextContent("vision-provider → fallback");
    const simulationCall = fetchMock.mock.calls.find(
      ([path, init]) => path === "/v1/config/simulate-routing" && init?.method === "POST",
    );
    expect(simulationCall?.[1]?.body).toContain('"strategy":"stage_router"');
    expect(simulationCall?.[1]?.body).toContain('"models":["vision-model","text-model"]');
    expect(simulationCall?.[1]?.body).toContain('"capable_model":"vision-model"');
    expect(simulationCall?.[1]?.body).toContain('"efficient_model":"text-model"');
    expect(simulationCall?.[1]?.body).toContain('"picker":"capable_first"');
    expect(simulationCall?.[1]?.body).toContain('"confidence_threshold":0.7');
    expect(simulationCall?.[1]?.body).toContain('"recent_turn_window":5');
    expect(simulationCall?.[1]?.body).toContain('"required_capabilities":["vision"]');
    expect(simulationCall?.[1]?.body).toContain('"routing_text":"inspect this image"');
    expect(simulationCall?.[1]?.body).toContain('"provider_priority":["vision-provider","fallback"]');
  });

  it("shows discovered routing metadata with provenance and an operator opt-out", async () => {
    const document = {
      providers: [{ name: "local", type: "openai_compatible", base_url: "http://127.0.0.1:8000/v1" }],
      deployments: [{ name: "local-deployment", provider: "local", model: "upstream" }],
      virtual_models: [{
        name: "local-chat",
        pools: [{ targets: [{ deployment: "local-deployment", weight: 100 }] }],
      }],
      model_routing: {
        version: 2,
        revision: 1,
        default_virtual_model: "auto",
        virtual_models: { auto: { strategy: "smallest", models: ["local-chat"] } },
        models: { "local-chat": { enabled: true } },
      },
    };
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input);
      if (path === "/v1/ui/bootstrap") return jsonResponse({
        ...bootstrap,
        features: {
          status: false,
          config_read: true,
          model_routing_discovered_metadata: true,
        },
      });
      if (path === "/v1/config") return jsonResponse({ revision: bootstrap.config_revision, document });
      if (path === "/v1/model-routing/discovered-metadata") return jsonResponse({
        generation: 4,
        sources: [{
          source: "sparkrun:test-generation",
          observed_at: "2026-08-13T12:00:00Z",
          models: { "local-chat": { size_b: 32.5, context: 65_536, tags: ["local", "vllm"] } },
        }],
        effective: {
          "local-chat": { size_b: 32.5, context: 65_536, tags: ["local", "vllm"] },
        },
      });
      return jsonResponse({ error: { message: `Unexpected request ${path}` } }, 500);
    });
    vi.stubGlobal("fetch", fetchMock);
    window.history.replaceState({}, "", "/admin/configuration");

    render(<AdminApp productName="SparkRoute" />);
    expect(await screen.findByText("Current configuration")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Model routing" }));

    expect(await screen.findByText("Discovered: 65,536")).toBeInTheDocument();
    expect(screen.getByText(/sparkrun:test-generation/)).toBeInTheDocument();
    expect(screen.getByText("Discovered: local, vllm")).toBeInTheDocument();
    const optOut = screen.getByRole("checkbox", { name: "Ignore discovered strategy metadata" });
    expect(optOut).not.toBeChecked();
    fireEvent.click(optOut);
    expect(optOut).toBeChecked();
  });

  it("hides multimedia projection controls while preserving existing configuration", async () => {
    const document = {
      providers: [{ name: "local", type: "openai_compatible", base_url: "http://127.0.0.1:8000/v1" }],
      deployments: [{ name: "deployment", provider: "local", model: "upstream" }],
      virtual_models: [
        { name: "text-model", pools: [{ targets: [{ deployment: "deployment", weight: 100 }] }] },
        { name: "vision-model", pools: [{ targets: [{ deployment: "deployment", weight: 100 }] }] },
      ],
      model_routing: {
        version: 2,
        revision: 1,
        default_virtual_model: "auto",
        virtual_models: { auto: { strategy: "balanced", models: ["text-model", "vision-model"], mm_projection: { analyzer_model: "selector-analyzer", failure_mode: "fail_closed", timeout_ms: 45000 } } },
        models: {
          "text-model": { enabled: true, weight: 100, mm_projection_disabled: true },
          "vision-model": { enabled: true, weight: 100, mm_projection: { failure_mode: "fallback", analyzer_model: "model-analyzer" } },
        },
      },
    };
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input);
      if (path === "/v1/ui/bootstrap") return jsonResponse({
        ...bootstrap,
        features: { status: false, config_read: true, config_validate: true },
      });
      if (path === "/v1/config") return jsonResponse({ revision: bootstrap.config_revision, document });
      return jsonResponse({ error: { message: `Unexpected request ${path}` } }, 500);
    });
    vi.stubGlobal("fetch", fetchMock);
    window.history.replaceState({}, "", "/admin/configuration");

    render(<AdminApp productName="SparkRoute" />);
    expect(await screen.findByText("Current configuration")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Model routing" }));
    expect(await screen.findByLabelText("text-model")).toBeChecked();
    expect(screen.queryByText("Multimedia projection")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Enable by default")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Projection behavior")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Analyzer model")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "JSON" }));

    const edited = JSON.parse(
      (screen.getByLabelText("Gateway configuration JSON") as HTMLTextAreaElement).value,
    );
    expect(edited.model_routing.virtual_models.auto.mm_projection).toEqual({
      analyzer_model: "selector-analyzer",
      failure_mode: "fail_closed",
      timeout_ms: 45000,
    });
    expect(edited.model_routing.models["text-model"]).toMatchObject({
      mm_projection_disabled: true,
    });
    expect(edited.model_routing.models["vision-model"].mm_projection).toEqual({
      failure_mode: "fallback",
      analyzer_model: "model-analyzer",
    });
  });

  it("edits only the operator managed set and keeps sparkrun generated configuration read-only", async () => {
    const activeRevision = "a".repeat(64);
    const nextRevision = "b".repeat(64);
    const empty = { providers: [], deployments: [], virtual_models: [] };
    const generated = {
      providers: [{ name: "sparkrun", type: "openai_compatible", base_url: "http://127.0.0.1:8000/v1" }],
      deployments: [{ name: "generated-target", title: "sparkrun:spark-a:upstream", provider: "sparkrun", model: "upstream" }],
      virtual_models: [{ name: "new-model", aliases: ["new-model-2"], pools: [{ targets: [{ deployment: "generated-target", weight: 1 }] }] }],
    };
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input);
      const method = init?.method ?? "GET";
      if (path === "/v1/ui/bootstrap") return jsonResponse({
        ...bootstrap,
        config_revision: activeRevision,
        features: {
          status: false,
          config_read: true,
          config_write: true,
          config_managed_sets: true,
        },
      });
      if (path === "/v1/config/managed-sets") return jsonResponse({
        active_revision: activeRevision,
        managed_sets: [
          { owner: "operator", revision: "1".repeat(64), updated_at: "2026-08-04T12:00:00Z", updated_by: "operator-a" },
          { owner: "sparkrun", revision: "2".repeat(64), updated_at: "2026-08-04T12:00:00Z", updated_by: "sparkrun-a" },
        ],
      });
      if (path === "/v1/config/managed-sets/operator" && method === "GET") return jsonResponse({
        owner: "operator", revision: "1".repeat(64), updated_at: "2026-08-04T12:00:00Z",
        updated_by: "operator-a", document: empty,
      });
      if (path === "/v1/config/managed-sets/sparkrun" && method === "GET") return jsonResponse({
        owner: "sparkrun", revision: "2".repeat(64), updated_at: "2026-08-04T12:00:00Z",
        updated_by: "sparkrun-a", document: generated,
      });
      if (path === "/v1/config") return jsonResponse({ revision: activeRevision, document: generated });
      if (path === "/v1/config/managed-sets/operator/validate" && method === "POST") return jsonResponse({
        owner: "operator", set_revision: "3".repeat(64), active_revision: activeRevision,
        candidate_revision: nextRevision, valid: true,
      });
      if (path === "/v1/config/managed-sets/operator" && method === "PUT") return jsonResponse({
        managed_set: { owner: "operator", revision: "3".repeat(64), updated_at: "2026-08-04T12:01:00Z", updated_by: "operator-a" },
		current: { revision: nextRevision, updated_at: "2026-08-04T12:01:00Z", updated_by: "operator-a" },
        changed: true,
      });
      return jsonResponse({ error: { message: "not found" } }, 404);
    });
    vi.stubGlobal("fetch", fetchMock);

    render(<AdminApp productName="SparkRoute" />);
    fireEvent.click(await screen.findByText("Configuration"));
    expect(await screen.findByRole("heading", { name: "Providers", level: 2 })).toBeInTheDocument();
    const tree = within(screen.getByRole("list", { name: "Configuration sections" }));
    expect(tree.getAllByRole("link")).toHaveLength(5);
    fireEvent.click(tree.getByRole("link", { name: "Virtual Models / Aliases" }));
    expect(window.location.pathname).toBe("/admin/configuration/models");
    fireEvent.click(await screen.findByTitle("Add virtual model"));
    expect(screen.getByRole("option", { name: "sparkrun:spark-a:upstream" })).toBeInTheDocument();
    expect(screen.getByLabelText("Canonical name")).toHaveValue("new-model-3");
    fireEvent.change(screen.getByLabelText("Canonical name"), { target: { value: "assistant" } });
    fireEvent.change(screen.getByRole("textbox", { name: /Aliases/ }), { target: { value: "chat, coding" } });
    fireEvent.blur(screen.getByRole("textbox", { name: /Aliases/ }));
    fireEvent.click(tree.getByRole("link", { name: "Model Deployments" }));
    expect(screen.getByLabelText("Deployment ID")).toBeDisabled();
    expect(screen.getByRole("button", { name: "Remove" })).toBeDisabled();
    fireEvent.click(tree.getByRole("link", { name: "Model Routing" }));
    fireEvent.click(screen.getByRole("button", { name: "Enable model routing" }));
    expect(screen.getByLabelText("new-model", { selector: "input" })).toBeChecked();
    expect(screen.getByLabelText("assistant", { selector: "input" })).toBeChecked();
    fireEvent.click(screen.getByRole("link", { name: /Overview/ }));
    expect(screen.queryByRole("button", { name: "Validate and replace operator set" })).not.toBeInTheDocument();
    fireEvent.click(tree.getByRole("link", { name: "Virtual Models / Aliases" }));
    expect(screen.getByLabelText("Canonical name")).toHaveValue("assistant");
    expect(screen.getByRole("textbox", { name: /Aliases/ })).toHaveValue("chat, coding");
    expect(screen.getByText("Unsaved changes")).toBeVisible();
    // A popstate navigation changes only the active section, preserving the shared draft.
    window.history.replaceState({}, "", "/admin/configuration/providers");
    fireEvent.popState(window);
    expect(tree.getByRole("link", { name: "Providers" })).toHaveAttribute("aria-current", "page");
    fireEvent.click(screen.getByRole("button", { name: "JSON" }));
    const candidate = JSON.parse((screen.getByLabelText("operator configuration JSON") as HTMLTextAreaElement).value);
    expect(candidate.providers).toEqual([]);
    expect(candidate.deployments).toEqual([]);
    expect(candidate.virtual_models).toHaveLength(1);
    expect(candidate.virtual_models[0]).toMatchObject({ name: "assistant", aliases: ["chat", "coding"], pools: [{ targets: [{ deployment: "generated-target", weight: 100 }] }] });
    expect(screen.queryByLabelText("Change reason")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));
    await screen.findByRole("button", { name: "Save" });
    fireEvent.change(screen.getByLabelText("operator configuration JSON"), { target: { value: JSON.stringify(candidate) } });
    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));
    fireEvent.click(await screen.findByRole("button", { name: "Save" }));
	expect(await screen.findByText(/Stored the current configuration/)).toBeInTheDocument();
    expect(fetchMock.mock.calls.some(([path, options]) =>
      String(path) === "/v1/config/managed-sets/operator" && options?.method === "PUT",
    )).toBe(true);

    const saveCall = fetchMock.mock.calls.find(([path, options]) => path === "/v1/config/managed-sets/operator" && options?.method === "PUT");
    expect(JSON.parse(String(saveCall?.[1]?.body)).document).toEqual(candidate);
    expect(fetchMock.mock.calls.some(([path, options]) => path === "/v1/config/managed-sets/sparkrun" && options?.method === "PUT")).toBe(false);
    expect(screen.queryByRole("link", { name: "sparkrun Generated" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Structured" }));
    expect(screen.getByLabelText("Provider name")).toBeDisabled();
    fireEvent.click(tree.getByRole("link", { name: "Virtual Models / Aliases" }));
    fireEvent.click(screen.getByRole("button", { name: /new-model.*Read only/ }));
    expect(screen.getByLabelText("Canonical name")).toBeDisabled();
    expect(screen.getByRole("button", { name: "Remove model" })).toBeDisabled();
    fireEvent.click(tree.getByRole("link", { name: "Providers" }));
    fireEvent.click(screen.getByRole("button", { name: "Add provider" }));
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));
    expect(await screen.findByRole("button", { name: "Save" })).toBeEnabled();
    // Native input events must retain their value when clearing validation.
    fireEvent.input(screen.getByLabelText("Provider name"), { target: { value: "renamed-provider" } });
    expect(screen.getByLabelText("Provider name")).toHaveValue("renamed-provider");
    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));
    expect(await screen.findByRole("button", { name: "Save" })).toBeEnabled();
    fireEvent.click(screen.getByText("Provider extra body defaults"));
    fireEvent.input(screen.getByLabelText("Provider extra body defaults JSON"), { target: { value: "{broken" } });
    expect(screen.getByLabelText("Provider extra body defaults JSON")).toHaveValue("{broken");
    fireEvent.blur(screen.getByLabelText("Provider extra body defaults JSON"));
    const validations = () => fetchMock.mock.calls.filter(([path]) => String(path).endsWith("/operator/validate")).length;
    const beforeInvalid = validations();
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));
    expect(validations()).toBe(beforeInvalid);
    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
    expect(screen.getByText(/Correct the invalid field/)).toBeVisible();
    fireEvent.change(screen.getByLabelText("Provider extra body defaults JSON"), { target: { value: '{"temperature":0.2}' } });
    fireEvent.blur(screen.getByLabelText("Provider extra body defaults JSON"));
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));
    expect(await screen.findByRole("button", { name: "Save" })).toBeEnabled();
  });

  it("simulates an operator routing draft against sparkrun-owned models", async () => {
    const activeRevision = "a".repeat(64);
    const generated = {
      providers: [{ name: "sparkrun", type: "openai_compatible", base_url: "http://127.0.0.1:8000/v1" }],
      deployments: [{ name: "generated-deployment", provider: "sparkrun", model: "upstream" }],
      virtual_models: [{
        name: "generated-model",
        pools: [{ targets: [{ deployment: "generated-deployment", weight: 100 }] }],
      }],
    };
    const operator = {
      providers: [],
      deployments: [],
      virtual_models: [],
      model_routing: {
        version: 2,
        revision: 1,
        default_virtual_model: "auto",
        virtual_models: { auto: { strategy: "balanced", models: ["generated-model"] } },
        models: { "generated-model": { enabled: true, weight: 100 } },
      },
    };
    const merged = {
      providers: generated.providers,
      deployments: generated.deployments,
      virtual_models: generated.virtual_models,
      model_routing: operator.model_routing,
    };
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input);
      const method = init?.method ?? "GET";
      if (path === "/v1/ui/bootstrap") return jsonResponse({
        ...bootstrap,
        config_revision: activeRevision,
        features: {
          status: false,
          config_read: true,
          config_write: true,
          config_managed_sets: true,
          config_routing_simulation: true,
        },
      });
      if (path === "/v1/config/managed-sets") return jsonResponse({
        active_revision: activeRevision,
        managed_sets: [
          { owner: "operator", revision: "1".repeat(64), updated_at: "2026-08-04T12:00:00Z", updated_by: "operator-a" },
          { owner: "sparkrun", revision: "2".repeat(64), updated_at: "2026-08-04T12:00:00Z", updated_by: "sparkrun-a" },
        ],
      });
      if (path === "/v1/config/managed-sets/operator" && method === "GET") return jsonResponse({
        owner: "operator", revision: "1".repeat(64), updated_at: "2026-08-04T12:00:00Z",
        updated_by: "operator-a", document: operator,
      });
      if (path === "/v1/config/managed-sets/sparkrun" && method === "GET") return jsonResponse({
        owner: "sparkrun", revision: "2".repeat(64), updated_at: "2026-08-04T12:00:00Z",
        updated_by: "sparkrun-a", document: generated,
      });
      if (path === "/v1/config") return jsonResponse({ revision: activeRevision, document: merged });
      if (path === "/v1/config/managed-sets/operator/simulate-routing" && method === "POST") return jsonResponse({
        decision: {
          requested_model: "auto", resolved_model: "generated-model", virtual_model: "auto",
          strategy: "balanced", reason: "selected by balanced", revision: 1,
          candidates: [{ model: "generated-model", eligible: true }], at: "2026-08-13T00:00:00Z",
        },
        available_models: ["generated-model"], state_mode: "stateless",
      });
      return jsonResponse({ error: { message: `Unexpected request ${path}` } }, 500);
    });
    vi.stubGlobal("fetch", fetchMock);

    render(<AdminApp productName="SparkRoute" />);
    fireEvent.click(await screen.findByText("Configuration"));
    expect(await screen.findByRole("heading", { name: "Providers", level: 2 })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("link", { name: "Model Routing" }));
    expect(screen.getByLabelText("generated-model")).toBeChecked();
    fireEvent.click(screen.getByRole("button", { name: "Simulate" }));
    expect(await screen.findByRole("region", { name: "Routing simulation result" })).toHaveTextContent("generated-model");
    const simulationCall = fetchMock.mock.calls.find(
      ([path, init]) => path === "/v1/config/managed-sets/operator/simulate-routing" && init?.method === "POST",
    );
    expect(simulationCall?.[1]?.body).toContain(`"expected_active_revision":"${activeRevision}"`);
    expect(simulationCall?.[1]?.body).toContain('"virtual_models":[]');
  });

  it("round-trips structured virtual-model routing changes without dropping guardrails", async () => {
    const activeRevision = "d".repeat(64);
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input);
      if (path === "/v1/ui/bootstrap") {
        return jsonResponse({
          ...bootstrap,
          edition: "cluster",
          config_revision: activeRevision,
          features: { status: false, config_read: true, config_validate: true, privacy_pii: true },
          principal: { id: "config-reader", roles: ["config_read"] },
        });
      }
      if (path === "/v1/config") {
        return jsonResponse({
          revision: activeRevision,
          document: {
            providers: [],
            deployments: [
              { name: "deploy-a", provider: "provider", model: "a" },
              { name: "deploy-b", provider: "provider", model: "b" },
              { name: "deploy-c", provider: "provider", model: "c" },
            ],
            virtual_models: [{
              name: "chat",
              aliases: ["default"],
              visibility: "hidden",
              required_capabilities: ["tools", "vision"],
              selection: { prompt_cache_affinity: { enabled: true, ttl: "7m", min_prefix_bytes: 8192, max_prefixes_per_request: 12, scope: "tenant" } },
              guardrails: {
                pre: [{ name: "safety", model: "guard", prompt: "preserve me" }],
              },
              privacy: { pii: { mode: "substitute", scope: "request" } },
              pools: [{
                priority: 0,
                targets: [
                  { deployment: "deploy-a", weight: 70 },
                  { deployment: "deploy-b", weight: 30 },
                ],
              }],
            }],
          },
        });
      }
      if (path === "/v1/config/validate" && init?.method === "POST") {
        return jsonResponse({ valid: true, revision: "e".repeat(64) });
      }
      return jsonResponse({ error: { message: `Unexpected request ${path}` } }, 500);
    });
    vi.stubGlobal("fetch", fetchMock);
    window.history.replaceState({}, "", "/admin/configuration");

    render(<AdminApp productName="SparkRoute" />);
    const name = await screen.findByLabelText("Canonical name");
    fireEvent.change(name, { target: { value: "chat-v2" } });
    const aliases = screen.getByLabelText(/^Aliases/);
    fireEvent.change(aliases, { target: { value: "default, production" } });
    fireEvent.blur(aliases);
    expect(screen.queryByText("Required capabilities")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Vision")).not.toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Selection"), { target: { value: "weighted_hash" } });
    fireEvent.change(screen.getByLabelText(/^Stable hash key/), { target: { value: "thread_id" } });
    expect(screen.queryByText("Prompt-cache route affinity")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Enable prompt-cache affinity")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Single Vector Embedding")).not.toBeInTheDocument();
    fireEvent.change(screen.getAllByLabelText("Relative weight")[0]!, {
      target: { value: "60" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add fallback pool" }));
    fireEvent.click(screen.getByRole("button", { name: "JSON" }));

    const document = JSON.parse(
      (screen.getByLabelText("Gateway configuration JSON") as HTMLTextAreaElement).value,
    );
    expect(document.virtual_models[0]).toMatchObject({
      name: "chat-v2",
      aliases: ["default", "production"],
      required_capabilities: ["tools", "vision"],
      guardrails: {
        pre: [{ name: "safety", model: "guard", prompt: "preserve me" }],
      },
      selection: {
        mode: "weighted_hash",
        hash_key: "thread_id",
        prompt_cache_affinity: {
          enabled: true,
          ttl: "7m",
          min_prefix_bytes: 8192,
          max_prefixes_per_request: 12,
          scope: "tenant",
        },
      },
      privacy: {
        pii: {
          mode: "substitute",
          scope: "request",
        },
      },
      pools: [
        { priority: 0, targets: [{ deployment: "deploy-a", weight: 60 }, { deployment: "deploy-b", weight: 30 }] },
        { priority: 1, targets: [{ deployment: "deploy-c", weight: 100 }] },
      ],
    });
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));
    expect(await screen.findByText(/Configuration is valid/)).toBeInTheDocument();
    const validationCall = fetchMock.mock.calls.find(
      ([path, init]) => path === "/v1/config/validate" && init?.method === "POST",
    );
    expect(validationCall?.[1]?.body).toContain('"vision"');
    expect(validationCall?.[1]?.body).toContain('"preserve me"');
  });

  it("edits provider and deployment policy while redacting configured header literals", async () => {
    const activeRevision = "f".repeat(64);
    const configuredHeader = "configured-literal-must-not-render";
    const deploymentHeader = "deployment-literal-must-not-render";
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input);
      if (path === "/v1/ui/bootstrap") {
        return jsonResponse({
          ...bootstrap,
          edition: "cluster",
          config_revision: activeRevision,
          features: { status: false, config_read: true, config_validate: true },
          principal: { id: "config-reader", roles: ["config_read"] },
        });
      }
      if (path === "/v1/config") {
        return jsonResponse({
          revision: activeRevision,
          document: {
            providers: [{
              name: "provider-a",
              type: "openai_compatible",
              base_url: "https://provider.example/v1",
              auth: {
                type: "header",
                header: "X-Provider-Key",
                credential: "env://PROVIDER_KEY",
                future_auth: "preserve-auth",
              },
              default_headers: {
                "X-API-Version": {
                  value: configuredHeader,
                  scope: "both",
                  future_header: "preserve-header",
                },
                "X-Request-Token": {
                  value_from: "k8s://gateway-secrets/provider#token",
                  scope: "inference",
                },
              },
              extra_body: { service_tier: "priority", future_default: { preserve: true } },
              future_provider: { preserve: true },
            }],
            deployments: [{
              name: "deploy-a",
              provider: "provider-a",
              model: "upstream-a",
              credential: "env://DEPLOYMENT_KEY",
              capabilities: ["tools", "x-existing-feature"],
              max_concurrency: 64,
              circuit: {
                minimum_samples: 20,
                sample_window: 50,
                failure_rate: 0.5,
                future_circuit: "preserve-circuit",
              },
              upstream_headers: {
                "X-Model-Profile": { value: deploymentHeader, scope: "health" },
              },
              extra_body: { reasoning_effort: "medium" },
              future_deployment: { preserve: true },
            }],
            virtual_models: [{
              name: "chat",
              pools: [{ priority: 0, targets: [{ deployment: "deploy-a", weight: 100 }] }],
            }],
            metadata: { preserve: true },
          },
        });
      }
      if (path === "/v1/config/validate" && init?.method === "POST") {
        return jsonResponse({ valid: true, revision: "1".repeat(64) });
      }
      return jsonResponse({ error: { message: `Unexpected request ${path}` } }, 500);
    });
    vi.stubGlobal("fetch", fetchMock);
    window.history.replaceState({}, "", "/admin/configuration");

    render(<AdminApp productName="SparkRoute" />);
    await screen.findByText("Current configuration");
    fireEvent.click(screen.getByRole("button", { name: "Providers & deployments" }));

    expect(await screen.findByLabelText("Provider name")).toHaveValue("provider-a");
    expect(screen.queryByDisplayValue(configuredHeader)).not.toBeInTheDocument();
    expect(screen.getByText("The current literal is intentionally not rendered.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Remove" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Remove" })).toHaveAttribute("title", "Used by 1 deployment");

    fireEvent.change(screen.getByLabelText("Provider name"), { target: { value: "provider-renamed" } });
    fireEvent.change(screen.getByLabelText(/^Credential reference/), { target: { value: "env://PROVIDER_KEY_V2" } });
    const literal = screen.getByLabelText(/^Literal header value/);
    expect(literal).toHaveValue("");
    fireEvent.change(literal, { target: { value: "replacement-literal" } });
    fireEvent.blur(literal);
    expect(literal).toHaveValue("");
    fireEvent.click(screen.getByRole("button", { name: "Add header" }));
    const headerNames = screen.getAllByLabelText("Header name");
    const addedHeaderName = headerNames[headerNames.length - 1]!;
    fireEvent.change(addedHeaderName, { target: { value: "X-Trace-Policy" } });
    fireEvent.blur(addedHeaderName);
    const addedHeaderRow = screen.getByDisplayValue("X-Trace-Policy").closest("article");
    expect(addedHeaderRow).not.toBeNull();
    fireEvent.change(within(addedHeaderRow!).getByLabelText("Value source"), {
      target: { value: "reference" },
    });
    const referenceHeaderRow = screen.getByDisplayValue("X-Trace-Policy").closest("article");
    expect(referenceHeaderRow).not.toBeNull();
    fireEvent.change(within(referenceHeaderRow!).getByLabelText("Header credential reference"), {
      target: { value: "env://TRACE_POLICY_HEADER" },
    });
    fireEvent.change(screen.getByLabelText("Provider extra body defaults JSON"), {
      target: { value: '{"service_tier":"flex","metadata":{"source":"console"}}' },
    });
    fireEvent.blur(screen.getByLabelText("Provider extra body defaults JSON"));

    fireEvent.click(screen.getByRole("button", { name: /deploy-a/ }));
    expect(screen.queryByDisplayValue(deploymentHeader)).not.toBeInTheDocument();
    expect(screen.getByLabelText("Provider")).toHaveValue("provider-renamed");
    expect(screen.getByRole("button", { name: "Remove" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Remove" })).toHaveAttribute("title", "Used by 1 virtual-model target");
    fireEvent.change(screen.getByLabelText("Deployment name"), { target: { value: "deploy-v2" } });
    fireEvent.change(screen.getByLabelText("Upstream model"), { target: { value: "upstream-v2" } });
    fireEvent.change(screen.getByLabelText(/^Maximum concurrency/), { target: { value: "96" } });
    fireEvent.click(screen.getByLabelText("Anthropic"));
    fireEvent.click(screen.getByLabelText("Vision"));
    fireEvent.click(screen.getByText("Concurrency and circuit policy"));
    fireEvent.change(screen.getByLabelText("Failure rate"), { target: { value: "0.25" } });
		fireEvent.click(screen.getByText("Endpoint source & lifecycle"));
		fireEvent.change(screen.getByLabelText("Endpoint source"), { target: { value: "activatable" } });
		fireEvent.change(screen.getByLabelText("Runtime controller"), { target: { value: "sparkrun" } });
		fireEvent.change(screen.getByLabelText("Binding revision"), { target: { value: "binding-sha256" } });
		fireEvent.change(screen.getByLabelText("Recipe reference"), { target: { value: "recipes/qwen.yaml" } });
		fireEvent.change(screen.getByLabelText("Recipe revision"), { target: { value: "recipe-sha256" } });
		fireEvent.change(screen.getByLabelText("Activation timeout"), { target: { value: "15m" } });
		fireEvent.change(screen.getByLabelText("Idle TTL"), { target: { value: "30m" } });
		fireEvent.change(screen.getByLabelText(/^Maximum queued waiters/), { target: { value: "24" } });
		fireEvent.change(screen.getByLabelText(/^Maximum queued body bytes/), { target: { value: "8388608" } });
		const clusters = screen.getByLabelText("Cluster candidates");
		fireEvent.change(clusters, { target: { value: "spark-a, spark-b" } });
		fireEvent.blur(clusters);
		const overrides = screen.getByLabelText(/^Approved recipe overrides/);
		fireEvent.change(overrides, { target: { value: "tensor_parallel=2" } });
		fireEvent.blur(overrides);
    fireEvent.change(screen.getByLabelText("Deployment extra body defaults JSON"), {
      target: { value: '{"reasoning_effort":"high","top_k":40}' },
    });
    fireEvent.blur(screen.getByLabelText("Deployment extra body defaults JSON"));
    fireEvent.click(screen.getByRole("button", { name: "JSON" }));

    const document = JSON.parse(
      (screen.getByLabelText("Gateway configuration JSON") as HTMLTextAreaElement).value,
    );
    expect(document).toMatchObject({ metadata: { preserve: true } });
    expect(document.providers[0]).toMatchObject({
      name: "provider-renamed",
      auth: {
        credential: "env://PROVIDER_KEY_V2",
        future_auth: "preserve-auth",
      },
      default_headers: {
        "X-API-Version": {
          value: "replacement-literal",
          scope: "both",
          future_header: "preserve-header",
        },
        "X-Request-Token": {
          value_from: "k8s://gateway-secrets/provider#token",
        },
        "X-Trace-Policy": {
          value_from: "env://TRACE_POLICY_HEADER",
          scope: "inference",
        },
      },
      extra_body: { service_tier: "flex", metadata: { source: "console" } },
      future_provider: { preserve: true },
    });
    expect(document.deployments[0]).toMatchObject({
      name: "deploy-v2",
      provider: "provider-renamed",
      model: "upstream-v2",
      credential: "env://DEPLOYMENT_KEY",
      native_protocols: ["openai", "anthropic"],
      capabilities: ["tools", "x-existing-feature", "vision"],
      max_concurrency: 96,
      circuit: { failure_rate: 0.25, future_circuit: "preserve-circuit" },
			endpoint_source: {
				type: "activatable",
				controller: "sparkrun",
				revision: "binding-sha256",
				recipe: "recipes/qwen.yaml",
				recipe_revision: "recipe-sha256",
				activation_timeout: "15m",
				idle_ttl: "30m",
				max_queued_waiters: 24,
				max_queued_body_bytes: 8388608,
				cluster_candidates: ["spark-a", "spark-b"],
				overrides: { tensor_parallel: "2" },
			},
      upstream_headers: {
        "X-Model-Profile": { value: deploymentHeader, scope: "health" },
      },
      extra_body: { reasoning_effort: "high", top_k: 40 },
      future_deployment: { preserve: true },
    });
    expect(document.virtual_models[0].pools[0].targets[0].deployment).toBe("deploy-v2");

    fireEvent.click(screen.getByRole("button", { name: "Validate" }));
    expect(await screen.findByText(/Configuration is valid/)).toBeInTheDocument();
    const validationCall = fetchMock.mock.calls.find(
      ([path, init]) => path === "/v1/config/validate" && init?.method === "POST",
    );
    expect(validationCall?.[1]?.body).toContain('"vision"');
    expect(validationCall?.[1]?.body).toContain('"deployment":"deploy-v2"');
  });

  it("leaves malformed provider header shapes untouched for JSON repair", async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(jsonResponse({
      ...bootstrap,
      features: { status: false, config_validate: true },
    }));
    vi.stubGlobal("fetch", fetchMock);

    render(<AdminApp productName="SparkRoute" />);
    fireEvent.click(await screen.findByText("Configuration"));
    fireEvent.click(screen.getByRole("button", { name: "JSON" }));
    const malformed = '{"providers":[{"name":"p","default_headers":[]}],"deployments":[],"virtual_models":[]}';
    fireEvent.change(screen.getByLabelText("Gateway configuration JSON"), {
      target: { value: malformed },
    });
    fireEvent.click(screen.getByRole("button", { name: "Structured" }));
    fireEvent.click(screen.getByRole("button", { name: "Providers & deployments" }));

    expect(screen.getByText("Provider editor unavailable")).toBeInTheDocument();
    expect(screen.getByText(/providers\[0\]\.default_headers must be an object/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "JSON" }));
    expect(
      (screen.getByLabelText("Gateway configuration JSON") as HTMLTextAreaElement).value,
    ).toBe(malformed);
  });

  it("leaves malformed extra-body shapes untouched for JSON repair", async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(jsonResponse({
      ...bootstrap,
      features: { status: false, config_validate: true },
    }));
    vi.stubGlobal("fetch", fetchMock);

    render(<AdminApp productName="SparkRoute" />);
    fireEvent.click(await screen.findByText("Configuration"));
    fireEvent.click(screen.getByRole("button", { name: "JSON" }));
    const malformed = '{"providers":[{"name":"p","extra_body":[]}],"deployments":[],"virtual_models":[]}';
    fireEvent.change(screen.getByLabelText("Gateway configuration JSON"), {
      target: { value: malformed },
    });
    fireEvent.click(screen.getByRole("button", { name: "Structured" }));
    fireEvent.click(screen.getByRole("button", { name: "Providers & deployments" }));

    expect(screen.getByText("Provider editor unavailable")).toBeInTheDocument();
    expect(screen.getByText(/providers\[0\]\.extra_body must be an object/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "JSON" }));
    expect(
      (screen.getByLabelText("Gateway configuration JSON") as HTMLTextAreaElement).value,
    ).toBe(malformed);
  });

  it("does not coerce or discard a document the structured editor cannot represent", async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(jsonResponse({
      ...bootstrap,
      features: { status: false, config_validate: true },
    }));
    vi.stubGlobal("fetch", fetchMock);

    render(<AdminApp productName="SparkRoute" />);
    fireEvent.click(await screen.findByText("Configuration"));
    fireEvent.click(screen.getByRole("button", { name: "JSON" }));
    const malformed = '{"providers":[],"deployments":[],"virtual_models":{"unexpected":true}}';
    fireEvent.change(screen.getByLabelText("Gateway configuration JSON"), {
      target: { value: malformed },
    });
    fireEvent.click(screen.getByRole("button", { name: "Structured" }));

    expect(screen.getByText("Structured editor unavailable")).toBeInTheDocument();
    expect(screen.getByText(/virtual_models must be an array/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "JSON" }));
    expect(
      (screen.getByLabelText("Gateway configuration JSON") as HTMLTextAreaElement).value,
    ).toBe(malformed);
  });

  it("keeps tenant-scoped traffic searches server-forced and drills into attempts", async () => {
    const requestRecord = {
      request_id: "req-1",
      started_at: "2026-08-02T12:00:00Z",
      completed_at: "2026-08-02T12:00:01Z",
      tenant_id: "tenant-a",
      protocol: "openai",
      operation: "chat_completions",
      stream: false,
      requested_model: "public-model",
      virtual_model: "public-model",
      final_provider: "provider-a",
      final_deployment: "deploy-a",
      attempt_count: 1,
      http_status: 200,
      outcome: "success",
      latency: 1_000_000_000,
      usage: { input_tokens: 10, output_tokens: 5, total_tokens: 15, completeness: "complete" },
    };
    const attemptRecord = {
      request_id: "req-1",
      attempt: 1,
      started_at: "2026-08-02T12:00:00Z",
      completed_at: "2026-08-02T12:00:01Z",
      provider: "provider-a",
      deployment: "deploy-a",
      upstream_model: "upstream-a",
      pool_priority: 0,
      http_status: 200,
      outcome: "success",
      retried: false,
      latency: 900_000_000,
      usage: { completeness: "complete" },
    };
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input);
      if (path === "/v1/ui/bootstrap") {
        return jsonResponse({
          ...bootstrap,
          edition: "cluster",
          features: { status: false, ledger_query: true },
          principal: { id: "reader-a", tenant: "tenant-a", roles: ["ledger_read"] },
        });
      }
      if (path === "/v1/ledger/requests?limit=50") {
        return jsonResponse({ records: [requestRecord] });
      }
      if (path === "/v1/ledger/requests/req-1") return jsonResponse(requestRecord);
      if (path === "/v1/ledger/attempts?request_id=req-1&limit=100") {
        return jsonResponse({ records: [attemptRecord] });
      }
      return jsonResponse({ error: { message: `Unexpected request ${path}` } }, 500);
    });
    vi.stubGlobal("fetch", fetchMock);
    window.history.replaceState({}, "", "/admin/traffic");

    render(<AdminApp productName="SparkRoute" />);
    expect(await screen.findByText("Tenant enforced: tenant-a")).toBeInTheDocument();
    expect(screen.queryByLabelText("Tenant")).not.toBeInTheDocument();
    fireEvent.click(await screen.findByRole("button", { name: "req-1" }));

    expect(await screen.findByText("Provider attempts")).toBeInTheDocument();
    expect(screen.getAllByText("provider-a / deploy-a").length).toBeGreaterThan(0);
    expect(screen.getByText(/Prompt and response content is not available/)).toBeInTheDocument();
    const listPath = String(fetchMock.mock.calls[1]?.[0]);
    expect(listPath).not.toContain("tenant_id");
  });

  it("renders tenant-safe server aggregates without estimating from request pages", async () => {
    const aggregate = {
      started_at_or_after: "2026-08-02T00:00:00Z",
      started_at_before: "2026-08-03T00:00:00Z",
      requests: 20,
      successful_requests: 19,
      streaming_requests: 4,
      attempts: 23,
      retried_requests: 3,
      retries: 3,
      latency: {
        average: 700_000_000,
        p50: 500_000_000,
        p95: 1_200_000_000,
        p99: 2_000_000_000,
      },
      tokens: {
        input_tokens: 12_000,
        output_tokens: 4_000,
        total_tokens: 16_000,
        cached_input_tokens: 2_000,
        cache_creation_tokens: 0,
        reasoning_tokens: 200,
        tool_use_prompt_tokens: 0,
        accepted_prediction_tokens: 0,
        rejected_prediction_tokens: 0,
      },
      usage_completeness: { complete: 18, partial: 1, missing: 1 },
      by_outcome: { groups: [{ value: "success", count: 19 }, { value: "upstream_error", count: 1 }], truncated: false },
      by_virtual_model: { groups: [{ value: "public", count: 20 }], truncated: false },
      by_provider: { groups: [{ value: "provider-a", count: 20 }], truncated: false },
      by_deployment: { groups: [{ value: "deploy-a", count: 20 }], truncated: false },
    };
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input);
      if (path === "/v1/ui/bootstrap") {
        return jsonResponse({
          ...bootstrap,
          edition: "cluster",
          features: { status: false, ledger_query: true, ledger_aggregate: true },
          principal: { id: "reader-a", tenant: "tenant-a", roles: ["ledger_read"] },
        });
      }
      if (path.startsWith("/v1/ledger/summary?")) return jsonResponse(aggregate);
      return jsonResponse({ error: { message: `Unexpected request ${path}` } }, 500);
    });
    vi.stubGlobal("fetch", fetchMock);
    window.history.replaceState({}, "", "/admin/traffic");

    render(<AdminApp productName="SparkRoute" />);
    expect(await screen.findByText("95%")).toBeInTheDocument();
    expect(screen.getAllByText("1.20 s")).toHaveLength(2);
    expect(screen.getByText("16,000")).toBeInTheDocument();
    expect(screen.getByText("90% complete usage")).toBeInTheDocument();
    expect(screen.queryByLabelText("Tenant")).not.toBeInTheDocument();
    expect(screen.getByText("provider-a")).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Final provider"), {
      target: { value: "provider-a" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Refresh dashboard" }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));
    const path = String(fetchMock.mock.calls[2]?.[0]);
    expect(path).toContain("provider=provider-a");
    expect(path).not.toContain("tenant_id");
    expect(fetchMock.mock.calls.some(([input]) => String(input).startsWith("/v1/ledger/requests?"))).toBe(false);
  });

  it("downloads a scoped, metadata-filtered trace export without previewing payloads", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input);
      if (path === "/v1/ui/bootstrap") {
        return jsonResponse({
          ...bootstrap,
          edition: "cluster",
          features: { status: false, saved_trace_export: true },
          principal: { id: "trace-a", tenant: "tenant-a", roles: ["trace_read"] },
        });
      }
      if (path.startsWith("/v1/saved-traces/export?")) {
		if (path.includes("format=dataset")) {
		  return new Response("PK-dataset", { headers: { "Content-Type": "application/zip" } });
		}
        return new Response('{"request_id":"req-1"}\n', {
          headers: { "Content-Type": "application/x-ndjson" },
        });
      }
      return jsonResponse({ error: { message: `Unexpected request ${path}` } }, 500);
    });
    vi.stubGlobal("fetch", fetchMock);
    const NativeURL = URL;
    class TestURL extends NativeURL {}
    TestURL.createObjectURL = vi.fn(() => "blob:trace-export");
    TestURL.revokeObjectURL = vi.fn();
    vi.stubGlobal("URL", TestURL);
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => undefined);
    window.history.replaceState({}, "", "/admin/traces");

    render(<AdminApp productName="SparkRoute" />);
    expect(await screen.findByText(/Exports contain caller-visible/)).toBeInTheDocument();
    expect(screen.queryByLabelText("Tenant")).not.toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Operation"), { target: { value: "responses" } });
    fireEvent.change(screen.getByLabelText("Metadata filters, one key=value per line"), {
      target: { value: "sparkroute.task_id=task-a\nrl.run=run-7" },
    });
    fireEvent.click(screen.getByLabelText(/I understand this download/));
    fireEvent.click(screen.getByRole("button", { name: "Export JSONL" }));

    expect(await screen.findByText(/Exported .* bytes/)).toBeInTheDocument();
    const exportPath = String(fetchMock.mock.calls[1]?.[0]);
    expect(exportPath).toContain("operation=responses");
	expect(exportPath).toContain("format=jsonl");
    expect(exportPath).toContain("metadata=sparkroute.task_id%3Dtask-a");
    expect(exportPath).toContain("metadata=rl.run%3Drun-7");
    expect(exportPath).not.toContain("tenant_id");
    expect(TestURL.createObjectURL).toHaveBeenCalledOnce();
    expect(screen.queryByText("req-1")).not.toBeInTheDocument();

	fireEvent.change(screen.getByLabelText("Export format"), { target: { value: "dataset" } });
	fireEvent.change(screen.getByLabelText("Dataset projection"), { target: { value: "mlflow" } });
	fireEvent.click(screen.getByLabelText(/Reject unless capture completeness/));
	fireEvent.click(screen.getByRole("button", { name: "Export dataset" }));
	await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));
	const datasetPath = String(fetchMock.mock.calls[2]?.[0]);
	expect(datasetPath).toContain("format=dataset");
	expect(datasetPath).toContain("projection=mlflow");
	expect(datasetPath).toContain("require_complete=true");
  });

  it("renders shared single-tenant client credential management without tenant controls", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input);
      if (path === "/v1/ui/bootstrap") {
        return jsonResponse({
          ...bootstrap,
          features: { status: false, client_credentials: true },
          principal: { id: "credential-reader", roles: ["credentials_read"] },
        });
      }
      if (path === "/v1/client-credentials?limit=200") {
        return jsonResponse({ credentials: [{
          id: "cc_example",
          name: "training worker",
          principal_id: "worker-a",
          principal_type: "machine",
          roles: ["inference"],
          state: "active",
          created_at: "2026-08-03T12:00:00Z",
          created_by: "operator-a",
          updated_at: "2026-08-03T12:00:00Z",
          updated_by: "operator-a",
        }] });
      }
      if (path === "/v1/client-credentials/audit?limit=100") {
        return jsonResponse({ events: [{
          id: 1,
          credential_id: "cc_example",
          action: "created",
          occurred_at: "2026-08-03T12:00:00Z",
          actor: "operator-a",
        }] });
      }
      return jsonResponse({ error: { message: `Unexpected request ${path}` } }, 500);
    });
    vi.stubGlobal("fetch", fetchMock);
    window.history.replaceState({}, "", "/admin/credentials");

    render(<AdminApp productName="SparkRoute" />);
    expect(await screen.findByText("training worker")).toBeInTheDocument();
		expect(screen.getByText("Single-tenant standalone scope")).toBeInTheDocument();
    expect(screen.queryByLabelText("Tenant")).not.toBeInTheDocument();
    expect(screen.getByText("created")).toBeInTheDocument();
    expect(screen.queryByText("Create client credential")).not.toBeInTheDocument();
  });
});
