<!--
SPDX-FileCopyrightText: 2026 Scitrera LLC
SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
SPDX-License-Identifier: AGPL-3.0-only
-->

# Configuring model routing

A selector such as `auto` is a model name clients can request. It chooses between
your configured virtual models. Calling a virtual model directly bypasses that
selector. Model routing is optional.

Choose a strategy by its purpose:

- **Balance cost, speed, and model size** combines prices, observed latency, size,
  and priority. Size is a rough preference, not a measured quality score.
- **Lowest configured cost**, **Smallest model**, and **Largest model** use
  metadata configured in Model Deployments. Missing numeric metadata is zero;
  supply it before relying on these strategies.
- **Fastest observed response** uses live latency observations. A stateless
  preview cannot reproduce those observations.
- **Take turns equally**, **Take turns by weight**, and **Random equal split**
  distribute traffic. Only weighted turns use the shared weights. Zero or omitted
  weights act as one for that strategy.
- **Agent stages** uses recent tool results to choose between two model roles,
  without an additional classifier call.

## Agent stages

Assign the **capable model** for difficult work and recovery, and the
**efficient model** for routine work. Roles must name two different virtual
models. The editor does not infer capability from names or parameter counts.
Disabled models cannot be selected; capability filtering can also make a role
ineligible, in which case the router tries the other role.

**Default model choice** controls new conversations and inconclusive signals:
Cost first uses the efficient model; Quality first uses the capable model.
Ordinary chat without tool results uses this default.

**Switching sensitivity** controls evidence required to leave the default:

| Choice | Threshold | Behavior |
| --- | --- | --- |
| Earlier switching | 0.3 | One clear signal can move work away from the default |
| More evidence | 0.5 | Default; normally needs corroborating evidence |
| Strong evidence | 0.7 | Fewer signal-driven switches |

These are convenience values for the existing `confidence_threshold` field,
not separate saved configuration presets. Custom numeric values remain editable
under **Advanced stage tuning**. The threshold measures heuristic signal
strength, not probability of task success. Exact equality retains the default.

Successful edits alone produce signal strength about 0.462, so Quality first with
threshold 0.5 or higher normally remains on the capable model until stronger
evidence appears. A recent edit followed by passing tests with no error selects
the efficient model. Critical errors and explicit context compaction select the
capable model regardless of sensitivity.

The **Recent tool-result window** counts completed tool results, not chat
messages. It defaults to three. At window one, a test result may exclude the
preceding edit, so the settled-work rule cannot see both events.

## Preview and advanced settings

**Simulate routing** evaluates the current draft through the same native scorer
and capability filter as inference. It does not call or start models. Agent-stage
policies offer sample activity scenarios, including failed tools, successful
edits, tests, and compaction. These are fixed examples, not a replay of live
traffic. The result explains the selected role and decision source. Changing the
draft or preview inputs clears outdated results.

Both `/v1/config/simulate-routing` and the managed-set simulation endpoint accept
optional `stage_scenario`: `no_tools`, `exploring`, `error_recovery`, `productive`,
`tests_passed`, `critical_error`, or `compacted`. Omission means no tool history.
Unknown scenarios are rejected. Keyword rules still use `routing_text` and may
redirect to a different selector before its strategy is evaluated.

**Shared model settings** controls availability, weight, and priority across
selectors, with metadata inherited from Model Deployments and discovery. Weights
and priorities do not affect the agent-stage choice. **Keyword overrides** are
also shared; the first matching rule wins. Use the arrows to set their order.
Named routing kwargs such as `auto:private` remain available in JSON mode and are
preserved by the structured editor.
