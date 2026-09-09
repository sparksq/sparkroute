import type { RoutingStageScenario } from "./types";

export const routingStrategies = [
  { value: "balanced", label: "Balance cost, speed, and model size", group: "Choose by preference", help: "Combines configured prices and model size with observed response time and priority. Model size is a rough preference, not a quality assessment." },
  { value: "lowest_cost", label: "Lowest configured cost", group: "Choose by preference", help: "Picks the lowest combined input and output token prices. Configure prices in Model Deployments; missing prices are treated as zero." },
  { value: "fastest", label: "Fastest observed response", group: "Choose by preference", help: "Uses recent response-time measurements. Until measurements are available, tied models are selected by name. The stateless preview has no live timing data." },
  { value: "smallest", label: "Smallest model", group: "Choose by preference", help: "Picks the smallest configured parameter count. Configure model size in Model Deployments; this does not estimate the difficulty of a prompt." },
  { value: "largest", label: "Largest model", group: "Choose by preference", help: "Picks the largest configured parameter count. Configure model size in Model Deployments; larger models are not guaranteed to perform better on every task." },
  { value: "stage_router", label: "Agent stages · two models", group: "Adapt to agent work", help: "Uses recent tool results to choose between an efficient model and a more capable model. Best for coding agents; ordinary chat uses your default model choice. No additional model call is made." },
  { value: "round_robin", label: "Take turns equally", group: "Distribute traffic", help: "Cycles through eligible models evenly. The preview starts with a fresh counter; live requests advance it." },
  { value: "weighted_round_robin", label: "Take turns by weight", group: "Distribute traffic", help: "Distributes requests using the weights in Shared model settings. A weight of 200 gets twice the share of 100; zero or missing weights act as 1." },
  { value: "random", label: "Random equal split", group: "Distribute traffic", help: "Chooses uniformly among eligible models. Weights do not affect this strategy; use weighted turns for a controlled traffic ratio." },
] as const;

export function routingStrategyLabel(strategy: string) {
  return routingStrategies.find(option => option.value === strategy)?.label ?? strategy;
}

export const stageScenarios: { value: RoutingStageScenario; label: string; help: string }[] = [
  { value: "no_tools", label: "New conversation / ordinary chat", help: "No tool results: uses your default model choice." },
  { value: "exploring", label: "Investigating without edits", help: "Ten messages into the conversation, with recent reads and planning but no writes. A single exploration signal may remain below the switching threshold." },
  { value: "error_recovery", label: "Investigating after a tool failure", help: "Recent investigation plus a failed tool provide two signals toward the capable model." },
  { value: "productive", label: "Making successful edits", help: "Recent writes and edits provide evidence toward the efficient model. The switching threshold still applies." },
  { value: "tests_passed", label: "Edits followed by passing tests", help: "A recent edit plus a test pass and no error select the efficient model if both fit within the configured tool-result window." },
  { value: "critical_error", label: "Critical tool failure", help: "A critical error selects the capable model regardless of sensitivity." },
  { value: "compacted", label: "Context compacted", help: "A context-compaction marker selects the capable model, even when earlier tool results are absent." },
];

export const stageDecisionLabels: Record<string, string> = {
  override: "Critical error or context compaction",
  tests_passed: "Passing tests with recent edits",
  dimensions: "Tool signals exceeded the switching threshold",
  fall_open: "Default choice: insufficient tool evidence",
  tier_unavailable: "Other role used: preferred model was ineligible",
};
