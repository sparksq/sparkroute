// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package routing

import (
	"fmt"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
)

type loadMap map[string]int

func (l loadMap) Outstanding(deployment string) int {
	return l[deployment]
}

// identicalPoolDocument is one virtual model whose single pool holds n
// interchangeable deployments (replicas of the same upstream model).
func identicalPoolDocument(n int, mode config.SelectionMode) config.Document {
	document := routingDocument()
	document.Deployments = nil
	targets := make([]config.WeightedTarget, 0, n)
	for index := 0; index < n; index++ {
		name := fmt.Sprintf("replica-%d", index)
		document.Deployments = append(document.Deployments,
			config.Deployment{Name: name, Provider: "provider", Model: "same-model"})
		targets = append(targets, config.WeightedTarget{Deployment: name, Weight: 1})
	}
	document.VirtualModels[0].Selection = config.SelectionPolicy{Mode: mode}
	document.VirtualModels[0].Pools = []config.RoutingPool{{Targets: targets}}
	document.VirtualModels[0].Limits.MaxAttempts = n
	return document
}

func candidateNames(plan Plan) []string {
	names := make([]string, 0, len(plan.Candidates))
	for _, candidate := range plan.Candidates {
		names = append(names, candidate.Deployment.Name)
	}
	return names
}

func TestBuildPlanForLeastOutstandingOrdersByLoad(t *testing.T) {
	t.Parallel()

	snapshot, err := Compile(identicalPoolDocument(4, config.SelectionLeastOutstanding))
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	plan, err := snapshot.BuildPlanFor("default", PlanOptions{
		Load:   loadMap{"replica-0": 5, "replica-1": 0, "replica-2": 3, "replica-3": 1},
		Picker: &sequencePicker{values: []int64{0, 0, 0, 0}},
	})
	if err != nil {
		t.Fatalf("BuildPlanFor() error = %v", err)
	}
	want := []string{"replica-1", "replica-3", "replica-2", "replica-0"}
	if got := candidateNames(plan); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("order = %v, want %v (least loaded first, fallbacks by load)", got, want)
	}
}

func TestBuildPlanForLeastOutstandingBreaksTiesWithPicker(t *testing.T) {
	t.Parallel()

	snapshot, err := Compile(identicalPoolDocument(3, config.SelectionLeastOutstanding))
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	picker := &recordingPicker{values: []int64{1, 0, 0}}
	plan, err := snapshot.BuildPlanFor("default", PlanOptions{
		Load:   loadMap{"replica-0": 2, "replica-1": 0, "replica-2": 0},
		Picker: picker,
	})
	if err != nil {
		t.Fatalf("BuildPlanFor() error = %v", err)
	}
	if got := candidateNames(plan)[0]; got != "replica-2" {
		t.Fatalf("first = %s, want replica-2 (picker value 1 among the two idle replicas)", got)
	}
	// The first draw is only among the two idle replicas, never the busy one.
	if picker.totals[0] != 2 {
		t.Fatalf("first draw total weight = %d, want 2", picker.totals[0])
	}
}

func TestBuildPlanForLeastOutstandingWeighsLoadByTargetWeight(t *testing.T) {
	t.Parallel()

	document := identicalPoolDocument(2, config.SelectionLeastOutstanding)
	document.VirtualModels[0].Pools[0].Targets[0].Weight = 4 // replica-0 has 4x capacity
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	// 3 in flight on weight 4 (0.75/unit) is lighter than 1 on weight 1.
	plan, err := snapshot.BuildPlanFor("default", PlanOptions{
		Load:   loadMap{"replica-0": 3, "replica-1": 1},
		Picker: &sequencePicker{values: []int64{0, 0}},
	})
	if err != nil {
		t.Fatalf("BuildPlanFor() error = %v", err)
	}
	if got := candidateNames(plan)[0]; got != "replica-0" {
		t.Fatalf("first = %s, want replica-0", got)
	}
}

func TestBuildPlanForLeastOutstandingSpreadsConcurrentRequests(t *testing.T) {
	t.Parallel()

	snapshot, err := Compile(identicalPoolDocument(4, config.SelectionLeastOutstanding))
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	// Simulate 8 requests arriving while none has finished: each plan sees
	// the admissions of the ones before it.
	load := loadMap{}
	for request := 0; request < 8; request++ {
		plan, buildErr := snapshot.BuildPlanFor("default", PlanOptions{
			Load:   load,
			Picker: CryptoPicker{},
		})
		if buildErr != nil {
			t.Fatalf("BuildPlanFor(%d) error = %v", request, buildErr)
		}
		load[plan.Candidates[0].Deployment.Name]++
	}
	for index := 0; index < 4; index++ {
		if got := load[fmt.Sprintf("replica-%d", index)]; got != 2 {
			t.Fatalf("in flight per replica = %v, want 2 each", load)
		}
	}
}

func TestBuildPlanForLeastOutstandingWithoutLoadIsWeightedRandom(t *testing.T) {
	t.Parallel()

	snapshot, err := Compile(identicalPoolDocument(3, config.SelectionLeastOutstanding))
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	picker := &recordingPicker{values: []int64{2, 0, 0}}
	plan, err := snapshot.BuildPlanFor("default", PlanOptions{Picker: picker})
	if err != nil {
		t.Fatalf("BuildPlanFor() error = %v", err)
	}
	if got := candidateNames(plan)[0]; got != "replica-2" || picker.totals[0] != 3 {
		t.Fatalf("first = %s over total %d, want replica-2 over 3", got, picker.totals[0])
	}
}

func TestBuildPlanForLeastOutstandingRespectsEligibilityAndPriority(t *testing.T) {
	t.Parallel()

	document := identicalPoolDocument(3, config.SelectionLeastOutstanding)
	document.Deployments = append(document.Deployments,
		config.Deployment{Name: "backup", Provider: "provider", Model: "same-model"})
	document.VirtualModels[0].Pools = append(document.VirtualModels[0].Pools, config.RoutingPool{
		Priority: 10, Targets: []config.WeightedTarget{{Deployment: "backup", Weight: 1}},
	})
	document.VirtualModels[0].Limits.MaxAttempts = 4
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	plan, err := snapshot.BuildPlanFor("default", PlanOptions{
		// replica-1 is idle but ejected; the idle lower-priority backup must
		// still come after every eligible priority-0 replica.
		Eligibility: eligibilityMap{"replica-0": true, "replica-2": true, "backup": true},
		Load:        loadMap{"replica-0": 4, "replica-1": 0, "replica-2": 2, "backup": 0},
		Picker:      &sequencePicker{values: []int64{0, 0, 0}},
	})
	if err != nil {
		t.Fatalf("BuildPlanFor() error = %v", err)
	}
	want := []string{"replica-2", "replica-0", "backup"}
	if got := candidateNames(plan); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestLowestLoadIndexes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		weights, loads []int
		least          bool
		want           []int
	}{
		{[]int{1, 1, 1}, []int{2, 0, 0}, true, []int{1, 2}},
		{[]int{1, 1, 1}, []int{2, 0, 0}, false, []int{0, 1, 2}},
		{[]int{2, 1}, []int{2, 1}, true, []int{0, 1}},
		{[]int{1, 1}, []int{0}, true, []int{0, 1}}, // mismatched loads: no narrowing
		{nil, nil, true, []int{}},
	}
	for _, tc := range cases {
		if got := lowestLoadIndexes(tc.weights, tc.loads, tc.least); fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Fatalf("lowestLoadIndexes(%v, %v, %v) = %v, want %v", tc.weights, tc.loads, tc.least, got, tc.want)
		}
	}
}

type recordingPicker struct {
	values []int64
	totals []int64
}

func (p *recordingPicker) Pick(totalWeight int64) (int64, error) {
	p.totals = append(p.totals, totalWeight)
	if len(p.values) == 0 {
		return 0, nil
	}
	value := p.values[0]
	p.values = p.values[1:]
	return value, nil
}
