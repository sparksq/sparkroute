// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.

package modelrouter

import (
	"encoding/json"
	"math"
	"regexp"
	"strings"
)

const (
	maxStageEvents        = 256
	maxStageOutputBytes   = 64 << 10
	stageStallTurnDepth   = 8
	stageHardSeverity     = 0.7
	stageCriticalSeverity = 1.0
)

type StageToolCategory string

const (
	StageToolWrite StageToolCategory = "write"
	StageToolEdit  StageToolCategory = "edit"
	StageToolRead  StageToolCategory = "read"
	StageToolPlan  StageToolCategory = "plan"
	StageToolOther StageToolCategory = "other"
)

// StageEvent is a content-free projection of one completed tool operation.
// Tool names, arguments, output, resource identifiers, and errors are never
// retained in this routing representation.
type StageEvent struct {
	Category    StageToolCategory `json:"category"`
	Severity    float64           `json:"severity,omitempty"`
	TestsPassed bool              `json:"tests_passed,omitempty"`
}

type StageHistory struct {
	Events    []StageEvent `json:"events,omitempty"`
	TurnDepth int          `json:"turn_depth,omitempty"`
	Compacted bool         `json:"compacted,omitempty"`
}

type StageDimensions struct {
	Severity            float64 `json:"severity"`
	Spinning            float64 `json:"spinning"`
	Exploring           float64 `json:"exploring"`
	ProductionIntensity float64 `json:"production_intensity"`
}

type StageRouteTrace struct {
	Tier           string          `json:"tier"`
	DecisionSource string          `json:"decision_source"`
	Score          float64         `json:"score"`
	Confidence     float64         `json:"confidence"`
	Dimensions     StageDimensions `json:"dimensions"`
}

func cloneStageRouterPolicy(policy *StageRouterPolicy) *StageRouterPolicy {
	if policy == nil {
		return nil
	}
	clone := *policy
	if policy.ConfidenceThreshold != nil {
		threshold := *policy.ConfidenceThreshold
		clone.ConfidenceThreshold = &threshold
	}
	return &clone
}

func chooseStageRoute(plan RoutePlan) (string, *StageRouteTrace) {
	if plan.StageRouter == nil {
		return "", nil
	}
	policy := *plan.StageRouter
	dimensions, testsPassed, hasSignals := stageDimensions(plan.StageHistory, policy.effectiveWindow())
	score := math.Tanh(5 * 0.10 * (dimensions.Severity/stageHardSeverity + dimensions.Spinning + dimensions.Exploring - dimensions.ProductionIntensity))
	confidence := math.Abs(score)
	tier := "efficient"
	source := "dimensions"
	if plan.StageHistory.Compacted || dimensions.Severity >= stageCriticalSeverity {
		tier, source, score, confidence = "capable", "override", 0, 1
	} else if !hasSignals {
		tier = stageDefaultTier(policy.effectivePicker())
		source = "fall_open"
		score, confidence = 0, 0
	} else if testsPassed && dimensions.ProductionIntensity > 0 && dimensions.Severity == 0 {
		tier, source, score, confidence = "efficient", "tests_passed", 0, 1
	} else if confidence <= policy.effectiveThreshold() {
		// The ambiguous band includes its boundary, including a neutral score
		// at threshold zero. Such turns retain the configured default tier.
		tier = stageDefaultTier(policy.effectivePicker())
		source = "fall_open"
	} else if score > 0 {
		tier = "capable"
	}
	target := policy.EfficientModel
	if tier == "capable" {
		target = policy.CapableModel
	}
	if !containsString(plan.Eligible, target) {
		fallback := policy.CapableModel
		fallbackTier := "capable"
		if tier == "capable" {
			fallback, fallbackTier = policy.EfficientModel, "efficient"
		}
		if !containsString(plan.Eligible, fallback) {
			return "", nil
		}
		target, tier, source = fallback, fallbackTier, "tier_unavailable"
	}
	return target, &StageRouteTrace{
		Tier: tier, DecisionSource: source, Score: score, Confidence: confidence, Dimensions: dimensions,
	}
}

func stageDefaultTier(picker StagePicker) string {
	if picker == StagePickerCapableFirst {
		return "capable"
	}
	return "efficient"
}

type stagePending struct {
	byID  map[string]StageToolCategory
	order []stagePendingCall
}

type stagePendingCall struct {
	id       string
	category StageToolCategory
}

func extractStageHistory(body map[string]any) StageHistory {
	history := StageHistory{}
	pending := stagePending{byID: map[string]StageToolCategory{}}
	if messages, ok := body["messages"].([]any); ok {
		history.TurnDepth = len(messages)
		for _, raw := range messages {
			message, _ := raw.(map[string]any)
			observeStageMessage(message, &pending, &history)
		}
	}
	if input, ok := body["input"].([]any); ok {
		if len(input) > history.TurnDepth {
			history.TurnDepth = len(input)
		}
		for _, raw := range input {
			item, _ := raw.(map[string]any)
			observeStageItem(item, &pending, &history)
		}
	}
	if len(history.Events) > maxStageEvents {
		history.Events = append([]StageEvent(nil), history.Events[len(history.Events)-maxStageEvents:]...)
	}
	return history
}

func observeStageMessage(message map[string]any, pending *stagePending, history *StageHistory) {
	if message == nil {
		return
	}
	if compacted, _ := message["compacted"].(bool); compacted {
		history.Compacted = true
	}
	if calls, ok := message["tool_calls"].([]any); ok {
		for _, raw := range calls {
			call, _ := raw.(map[string]any)
			function, _ := call["function"].(map[string]any)
			rememberStageCall(stringValue(call["id"]), stringValue(function["name"]), function["arguments"], pending)
		}
	}
	if call, ok := message["function_call"].(map[string]any); ok {
		rememberStageCall("", stringValue(call["name"]), call["arguments"], pending)
	}
	role := strings.ToLower(stringValue(message["role"]))
	if role == "tool" || role == "function" {
		category := consumeStageCall(stringValue(message["tool_call_id"]), pending)
		appendStageResult(category, message["content"], boolValue(message["is_error"]), history)
	}
	if blocks, ok := message["content"].([]any); ok {
		for _, raw := range blocks {
			block, _ := raw.(map[string]any)
			observeStageItem(block, pending, history)
		}
	}
}

func observeStageItem(item map[string]any, pending *stagePending, history *StageHistory) {
	if item == nil {
		return
	}
	typ := strings.ToLower(stringValue(item["type"]))
	switch typ {
	case "compaction", "compaction_summary", "summary":
		history.Compacted = true
	case "function_call", "tool_call", "tool_use", "computer_call", "custom_tool_call":
		id := firstString(item, "call_id", "tool_call_id", "id")
		name := firstString(item, "name", "tool_name")
		arguments := item["arguments"]
		if arguments == nil {
			arguments = item["input"]
		}
		rememberStageCall(id, name, arguments, pending)
	case "function_call_output", "tool_result", "computer_call_output", "custom_tool_call_output":
		id := firstString(item, "call_id", "tool_call_id", "tool_use_id", "id")
		output := item["output"]
		if output == nil {
			output = item["content"]
		}
		isError := boolValue(item["is_error"]) || strings.EqualFold(stringValue(item["status"]), "failed")
		appendStageResult(consumeStageCall(id, pending), output, isError, history)
	}
	if blocks, ok := item["content"].([]any); ok && typ != "tool_result" {
		for _, raw := range blocks {
			block, _ := raw.(map[string]any)
			observeStageItem(block, pending, history)
		}
	}
}

func rememberStageCall(id, name string, arguments any, pending *stagePending) {
	category := classifyStageTool(name, arguments)
	if id != "" {
		pending.byID[id] = category
	}
	pending.order = append(pending.order, stagePendingCall{id: id, category: category})
}

func consumeStageCall(id string, pending *stagePending) StageToolCategory {
	if id != "" {
		if category, ok := pending.byID[id]; ok {
			delete(pending.byID, id)
			for index, call := range pending.order {
				if call.id == id {
					pending.order = append(pending.order[:index], pending.order[index+1:]...)
					break
				}
			}
			return category
		}
	}
	if len(pending.order) == 0 {
		return StageToolOther
	}
	call := pending.order[0]
	pending.order = pending.order[1:]
	if call.id != "" {
		delete(pending.byID, call.id)
	}
	return call.category
}

func appendStageResult(category StageToolCategory, output any, explicitError bool, history *StageHistory) {
	text := strings.ToLower(stageOutputText(output, maxStageOutputBytes))
	severity := stageErrorSeverity(text)
	if explicitError && severity < stageHardSeverity {
		severity = stageHardSeverity
	}
	history.Events = append(history.Events, StageEvent{
		Category: category, Severity: severity, TestsPassed: stageTestsPassed(text, severity),
	})
}

var nonzeroTestFailures = regexp.MustCompile(`(?:^|[^0-9])[1-9][0-9]*\s+(?:failed|failures?|errors?)(?:[^a-z]|$)`)
var stagePythonCommand = regexp.MustCompile(`(?:^|[;&|\n])\s*(?:[a-z0-9_./-]*/)?python[0-9.]*\s`)

func stageErrorSeverity(text string) float64 {
	for _, pattern := range []string{"out of memory", "memoryerror", "cannot allocate memory", "connection refused", "econnrefused"} {
		if strings.Contains(text, pattern) {
			return stageCriticalSeverity
		}
	}
	for _, pattern := range []string{
		"traceback (most recent call last)", "modulenotfounderror:", "importerror:",
		"assertionerror", "valueerror:", "syntaxerror:", "timed out", "timeouterror",
		"deadline exceeded", "filenotfounderror:", "no such file or directory",
		"command not found", "panic:", "fatal error:",
	} {
		if strings.Contains(text, pattern) {
			return stageHardSeverity
		}
	}
	for _, pattern := range []string{"exit code 1", "exit code 2", "exit status 1", "returned non-zero", "exited with code"} {
		if strings.Contains(text, pattern) {
			return 0.3
		}
	}
	if nonzeroTestFailures.MatchString(text) {
		return stageHardSeverity
	}
	return 0
}

func stageTestsPassed(text string, severity float64) bool {
	if severity > 0 {
		return false
	}
	for _, phrase := range []string{" tests passed", "test passed", "passed in", "test result: ok", "tests pass", "\nok\t", "\nok "} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func classifyStageTool(name string, arguments any) StageToolCategory {
	lower := strings.ToLower(strings.TrimSpace(name))
	if stringIn(lower, "write", "create_file", "new_file", "write_file") {
		return StageToolWrite
	}
	if stringIn(lower, "edit", "multiedit", "notebookedit", "str_replace", "str_replace_based_edit_tool", "text_editor", "patch", "apply_patch") {
		return StageToolEdit
	}
	if stringIn(lower, "read", "view", "read_file", "search_files", "view_image") {
		return StageToolRead
	}
	if stringIn(lower, "todowrite", "todo_write", "todo", "update_plan") {
		return StageToolPlan
	}
	if stringIn(lower, "bash", "shell_command", "shell", "local_shell_call", "terminal", "exec_command") {
		command := strings.ToLower(stageOutputText(arguments, maxStageOutputBytes))
		if containsAny(command, "sed -i", "--in-place", "patch ", "apply_patch", "perl -i", "perl -pi") {
			return StageToolEdit
		}
		if containsAny(command, "cat >", "cat >>", "echo >", "echo >>", "tee ", "printf >", "printf >>", "<<eof", "<<'eof'") {
			return StageToolWrite
		}
		if stagePythonCommand.MatchString(command) && containsAny(command, ".write_text(", ".write_bytes(", ".write(") {
			return StageToolWrite
		}
		if containsAny(command, "cat ", "grep ", "rg ", "ls ", "find ", "head ", "tail ", "diff ", "stat ", "less ", "more ") {
			return StageToolRead
		}
	}
	return StageToolOther
}

func stageDimensions(history StageHistory, window int) (StageDimensions, bool, bool) {
	if len(history.Events) == 0 {
		return StageDimensions{}, false, false
	}
	if window <= 0 {
		window = 3
	}
	start := len(history.Events) - window
	if start < 0 {
		start = 0
	}
	recent := history.Events[start:]
	var writes, edits, reads, plans int
	severity := 0.0
	testsPassed := false
	for _, event := range recent {
		severity = math.Max(severity, event.Severity)
		testsPassed = testsPassed || event.TestsPassed
		switch event.Category {
		case StageToolWrite:
			writes++
		case StageToolEdit:
			edits++
		case StageToolRead:
			reads++
		case StageToolPlan:
			plans++
		}
	}
	production := writes + edits
	known := production + reads + plans
	spinning := history.TurnDepth >= stageStallTurnDepth && production == 0 && reads+plans == 0
	exploring := history.TurnDepth >= stageStallTurnDepth && production == 0 && reads+plans > 0
	intensity := 0.0
	if known > 0 {
		intensity = float64(production) / float64(known)
	}
	return StageDimensions{
		Severity: severity, Spinning: boolFloat(spinning), Exploring: boolFloat(exploring), ProductionIntensity: intensity,
	}, testsPassed, true
}

func stageOutputText(value any, limit int) string {
	var builder strings.Builder
	var walk func(any)
	walk = func(current any) {
		if builder.Len() >= limit {
			return
		}
		switch typed := current.(type) {
		case string:
			remaining := limit - builder.Len()
			if len(typed) > remaining {
				typed = typed[:remaining]
			}
			builder.WriteString(typed)
			builder.WriteByte('\n')
		case []any:
			for _, item := range typed {
				walk(item)
			}
		case map[string]any:
			for _, key := range []string{"command", "cmd", "text", "content", "output", "error", "message"} {
				if child, ok := typed[key]; ok {
					walk(child)
				}
			}
		case json.RawMessage:
			var decoded any
			if json.Unmarshal(typed, &decoded) == nil {
				walk(decoded)
			}
		}
	}
	if text, ok := value.(string); ok && strings.HasPrefix(strings.TrimSpace(text), "{") {
		var decoded any
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.UseNumber()
		if decoder.Decode(&decoded) == nil {
			walk(decoded)
			return builder.String()
		}
	}
	walk(value)
	return builder.String()
}

func firstString(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringValue(object[key]); value != "" {
			return value
		}
	}
	return ""
}

func stringValue(value any) string { valueString, _ := value.(string); return valueString }
func boolValue(value any) bool     { valueBool, _ := value.(bool); return valueBool }
func boolFloat(value bool) float64 {
	if value {
		return 1
	}
	return 0
}
func stringIn(value string, options ...string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}
func containsAny(value string, options ...string) bool {
	for _, option := range options {
		if strings.Contains(value, option) {
			return true
		}
	}
	return false
}
