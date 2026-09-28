package schedule

import (
	"strings"
	"testing"
	"time"
)

func testConfig() ChangeoverConfig {
	return ChangeoverConfig{
		PlateChange:        20 * time.Minute,
		InkChange:          10 * time.Minute,
		InkPerColor:        5 * time.Minute,
		ReheatFactor:       0.5,
		DryerTempTolerance: 10,
	}
}

func mustOrder(t *testing.T, name string, due time.Time, plate string, colors []string, preheat time.Duration, setup, printing, finishing time.Duration, now time.Time) Order {
	t.Helper()
	order, err := NewOrder(name, due, plate, colors, DryerConfig{Profile: "production", TargetTemp: 90, Preheat: preheat}, ProcessDurations{Setup: setup, Printing: printing, Finishing: finishing}, 0, now)
	if err != nil {
		t.Fatalf("NewOrder(%s): %v", name, err)
	}
	return order
}

func TestBuildPlanColdStartWindows(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	order := mustOrder(t, "A", now.Add(6*time.Hour), "plate-a", []string{"cyan"}, 40*time.Minute, 10*time.Minute, time.Hour, 5*time.Minute, now)

	plan, err := BuildPlan([]Order{order}, nil, nil, now, now, testConfig())
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.Orders) != 1 {
		t.Fatalf("expected 1 scheduled order, got %d", len(plan.Orders))
	}
	entry := plan.Orders[0]
	changeover := entry.Changeover
	if changeover == nil || !changeover.ColdStart {
		t.Fatal("expected a cold start changeover")
	}
	// 冷启动换线 = max(换版20m + 换墨15m, 预热40m) = 40m
	if got := changeover.TotalDuration(); got != 40*time.Minute {
		t.Fatalf("cold start changeover = %s, want 40m", got)
	}
	if !entry.SetupStart.Equal(now.Add(40 * time.Minute)) {
		t.Fatalf("setup start = %s, want %s", entry.SetupStart, now.Add(40*time.Minute))
	}
	// 预热窗口必须在正式印刷前恰好结束，且与人工换线并行。
	if changeover.PreheatWindow == nil || !changeover.PreheatWindow.End.Equal(entry.PrintStart) {
		t.Fatalf("preheat window must finish exactly at print start, got %+v", changeover.PreheatWindow)
	}
	if !entry.OnTime {
		t.Fatal("order should be on time")
	}
}

func TestBuildPlanSamePlateSameColorSkipsChangeover(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	first := mustOrder(t, "A", now.Add(12*time.Hour), "plate-a", []string{"cyan", "black"}, 30*time.Minute, 10*time.Minute, time.Hour, 0, now)
	second := mustOrder(t, "B", now.Add(24*time.Hour), "plate-a", []string{"cyan", "black"}, 30*time.Minute, 10*time.Minute, time.Hour, 0, now.Add(time.Second))

	plan, err := BuildPlan([]Order{first, second}, nil, nil, now, now, testConfig())
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.Orders) != 2 {
		t.Fatalf("expected 2 orders, got %d", len(plan.Orders))
	}
	change := plan.Orders[1].Changeover
	if change.ColdStart {
		t.Fatal("second order must not be a cold start")
	}
	if change.PlateChanged || change.InkChanged {
		t.Fatalf("identical plate/colors should not require changeover, got plate=%v ink=%v", change.PlateChanged, change.InkChanged)
	}
	if change.plateDuration != 0 || change.inkDuration != 0 {
		t.Fatalf("identical plate/colors should skip manual changeover, got plate %s ink %s", change.plateDuration, change.inkDuration)
	}
	// 总换线时长只剩预热：max(0, 15m) = 15m。
	if change.TotalDuration() != 15*time.Minute {
		t.Fatalf("changeover total = %s, want 15m preheat only", change.TotalDuration())
	}
	// 同版同色但仍需烘干预热；温差 0 <= 容差，应复用余温折减为 15m。
	if !change.DryerReusedWasteHeat || change.effectivePreheat != 15*time.Minute {
		t.Fatalf("expected 15m reheated preheat, got %+v", change)
	}
	// 不换版、不换墨，只余温预热 15m：B 在 A 完工后 15 分钟上机。
	if !plan.Orders[1].SetupStart.Equal(plan.Orders[0].FinishAt.Add(15 * time.Minute)) {
		t.Fatalf("second order setup = %s, want 15m after first finish %s", plan.Orders[1].SetupStart, plan.Orders[0].FinishAt)
	}
}

func TestBuildPlanPlateInkAndColdDryer(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	first := mustOrder(t, "A", now.Add(24*time.Hour), "plate-a", []string{"cyan"}, 30*time.Minute, 0, time.Hour, 0, now)
	second := mustOrder(t, "B", now.Add(48*time.Hour), "plate-b", []string{"magenta"}, 30*time.Minute, 0, time.Hour, 0, now.Add(time.Second))
	// 温差超过容差：把第二单烘干温度改掉。
	second.Dryer.TargetTemp = 130

	plan, err := BuildPlan([]Order{first, second}, nil, nil, now, now, testConfig())
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	change := plan.Orders[1].Changeover
	if !change.PlateChanged || !change.InkChanged {
		t.Fatalf("expected plate and ink change, got %+v", change)
	}
	if len(change.ColorsAdded) != 1 || change.ColorsAdded[0] != "magenta" || len(change.ColorsRemoved) != 1 || change.ColorsRemoved[0] != "cyan" {
		t.Fatalf("color diff wrong: %+v", change)
	}
	// 换版20 + 换墨（换 2 个色组：1 基础 10m + 1 增量 5m）= 35m；温差大不能复用余温，预热 30m 与人工并行，总占用 35m。
	if change.DryerReusedWasteHeat {
		t.Fatal("130 vs 90 must not reuse waste heat")
	}
	if change.TotalDuration() != 35*time.Minute {
		t.Fatalf("changeover = %s, want 35m", change.TotalDuration())
	}
	if change.PlateWindow == nil || change.InkWindow == nil {
		t.Fatal("plate and ink windows must both be present")
	}
}

func TestBuildPlanEDDSortsByDueDate(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	later := mustOrder(t, "later", now.Add(48*time.Hour), "p", []string{"cyan"}, time.Minute, 0, time.Hour, 0, now)
	sooner := mustOrder(t, "sooner", now.Add(24*time.Hour), "p", []string{"cyan"}, time.Minute, 0, time.Hour, 0, now.Add(time.Second))

	plan, err := BuildPlan([]Order{later, sooner}, nil, nil, now, now, testConfig())
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if plan.Orders[0].Name != "sooner" || plan.Orders[1].Name != "later" {
		t.Fatalf("EDD order = %s, %s; want sooner first", plan.Orders[0].Name, plan.Orders[1].Name)
	}
}

func TestBuildPlanReportsLateness(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	order := mustOrder(t, "tight", now.Add(30*time.Minute), "p", []string{"cyan"}, 40*time.Minute, 0, time.Hour, 0, now)

	plan, err := BuildPlan([]Order{order}, nil, nil, now, now, testConfig())
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if plan.Orders[0].OnTime {
		t.Fatal("order finishing at 09:40 due 08:30 must be late")
	}
	if len(plan.LateDeliveries) != 1 {
		t.Fatalf("expected 1 late delivery, got %d", len(plan.LateDeliveries))
	}
	if plan.LateDeliveries[0].shift != 70*time.Minute {
		t.Fatalf("lateness = %s, want 70m", plan.LateDeliveries[0].shift)
	}
	if len(plan.LateDeliveries[0].Causes) == 0 {
		t.Fatal("lateness explanation must give causes")
	}
}

func TestBuildPlanMovesProductionAroundDowntime(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	// 冷启动换线占用 8:00-8:40（预热40m），生产 8:40-9:40，停机 9:00-10:00 必须避让。
	order := mustOrder(t, "A", now.Add(24*time.Hour), "p", []string{"cyan"}, 40*time.Minute, 0, time.Hour, 0, now)
	downtime, err := NewDowntime("blade replacement", now.Add(time.Hour), now.Add(2*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}

	without, err := BuildPlan([]Order{order}, nil, nil, now, now, testConfig())
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	// 以无停机版排程作为上一版，新增停机后重算，交期影响应能被解释。
	plan, err := BuildPlan([]Order{order}, []Downtime{downtime}, without.Orders, now, now, testConfig())
	if err != nil {
		t.Fatalf("BuildPlan with downtime: %v", err)
	}
	entry := plan.Orders[0]
	wantStart := downtime.EndAt // 生产段整体推到 10:00
	if !entry.SetupStart.Equal(wantStart) {
		t.Fatalf("setup start = %s, want %s", entry.SetupStart, wantStart)
	}
	// 换版/换墨/预热窗口可以落在停机期间，预热仍应恰在印刷前结束。
	if entry.Changeover.PreheatWindow.End != entry.PrintStart {
		t.Fatalf("preheat end %s != print start %s", entry.Changeover.PreheatWindow.End, entry.PrintStart)
	}
	// 交期影响解释应明确指出停机顺延。
	impact := findImpact(plan.ChangedDeliveries, order.ID)
	if impact == nil || impact.shift != 80*time.Minute {
		t.Fatalf("expected +80m impact, got %+v", impact)
	}
	if !containsCause(impact.Causes, "blade replacement") {
		t.Fatalf("impact causes must name the downtime, got %v", impact.Causes)
	}
}

func TestRebuildFreezesStartedOrderAndReplansRest(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	state := NewState(testConfig())

	a := mustOrder(t, "A", now.Add(72*time.Hour), "plate-a", []string{"cyan"}, time.Minute, 0, time.Hour, 0, now)
	b := mustOrder(t, "B", now.Add(96*time.Hour), "plate-b", []string{"magenta"}, time.Minute, 0, time.Hour, 0, now.Add(time.Second))
	for _, order := range []Order{a, b} {
		if _, err := state.AddOrder(order, now); err != nil {
			t.Fatalf("AddOrder: %v", err)
		}
	}
	firstPlan := state.Plan()
	originalA := firstPlan.Orders[0]
	originalBFinish := firstPlan.Orders[1].FinishAt

	// A 开工，窗口冻结；随后录入一张交期比 B 更早的急单 C，只能插到 A 之后、B 之前。
	if _, err := state.StartOrder(a.ID, now); err != nil {
		t.Fatalf("StartOrder: %v", err)
	}
	rush := mustOrder(t, "RUSH", now.Add(80*time.Hour), "plate-c", []string{"yellow"}, time.Minute, 0, time.Hour, 0, now.Add(2*time.Second))
	plan, err := state.AddOrder(rush, now)
	if err != nil {
		t.Fatalf("AddOrder rush: %v", err)
	}
	if len(plan.Orders) != 3 {
		t.Fatalf("expected 3 orders, got %d", len(plan.Orders))
	}
	if plan.Orders[0].OrderID != a.ID || !plan.Orders[0].Frozen {
		t.Fatal("started order A must remain first and frozen")
	}
	if !plan.Orders[0].SetupStart.Equal(originalA.SetupStart) {
		t.Fatalf("frozen window moved: %s -> %s", originalA.SetupStart, plan.Orders[0].SetupStart)
	}
	if plan.Orders[1].OrderID != rush.ID {
		t.Fatalf("rush order must be second after frozen A, got %s", plan.Orders[1].Name)
	}
	if plan.Orders[2].OrderID != b.ID {
		t.Fatal("B must be third")
	}
	if !plan.Orders[2].FinishAt.After(originalBFinish) {
		t.Fatalf("B finish must move later: old %s new %s", originalBFinish, plan.Orders[2].FinishAt)
	}
	impact := findImpact(plan.ChangedDeliveries, b.ID)
	if impact == nil || impact.shift <= 0 {
		t.Fatalf("expected positive shift impact on B, got %+v", impact)
	}
	if len(impact.Causes) == 0 {
		t.Fatal("impact on B must explain the rush insertion")
	}
}

func TestStartedOrderCannotBeEdited(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	state := NewState(testConfig())
	order := mustOrder(t, "A", now.Add(24*time.Hour), "p", []string{"cyan"}, time.Minute, 0, time.Hour, 0, now)
	if _, err := state.AddOrder(order, now); err != nil {
		t.Fatal(err)
	}
	if _, err := state.StartOrder(order.ID, now); err != nil {
		t.Fatal(err)
	}
	due := now.Add(48 * time.Hour)
	if _, err := state.UpdateOrder(order.ID, OrderUpdate{DueAt: &due}, now); err == nil {
		t.Fatal("updating a started order must be rejected")
	}
}

func TestCancelledOrderDropsOutOfPlan(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	state := NewState(testConfig())
	a := mustOrder(t, "A", now.Add(24*time.Hour), "p", []string{"cyan"}, time.Minute, 0, time.Hour, 0, now)
	b := mustOrder(t, "B", now.Add(48*time.Hour), "q", []string{"magenta"}, time.Minute, 0, time.Hour, 0, now.Add(time.Second))
	if _, err := state.AddOrder(a, now); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddOrder(b, now); err != nil {
		t.Fatal(err)
	}
	plan, err := state.CancelOrder(a.ID, now)
	if err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	if len(plan.Orders) != 1 || plan.Orders[0].OrderID != b.ID {
		t.Fatalf("cancelled A must leave only B, got %d orders", len(plan.Orders))
	}
}

func TestStatePersistRoundTrip(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	path := t.TempDir() + "/schedule.json"
	state := NewState(testConfig())
	order := mustOrder(t, "A", now.Add(24*time.Hour), "p", []string{"cyan"}, 30*time.Minute, 10*time.Minute, time.Hour, 5*time.Minute, now)
	if _, err := state.AddOrder(order, now); err != nil {
		t.Fatal(err)
	}
	downtime, err := NewDowntime("pm", now.Add(4*time.Hour), now.Add(5*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddDowntime(downtime, now); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	restored := NewState(testConfig())
	if err := restored.Load(path, now.Add(time.Minute)); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(restored.Orders()) != 1 || len(restored.Downtimes()) != 1 {
		t.Fatalf("restored counts wrong: %d orders, %d downtimes", len(restored.Orders()), len(restored.Downtimes()))
	}
	if restored.Plan().Orders[0].FinishAt != state.Plan().Orders[0].FinishAt {
		t.Fatal("restored plan windows differ")
	}

	// 开工后再恢复：已开工窗口必须作为冻结锚点，未执行部分顺延。
	if _, err := restored.StartOrder(order.ID, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	second := mustOrder(t, "B", now.Add(72*time.Hour), "q", []string{"magenta"}, 30*time.Minute, 0, time.Hour, 0, now.Add(time.Second))
	if _, err := restored.AddOrder(second, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := restored.Save(path); err != nil {
		t.Fatal(err)
	}
	again := NewState(testConfig())
	if err := again.Load(path, now.Add(10*time.Minute)); err != nil {
		t.Fatalf("second Load: %v", err)
	}
	frozen := again.Plan().Orders[0]
	if !frozen.Frozen || frozen.OrderID != order.ID {
		t.Fatalf("expected started order frozen after restore, got %+v", frozen)
	}
}

func TestDowntimeValidation(t *testing.T) {
	now := time.Now().UTC()
	if _, err := NewDowntime("bad", now.Add(time.Hour), now, now); err == nil {
		t.Fatal("downtime ending before start must be rejected")
	}
}

func findImpact(impacts []DeliveryImpact, orderID string) *DeliveryImpact {
	for i := range impacts {
		if impacts[i].OrderID == orderID {
			return &impacts[i]
		}
	}
	return nil
}

func containsCause(causes []string, fragment string) bool {
	for _, cause := range causes {
		if strings.Contains(cause, fragment) {
			return true
		}
	}
	return false
}
