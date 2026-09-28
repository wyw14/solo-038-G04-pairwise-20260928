package schedule

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{
		PlateChange:       20 * time.Minute,
		InkChangePerColor: 5 * time.Minute,
		DryerRampPerC:     30 * time.Second,
		DryerMinPreheat:   5 * time.Minute,
	}
}

func makeOrder(id string, due time.Time, print time.Duration, plate string, colors []string, temp float64) Order {
	return Order{
		ID:              id,
		DueAt:           due,
		PrintDuration:   print,
		Plate:           plate,
		Colors:          colors,
		DryerTargetTemp: temp,
		Status:          StatusPending,
		CreatedAt:       due.Add(-24 * time.Hour),
		UpdatedAt:       due.Add(-24 * time.Hour),
	}
}

func stepByKind(entry PlannedOrder, kind StepKind) (Step, bool) {
	for _, step := range entry.Steps {
		if step.Kind == kind {
			return step, true
		}
	}
	return Step{}, false
}

func entryByID(schedule Schedule, id string) PlannedOrder {
	for _, entry := range schedule.Orders {
		if entry.OrderID == id {
			return entry
		}
	}
	return PlannedOrder{}
}

// The earliest-due order runs first even when it was entered second.
func TestPlanOrdersByDueDate(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	earlier := makeOrder("early", now.Add(3*time.Hour), time.Hour, "P1", []string{"black"}, 60)
	later := makeOrder("later", now.Add(6*time.Hour), time.Hour, "P1", []string{"black"}, 60)
	later.CreatedAt = now.Add(-time.Hour)

	generated := Plan([]Order{later, earlier}, nil, testConfig(), now, now, nil)
	if got := generated.Orders[0].OrderID; got != "early" {
		t.Fatalf("expected earliest-due order first, got %q", got)
	}
	if generated.Orders[0].Sequence != 1 || generated.Orders[1].Sequence != 2 {
		t.Fatalf("expected sequences 1 and 2, got %d and %d", generated.Orders[0].Sequence, generated.Orders[1].Sequence)
	}

	// An identical follow-up needs no plate/ink change and no extra preheat,
	// so it prints immediately when the press comes free.
	first := generated.Orders[0]
	second := generated.Orders[1]
	if second.PressStart != first.End {
		t.Fatalf("expected back-to-back print: first ends %s, second starts %s", first.End, second.PressStart)
	}
	for _, kind := range []StepKind{StepPlateChange, StepInkChange, StepPreheat} {
		if _, ok := stepByKind(second, kind); ok {
			t.Fatalf("did not expect %s step on identical follow-up", kind)
		}
	}
}

// Plate swap, per-color ink changes and parallel dryer preheat become explicit
// non-overlapping (except preheat) windows between two different orders.
func TestChangeoverWindows(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	first := makeOrder("a", now.Add(4*time.Hour), time.Hour, "P1", []string{"cyan", "magenta"}, 60)
	second := makeOrder("b", now.Add(8*time.Hour), time.Hour, "P2", []string{"magenta", "yellow", "black"}, 80)

	generated := Plan([]Order{first, second}, nil, testConfig(), now, now, nil)
	firstEntry := entryByID(generated, "a")
	secondEntry := entryByID(generated, "b")

	plate, ok := stepByKind(secondEntry, StepPlateChange)
	if !ok || plate.End.Sub(plate.Start) != 20*time.Minute {
		t.Fatalf("expected 20m plate change, got %+v ok=%v", plate, ok)
	}
	ink, ok := stepByKind(secondEntry, StepInkChange)
	if !ok {
		t.Fatalf("expected ink change step")
	}
	// cyan removed; black and yellow added => 3 color swaps * 5m.
	if ink.End.Sub(ink.Start) != 15*time.Minute {
		t.Fatalf("expected 15m ink change, got %s", ink.End.Sub(ink.Start))
	}
	if ink.Start != plate.End {
		t.Fatalf("expected ink change to follow plate change serially")
	}

	// Preheat lift is 20C * 30s = 10m and must run in parallel with changeover
	// (changeover is 35m long), ending exactly at press start.
	preheat, ok := stepByKind(secondEntry, StepPreheat)
	if !ok {
		t.Fatalf("expected dryer preheat step")
	}
	if preheat.End != secondEntry.PressStart {
		t.Fatalf("preheat must finish at press start")
	}
	if preheat.End.Sub(preheat.Start) != 10*time.Minute {
		t.Fatalf("expected 10m preheat, got %s", preheat.End.Sub(preheat.Start))
	}
	if preheat.Start.Before(plate.Start) {
		t.Fatalf("preheat must not start before changeover starts")
	}
	if !preheat.Start.Before(ink.End) {
		t.Fatalf("expected preheat to overlap the changeover window")
	}
	if secondEntry.PressStart != firstEntry.End.Add(35*time.Minute) {
		t.Fatalf("press should start right after the 35m serial changeover, got %s", secondEntry.PressStart.Sub(firstEntry.End))
	}
}

// Nothing may be scheduled inside an equipment downtime; the print run and the
// preheat ramp are both pushed past the outage.
func TestDowntimePushesWork(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	order := makeOrder("a", now.Add(8*time.Hour), time.Hour, "P1", []string{"black"}, 60)
	outage := Downtime{ID: "dt1", Start: now.Add(40 * time.Minute), End: now.Add(70 * time.Minute), Reason: "press maintenance"}

	generated := Plan([]Order{order}, []Downtime{outage}, testConfig(), now, now, nil)
	entry := generated.Orders[0]
	for _, step := range entry.Steps {
		if outage.overlaps(step.Start, step.End) {
			t.Fatalf("step %s %s-%s overlaps downtime", step.Kind, step.Start, step.End)
		}
	}
	print, _ := stepByKind(entry, StepPrint)
	if !print.Start.Before(outage.Start) && !print.Start.After(outage.End) {
		t.Fatalf("print start should be clear of the outage, got %s", print.Start)
	}
	if print.Start != now.Add(100*time.Minute) {
		t.Fatalf("expected print at 100m (after outage plus 30m preheat), got %s", print.Start.Sub(now))
	}
}

// Starting an order freezes it: a later replan keeps its actual window and only
// moves the pending queue around it.
func TestRunningOrderFrozenOnReplan(t *testing.T) {
	now := time.Now().UTC()
	planner, err := NewPlanner(filepath.Join(t.TempDir(), "schedule.json"), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	a, _, err := planner.CreateOrder(OrderInput{DueAt: now.Add(2 * time.Hour), PrintDuration: time.Hour, Plate: "P1", Colors: []string{"black"}, DryerTargetTemp: 60}, now)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := planner.CreateOrder(OrderInput{DueAt: now.Add(5 * time.Hour), PrintDuration: time.Hour, Plate: "P2", Colors: []string{"black"}, DryerTargetTemp: 60}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	started, firstPlan, err := planner.StartOrder(a.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	running := entryByID(firstPlan, a.ID)
	if !running.Frozen || running.PressStart != started.StartedAt {
		t.Fatalf("running order must be frozen at its actual start, got %+v", running)
	}

	// New outage lands after the running order; replan must not move it.
	_, secondPlan, err := planner.AddDowntime(started.StartedAt.Add(90*time.Minute), started.StartedAt.Add(2*time.Hour), "dryer fault", now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	runningAgain := entryByID(secondPlan, a.ID)
	if runningAgain.PressStart != running.PressStart || runningAgain.End != running.End {
		t.Fatalf("running window moved: %s-%s -> %s-%s", running.PressStart, running.End, runningAgain.PressStart, runningAgain.End)
	}
	// The pending order must wait for both the frozen order and the outage.
	waiting := entryByID(secondPlan, b.ID)
	if waiting.PressStart.Before(started.ProjectedCompleteAt) {
		t.Fatalf("pending order scheduled before running order finished")
	}
}

// A new downtime must be reflected in the impact explanation of the order whose
// promised completion moved.
func TestImpactExplainsDowntimeDelay(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	planner, err := NewPlanner("", testConfig())
	if err != nil {
		t.Fatal(err)
	}
	order, _, err := planner.CreateOrder(OrderInput{DueAt: now.Add(90 * time.Minute), PrintDuration: time.Hour, Plate: "P1", Colors: []string{"black"}, DryerTargetTemp: 60}, now)
	if err != nil {
		t.Fatal(err)
	}
	before := planner.Schedule()
	beforeEnd := entryByID(before, order.ID).End

	_, after, err := planner.AddDowntime(now.Add(45*time.Minute), now.Add(2*time.Hour), "plate setter broken", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var matched *Impact
	for index := range after.Impacts {
		if after.Impacts[index].OrderID == order.ID {
			matched = &after.Impacts[index]
		}
	}
	if matched == nil {
		t.Fatalf("expected an impact entry for delayed order")
	}
	if !matched.ScheduledEnd.After(beforeEnd) || matched.Delta <= 0 {
		t.Fatalf("expected positive delay, delta=%s", matched.Delta)
	}
	if !containsText(matched.Reasons, "plate setter broken") {
		t.Fatalf("expected downtime reason, got %v", matched.Reasons)
	}
	if matched.OnTime {
		t.Fatalf("order pushed well past its due date should be marked late")
	}
}

// Revising a pending ticket explains the due-date change specifically.
func TestImpactExplainsTicketRevision(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	planner, err := NewPlanner("", testConfig())
	if err != nil {
		t.Fatal(err)
	}
	order, _, err := planner.CreateOrder(OrderInput{DueAt: now.Add(3 * time.Hour), PrintDuration: time.Hour, Plate: "P1", Colors: []string{"black"}, DryerTargetTemp: 60}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, revised, err := planner.ReviseOrder(order.ID, OrderInput{DueAt: now.Add(2 * time.Hour), PrintDuration: time.Hour, Plate: "P1", Colors: []string{"black"}, DryerTargetTemp: 60}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var matched *Impact
	for index := range revised.Impacts {
		if revised.Impacts[index].OrderID == order.ID {
			matched = &revised.Impacts[index]
		}
	}
	if matched == nil || !containsText(matched.Reasons, "due date changed") {
		t.Fatalf("expected due date change reason, got %+v", matched)
	}
}

func TestInvalidOrderRejected(t *testing.T) {
	now := time.Now().UTC()
	planner, err := NewPlanner(filepath.Join(t.TempDir(), "schedule.json"), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	cases := []OrderInput{
		{DueAt: now, PrintDuration: 0, Plate: "P", Colors: []string{"black"}, DryerTargetTemp: 50},
		{DueAt: now, PrintDuration: time.Hour, Plate: "", Colors: []string{"black"}, DryerTargetTemp: 50},
		{DueAt: now, PrintDuration: time.Hour, Plate: "P", Colors: nil, DryerTargetTemp: 50},
		{DueAt: now, PrintDuration: time.Hour, Plate: "P", Colors: []string{"black"}, DryerTargetTemp: -1},
	}
	for index, input := range cases {
		if _, _, err := planner.CreateOrder(input, now); err == nil {
			t.Fatalf("case %d: expected validation error", index)
		}
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedule.json")
	now := time.Now().UTC()
	planner, err := NewPlanner(path, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	order, _, err := planner.CreateOrder(OrderInput{DueAt: now.Add(3 * time.Hour), PrintDuration: 45 * time.Minute, Plate: "P9", Colors: []string{"cyan", "black"}, DryerTargetTemp: 72}, now)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadPlanner(path, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	restored, ok := reloaded.Order(order.ID)
	if !ok {
		t.Fatalf("order missing after reload")
	}
	if restored.PrintDuration != 45*time.Minute || restored.Plate != "P9" || restored.DryerTargetTemp != 72 {
		t.Fatalf("order fields not restored: %+v", restored)
	}
	if reloaded.Schedule().Version == 0 {
		t.Fatalf("expected a planned schedule version after reload")
	}
}

func TestOrderDurationJSONInSeconds(t *testing.T) {
	order := makeOrder("x", time.Now().UTC(), 45*time.Minute+30*time.Second, "P", []string{"black"}, 60)
	data, err := json.Marshal(order)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["print_duration_seconds"] != 2730.0 {
		t.Fatalf("expected 2730 seconds, got %v", decoded["print_duration_seconds"])
	}
	var roundTrip Order
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.PrintDuration != order.PrintDuration {
		t.Fatalf("duration round trip mismatch: %s vs %s", roundTrip.PrintDuration, order.PrintDuration)
	}
}

func containsText(values []string, fragment string) bool {
	for _, value := range values {
		if strings.Contains(value, fragment) {
			return true
		}
	}
	return false
}
