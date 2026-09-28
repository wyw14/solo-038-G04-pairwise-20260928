package schedule

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// OrderStatus tracks how far a print order has moved through execution.
type OrderStatus string

const (
	StatusPending   OrderStatus = "pending"
	StatusRunning   OrderStatus = "running"
	StatusCompleted OrderStatus = "completed"
	StatusCancelled OrderStatus = "cancelled"
)

// Order is a print ticket waiting for press time.
type Order struct {
	ID              string        `json:"id"`
	DueAt           time.Time     `json:"due_at"`
	PrintDuration   time.Duration `json:"print_duration_seconds"`
	Plate           string        `json:"plate"`
	Colors          []string      `json:"colors"`
	DryerTargetTemp float64       `json:"dryer_target_temp"`
	Status          OrderStatus   `json:"status"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
	// StartedAt / CompletedAt freeze the executed portion of the schedule.
	StartedAt           time.Time `json:"started_at,omitempty"`
	ProjectedCompleteAt time.Time `json:"projected_complete_at,omitempty"`
	CompletedAt         time.Time `json:"completed_at,omitempty"`
}

func (order Order) Validate() error {
	var problems []string
	if strings.TrimSpace(order.ID) == "" {
		problems = append(problems, "order id is required")
	}
	if order.DueAt.IsZero() {
		problems = append(problems, "due date is required")
	}
	if order.PrintDuration <= 0 {
		problems = append(problems, "print duration must be positive")
	}
	if strings.TrimSpace(order.Plate) == "" {
		problems = append(problems, "plate is required")
	}
	if len(order.Colors) == 0 {
		problems = append(problems, "at least one ink color is required")
	}
	seen := map[string]bool{}
	for _, color := range order.Colors {
		if strings.TrimSpace(color) == "" {
			problems = append(problems, "ink color cannot be blank")
		}
		if seen[color] {
			problems = append(problems, fmt.Sprintf("ink color %q is duplicated", color))
		}
		seen[color] = true
	}
	if order.DryerTargetTemp < 0 {
		problems = append(problems, "dryer target temperature cannot be negative")
	}
	return errors.Join(problemsToErrors(problems)...)
}

// Signature is the configuration that drives changeover work between orders.
func (order Order) Signature() string {
	colors := append([]string(nil), order.Colors...)
	sort.Strings(colors)
	return strings.Join([]string{order.Plate, strings.Join(colors, "|"), fmt.Sprintf("%.2f", order.DryerTargetTemp)}, "#")
}

// Downtime is a window where the line (press and dryer) is unavailable.
type Downtime struct {
	ID        string    `json:"id"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

func (window Downtime) Validate() error {
	var problems []string
	if strings.TrimSpace(window.ID) == "" {
		problems = append(problems, "downtime id is required")
	}
	if window.Start.IsZero() || !window.End.After(window.Start) {
		problems = append(problems, "downtime window must have end after start")
	}
	if strings.TrimSpace(window.Reason) == "" {
		problems = append(problems, "downtime reason is required")
	}
	return errors.Join(problemsToErrors(problems)...)
}

func (window Downtime) overlaps(start, end time.Time) bool {
	return start.Before(window.End) && end.After(window.Start)
}

// Config holds line-wide changeover timing assumptions.
type Config struct {
	PlateChange       time.Duration // time to swap plates when plate id changes
	InkChangePerColor time.Duration // time per color added or removed on the units
	DryerRampPerC     time.Duration // preheat time needed per degree Celsius of lift
	DryerMinPreheat   time.Duration // minimum preheat window even for a small lift
}

func (config Config) Validate() error {
	var problems []string
	if config.PlateChange < 0 {
		problems = append(problems, "plate change duration cannot be negative")
	}
	if config.InkChangePerColor < 0 {
		problems = append(problems, "ink change duration cannot be negative")
	}
	if config.DryerRampPerC <= 0 {
		problems = append(problems, "dryer ramp duration per degree must be positive")
	}
	if config.DryerMinPreheat < 0 {
		problems = append(problems, "dryer minimum preheat cannot be negative")
	}
	return errors.Join(problemsToErrors(problems)...)
}

// StepKind enumerates the windows a planned order is broken into.
type StepKind string

const (
	StepPlateChange StepKind = "plate_change"
	StepInkChange   StepKind = "ink_change"
	StepPreheat     StepKind = "dryer_preheat"
	StepPrint       StepKind = "print"
)

// Step is one scheduled activity window. Preheat may overlap the previous
// order's print window; plate/ink changeover and print never overlap.
type Step struct {
	OrderID string    `json:"order_id"`
	Kind    StepKind  `json:"kind"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Detail  string    `json:"detail"`
}

func (step Step) validateTimes() bool {
	return !step.Start.IsZero() && !step.End.IsZero() && !step.End.Before(step.Start)
}

// PlannedOrder is one order's position in the generated schedule.
type PlannedOrder struct {
	OrderID  string      `json:"order_id"`
	Sequence int         `json:"sequence"`
	Status   OrderStatus `json:"status"`
	// Spec snapshots the ticket requirements so plan versions can be compared
	// independently of where the order sits in the queue.
	Plate           string        `json:"plate"`
	Colors          []string      `json:"colors"`
	DryerTargetTemp float64       `json:"dryer_target_temp"`
	PrintDuration   time.Duration `json:"print_duration_seconds"`
	Start           time.Time     `json:"start"`       // start of changeover work
	PressStart      time.Time     `json:"press_start"` // ink/plates ready and dryer warm
	End             time.Time     `json:"end"`         // print finish
	DueAt           time.Time     `json:"due_at"`
	OnTime          bool          `json:"on_time"`
	Lateness        time.Duration `json:"lateness_seconds"`
	Steps           []Step        `json:"steps"`
	Frozen          bool          `json:"frozen"`
}

// Impact explains how one order's promised finish moved versus the last plan.
type Impact struct {
	OrderID          string        `json:"order_id"`
	DueAt            time.Time     `json:"due_at"`
	PreviousEnd      *time.Time    `json:"previous_end,omitempty"`
	ScheduledEnd     time.Time     `json:"scheduled_end"`
	Delta            time.Duration `json:"delta"`
	PreviouslyOnTime bool          `json:"previously_on_time"`
	OnTime           bool          `json:"on_time"`
	Reasons          []string      `json:"reasons"`
}

// Schedule is one generated version of the plan.
type Schedule struct {
	Version     uint64         `json:"version"`
	GeneratedAt time.Time      `json:"generated_at"`
	Horizon     time.Time      `json:"horizon"`
	Orders      []PlannedOrder `json:"orders"`
	Steps       []Step         `json:"steps"`
	Downtimes   []Downtime     `json:"downtimes"`
	Impacts     []Impact       `json:"impacts"`
}

func problemsToErrors(problems []string) []error {
	values := make([]error, 0, len(problems))
	for _, problem := range problems {
		values = append(values, errors.New(problem))
	}
	return values
}
