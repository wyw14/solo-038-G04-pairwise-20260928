package schedule

import (
	"sort"
	"strconv"
	"time"
)

// planContext collects the mutable cursor state while laying orders on a
// single shared press/dryer timeline.
type planContext struct {
	config    Config
	horizon   time.Time
	now       time.Time
	downtimes []Downtime
	// exclusiveCursor is when the press line is free again after the last
	// frozen or planned order.
	exclusiveCursor time.Time
	// baseline is the configuration left mounted by the previous order.
	baselineValid  bool
	baselinePlate  string
	baselineColors []string
	baselineTemp   float64
}

// Plan generates a non-conflicting schedule.
//
// Running and completed orders are frozen at their recorded windows; only
// pending orders are re-sequenced. Pending orders are ordered by due date
// (EDD), breaking ties with shortest print duration (SPT) and then id.
func Plan(orders []Order, downtimes []Downtime, config Config, horizon, now time.Time, previous *Schedule) Schedule {
	sortedDowntimes := sortedDowntimeWindows(downtimes)
	ctx := &planContext{
		config:          config,
		horizon:         horizon.UTC(),
		now:             now.UTC(),
		downtimes:       sortedDowntimes,
		exclusiveCursor: horizon.UTC(),
	}

	var frozen []PlannedOrder
	var pending []Order
	for _, order := range orders {
		switch order.Status {
		case StatusCompleted, StatusRunning:
			frozen = append(frozen, frozenPlanFor(order, previous))
		case StatusPending:
			pending = append(pending, order)
		}
	}
	sort.Slice(frozen, func(i, j int) bool { return frozen[i].End.Before(frozen[j].End) })
	for _, planned := range frozen {
		if planned.End.After(ctx.exclusiveCursor) {
			ctx.exclusiveCursor = planned.End
		}
		if source, ok := findOrder(orders, planned.OrderID); ok {
			ctx.baselineValid = true
			ctx.baselinePlate = source.Plate
			ctx.baselineColors = source.Colors
			ctx.baselineTemp = source.DryerTargetTemp
		}
	}

	sort.Slice(pending, func(i, j int) bool {
		if !pending[i].DueAt.Equal(pending[j].DueAt) {
			return pending[i].DueAt.Before(pending[j].DueAt)
		}
		if pending[i].PrintDuration != pending[j].PrintDuration {
			return pending[i].PrintDuration < pending[j].PrintDuration
		}
		return pending[i].ID < pending[j].ID
	})

	planned := make([]PlannedOrder, 0, len(frozen)+len(pending))
	planned = append(planned, frozen...)
	for _, order := range pending {
		entry := ctx.planOrder(order)
		planned = append(planned, entry)
		ctx.baselineValid = true
		ctx.baselinePlate = order.Plate
		ctx.baselineColors = order.Colors
		ctx.baselineTemp = order.DryerTargetTemp
		if entry.End.After(ctx.exclusiveCursor) {
			ctx.exclusiveCursor = entry.End
		}
	}

	sort.Slice(planned, func(i, j int) bool {
		if !planned[i].PressStart.Equal(planned[j].PressStart) {
			return planned[i].PressStart.Before(planned[j].PressStart)
		}
		return planned[i].OrderID < planned[j].OrderID
	})
	for index := range planned {
		planned[index].Sequence = index + 1
	}
	allSteps := make([]Step, 0)
	for _, entry := range planned {
		allSteps = append(allSteps, entry.Steps...)
	}
	sortSteps(allSteps)
	impacts := diffImpacts(planned, orders, sortedDowntimes, previous)
	if impacts == nil {
		impacts = []Impact{}
	}

	return Schedule{
		GeneratedAt: now.UTC(),
		Horizon:     horizon.UTC(),
		Orders:      planned,
		Steps:       allSteps,
		Downtimes:   sortedDowntimes,
		Impacts:     impacts,
	}
}

func (ctx *planContext) planOrder(order Order) PlannedOrder {
	var steps []Step

	// Build the serial plate/ink changeover block starting at the press cursor.
	blockStart := earliestExclusive(ctx.exclusiveCursor, 0, ctx.downtimes)
	offset := time.Duration(0)

	if !ctx.baselineValid || ctx.baselinePlate != order.Plate {
		steps = append(steps, Step{
			OrderID: order.ID,
			Kind:    StepPlateChange,
			Start:   blockStart,
			End:     blockStart.Add(ctx.config.PlateChange),
			Detail:  plateChangeDetail(ctx.baselineValid, ctx.baselinePlate, order.Plate),
		})
		offset += ctx.config.PlateChange
	}

	added, removed := colorDiff(ctx.baselineColors, order.Colors)
	if !ctx.baselineValid {
		added = append([]string(nil), order.Colors...)
		removed = nil
	}
	if inkSwaps := len(added) + len(removed); inkSwaps > 0 {
		duration := ctx.config.InkChangePerColor * time.Duration(inkSwaps)
		steps = append(steps, Step{
			OrderID: order.ID,
			Kind:    StepInkChange,
			Start:   blockStart.Add(offset),
			End:     blockStart.Add(offset + duration),
			Detail:  inkChangeDetail(added, removed),
		})
		offset += duration
	}

	// The whole changeover block must avoid downtime windows.
	changeDuration := offset
	if changeDuration > 0 {
		blockStart = earliestExclusive(ctx.exclusiveCursor, changeDuration, ctx.downtimes)
		offset = time.Duration(0)
		for index := range steps {
			steps[index].Start = blockStart.Add(offset)
			switch steps[index].Kind {
			case StepPlateChange:
				offset += ctx.config.PlateChange
			case StepInkChange:
				offset += ctx.config.InkChangePerColor * time.Duration(len(added)+len(removed))
			}
			steps[index].End = blockStart.Add(offset)
		}
	}
	changeStart := blockStart
	changeEnd := blockStart.Add(changeDuration)

	lift := 0.0
	switch {
	case order.DryerTargetTemp > ctx.baselineTemp:
		lift = order.DryerTargetTemp - ctx.baselineTemp
		if !ctx.baselineValid {
			lift = order.DryerTargetTemp
		}
	}
	preheatDuration := time.Duration(0)
	if lift > 0 {
		preheatDuration = time.Duration(float64(ctx.config.DryerRampPerC) * lift)
		if preheatDuration < ctx.config.DryerMinPreheat {
			preheatDuration = ctx.config.DryerMinPreheat
		}
	}

	// Resolve the press start jointly with the dryer preheat. Preheat runs in
	// parallel with the plate/ink changeover (it may start when the changeover
	// starts), but its window must not cross a downtime, and printing itself
	// must never overlap a downtime. Both preheat and changeover must finish
	// before the press starts.
	printStart, preheatStart := resolvePrintStart(blockStart, changeEnd, preheatDuration, order.PrintDuration, ctx.downtimes)
	printEnd := printStart.Add(order.PrintDuration)

	if preheatDuration > 0 {
		steps = append(steps, Step{
			OrderID: order.ID,
			Kind:    StepPreheat,
			Start:   preheatStart,
			End:     printStart,
			Detail:  preheatDetail(ctx.baselineValid, ctx.baselineTemp, order.DryerTargetTemp),
		})
	}
	steps = append(steps, Step{OrderID: order.ID, Kind: StepPrint, Start: printStart, End: printEnd, Detail: "print run"})
	sortSteps(steps)

	onTime := !printEnd.After(order.DueAt)
	entry := PlannedOrder{
		OrderID:         order.ID,
		Status:          order.Status,
		Plate:           order.Plate,
		Colors:          append([]string(nil), order.Colors...),
		DryerTargetTemp: order.DryerTargetTemp,
		PrintDuration:   order.PrintDuration,
		Start:           changeStart,
		PressStart:      printStart,
		End:             printEnd,
		DueAt:           order.DueAt,
		OnTime:          onTime,
		Steps:           steps,
	}
	if !onTime {
		entry.Lateness = printEnd.Sub(order.DueAt)
	}
	return entry
}

// resolvePrintStart finds the earliest press start at or after changeEnd such
// that [start, start+printDuration] avoids every downtime and the dryer has an
// uninterrupted [start-preheat, start] preheat window beginning no earlier
// than changeStart (it runs in parallel with the changeover) and never
// crossing a downtime. It returns the press start and the preheat start.
func resolvePrintStart(changeStart, changeEnd time.Time, preheat, printDuration time.Duration, downtimes []Downtime) (time.Time, time.Time) {
	start := earliestExclusive(changeEnd, printDuration, downtimes)
	for {
		if preheat <= 0 {
			return start, time.Time{}
		}
		preheatStart := start.Add(-preheat)
		if preheatStart.Before(changeStart) {
			// Not enough parallel runway inside the changeover window; push the
			// press start out so preheat can begin exactly at changeStart.
			start = earliestExclusive(changeStart.Add(preheat), printDuration, downtimes)
			continue
		}
		collision := false
		for _, window := range downtimes {
			if window.overlaps(preheatStart, start) {
				start = earliestExclusive(window.End.Add(preheat), printDuration, downtimes)
				collision = true
				break
			}
		}
		if !collision {
			return start, preheatStart
		}
	}
}

// earliestExclusive returns the earliest start at or after desiredAt for
// which the whole [start, start+duration] window avoids every downtime.
func earliestExclusive(desiredAt time.Time, duration time.Duration, downtimes []Downtime) time.Time {
	start := desiredAt
	for {
		collision := false
		for _, window := range downtimes {
			if window.overlaps(start, start.Add(duration)) {
				start = window.End
				collision = true
				break
			}
		}
		if !collision {
			return start
		}
	}
}

func frozenPlanFor(order Order, previous *Schedule) PlannedOrder {
	// Executed work is frozen at the recorded actual window. StartOrder stamps
	// StartedAt and the projected completion; CompleteOrder stamps the finish.
	end := order.CompletedAt
	if end.IsZero() {
		end = order.ProjectedCompleteAt
	}
	start := order.StartedAt
	if start.IsZero() {
		start = end
	}
	if previous != nil && order.StartedAt.IsZero() && order.CompletedAt.IsZero() && order.ProjectedCompleteAt.IsZero() {
		// Defensive fallback for state restored without execution stamps.
		for _, planned := range previous.Orders {
			if planned.OrderID == order.ID {
				start = planned.PressStart
				end = planned.End
				break
			}
		}
	}
	onTime := !end.After(order.DueAt)
	entry := PlannedOrder{
		OrderID:         order.ID,
		Status:          order.Status,
		Plate:           order.Plate,
		Colors:          append([]string(nil), order.Colors...),
		DryerTargetTemp: order.DryerTargetTemp,
		PrintDuration:   order.PrintDuration,
		Start:           start,
		PressStart:      start,
		End:             end,
		DueAt:           order.DueAt,
		OnTime:          onTime,
		Steps:           []Step{{OrderID: order.ID, Kind: StepPrint, Start: start, End: end, Detail: "print run"}},
		Frozen:          true,
	}
	if !onTime {
		entry.Lateness = end.Sub(order.DueAt)
	}
	return entry
}

func sortedDowntimeWindows(downtimes []Downtime) []Downtime {
	values := append([]Downtime(nil), downtimes...)
	sort.Slice(values, func(i, j int) bool {
		if !values[i].Start.Equal(values[j].Start) {
			return values[i].Start.Before(values[j].Start)
		}
		return values[i].End.Before(values[j].End)
	})
	return values
}

func sortSteps(steps []Step) {
	sort.Slice(steps, func(i, j int) bool {
		if !steps[i].Start.Equal(steps[j].Start) {
			return steps[i].Start.Before(steps[j].Start)
		}
		return steps[i].Kind < steps[j].Kind
	})
}

func findOrder(orders []Order, id string) (Order, bool) {
	for _, order := range orders {
		if order.ID == id {
			return order, true
		}
	}
	return Order{}, false
}

func colorDiff(previous, current []string) (added, removed []string) {
	prevSet := map[string]bool{}
	for _, color := range previous {
		prevSet[color] = true
	}
	curSet := map[string]bool{}
	for _, color := range current {
		curSet[color] = true
	}
	for color := range curSet {
		if !prevSet[color] {
			added = append(added, color)
		}
	}
	for color := range prevSet {
		if !curSet[color] {
			removed = append(removed, color)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

func plateChangeDetail(hasBaseline bool, previous, current string) string {
	if !hasBaseline {
		return "mount plate " + current
	}
	return "change plate " + previous + " -> " + current
}

func inkChangeDetail(added, removed []string) string {
	detail := "change inks"
	if len(added) > 0 {
		detail += " +" + joinSorted(added)
	}
	if len(removed) > 0 {
		detail += " -" + joinSorted(removed)
	}
	return detail
}

func preheatDetail(hasBaseline bool, previous, current float64) string {
	if !hasBaseline {
		return "preheat dryer to " + formatTemp(current) + "C"
	}
	return "preheat dryer " + formatTemp(previous) + "C -> " + formatTemp(current) + "C"
}

func joinSorted(values []string) string {
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	result := ""
	for index, value := range sorted {
		if index > 0 {
			result += ","
		}
		result += value
	}
	return result
}

func formatTemp(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}
