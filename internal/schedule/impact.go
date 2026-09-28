package schedule

import (
	"fmt"
	"sort"
	"time"
)

// diffImpacts compares a freshly generated plan with the previous version and
// explains every pending order whose promised finish or on-time status moved.
// Frozen (running/completed) orders are never reported as impacted.
func diffImpacts(planned []PlannedOrder, orders []Order, currentDowntimes []Downtime, previous *Schedule) []Impact {
	if previous == nil {
		return nil
	}
	prevByID := map[string]PlannedOrder{}
	for _, entry := range previous.Orders {
		prevByID[entry.OrderID] = entry
	}
	newDowntimes := addedDowntimes(previous.Downtimes, currentDowntimes)

	impacts := make([]Impact, 0)
	for _, entry := range planned {
		if entry.Frozen || entry.Status == StatusCancelled {
			continue
		}
		prev, ok := prevByID[entry.OrderID]
		if !ok {
			continue
		}
		delta := entry.End.Sub(prev.End)
		dueMoved := !entry.DueAt.Equal(prev.DueAt)
		onTimeChanged := entry.OnTime != prev.OnTime
		// Sub-second drift between replans (the horizon follows the clock) is
		// not a meaningful change to a promised delivery; only surface real
		// movement, on-time flips, or ticket edits.
		materiallyMoved := delta >= time.Second || delta <= -time.Second
		if !dueMoved && !onTimeChanged && !materiallyMoved {
			continue
		}
		previousEnd := prev.End
		impacts = append(impacts, Impact{
			OrderID:          entry.OrderID,
			DueAt:            entry.DueAt,
			PreviousEnd:      &previousEnd,
			ScheduledEnd:     entry.End,
			Delta:            delta,
			PreviouslyOnTime: prev.OnTime,
			OnTime:           entry.OnTime,
			Reasons:          impactReasons(entry, prev, orders, previous, newDowntimes),
		})
	}
	sort.Slice(impacts, func(i, j int) bool {
		if impacts[i].Delta != impacts[j].Delta {
			return impacts[i].Delta > impacts[j].Delta
		}
		return impacts[i].OrderID < impacts[j].OrderID
	})
	return impacts
}

func impactReasons(entry, prev PlannedOrder, orders []Order, previous *Schedule, newDowntimes []Downtime) []string {
	var reasons []string
	source, _ := findOrder(orders, entry.OrderID)

	if !entry.DueAt.Equal(prev.DueAt) {
		reasons = append(reasons, fmt.Sprintf("due date changed from %s to %s", prev.DueAt.Format(time.RFC3339), entry.DueAt.Format(time.RFC3339)))
	}
	if !source.UpdatedAt.IsZero() && source.UpdatedAt.After(previous.GeneratedAt) && !specUnchanged(prev, entry) {
		reasons = append(reasons, "order ticket (duration, plate, colors or dryer setting) was revised")
	}
	if window, ok := downtimeBetween(newDowntimes, prev.PressStart, entry.PressStart); ok {
		reasons = append(reasons, fmt.Sprintf("new equipment downtime %q (%s until %s) blocks the line", window.Reason, window.Start.Format(time.RFC3339), window.End.Format(time.RFC3339)))
	}
	if window, ok := downtimeBetween(previous.Downtimes, prev.PressStart, entry.PressStart); ok && !containsDowntime(newDowntimes, window.ID) {
		reasons = append(reasons, fmt.Sprintf("shifted around equipment downtime %q (%s until %s)", window.Reason, window.Start.Format(time.RFC3339), window.End.Format(time.RFC3339)))
	}
	if entry.Sequence != prev.Sequence {
		verb := "moved later"
		if entry.Sequence < prev.Sequence {
			verb = "moved earlier"
		}
		reasons = append(reasons, fmt.Sprintf("queue position %s (#%d -> #%d)", verb, prev.Sequence, entry.Sequence))
	}
	if len(reasons) == 0 {
		switch {
		case entry.End.After(prev.End):
			reasons = append(reasons, "upstream changeover work shifted the start time")
		case entry.End.Before(prev.End):
			reasons = append(reasons, "upstream queue cleared earlier than previously planned")
		}
	}
	if prev.OnTime && !entry.OnTime {
		reasons = append(reasons, fmt.Sprintf("now misses due date by %s", entry.Lateness.Round(time.Second)))
	}
	if !prev.OnTime && entry.OnTime {
		reasons = append(reasons, "order is back on time")
	}
	return reasons
}

// downtimeBetween reports a window that sits in the gap between the previous
// and current press start, i.e. one that plausibly caused the push.
func downtimeBetween(windows []Downtime, previousStart, currentStart time.Time) (Downtime, bool) {
	if !currentStart.After(previousStart) {
		return Downtime{}, false
	}
	for _, window := range windows {
		if window.End.After(previousStart) && !window.Start.After(currentStart) {
			return window, true
		}
	}
	return Downtime{}, false
}

func addedDowntimes(previous, current []Downtime) []Downtime {
	known := map[string]bool{}
	for _, window := range previous {
		known[window.ID] = true
	}
	values := make([]Downtime, 0)
	for _, window := range current {
		if !known[window.ID] {
			values = append(values, window)
		}
	}
	return values
}

func containsDowntime(windows []Downtime, id string) bool {
	for _, window := range windows {
		if window.ID == id {
			return true
		}
	}
	return false
}

// specUnchanged reports whether the ticket's physical requirements are the
// same in both plan versions. A due-date-only edit must not be reported as a
// process revision.
func specUnchanged(previous, current PlannedOrder) bool {
	if previous.Plate != current.Plate ||
		previous.DryerTargetTemp != current.DryerTargetTemp ||
		previous.PrintDuration != current.PrintDuration {
		return false
	}
	prevColors := map[string]bool{}
	for _, color := range previous.Colors {
		prevColors[color] = true
	}
	if len(prevColors) != len(current.Colors) {
		return false
	}
	for _, color := range current.Colors {
		if !prevColors[color] {
			return false
		}
	}
	return true
}
