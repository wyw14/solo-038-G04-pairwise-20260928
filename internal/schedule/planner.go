package schedule

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Sentinel errors let API layers map failures to status codes.
var (
	ErrNotFound = errors.New("schedule entry not found")
	ErrConflict = errors.New("schedule entry is in a conflicting state")
	ErrInvalid  = errors.New("schedule input is invalid")
)

// Planner owns the print-order queue, equipment downtime windows and the
// current plan version. Every mutation re-runs Plan for the not-yet-executed
// portion only; running and completed orders stay frozen.
type Planner struct {
	mu        sync.RWMutex
	path      string
	config    Config
	orders    map[string]Order
	downtimes map[string]Downtime
	schedule  *Schedule
}

type plannerSnapshot struct {
	Orders    []Order    `json:"orders"`
	Downtimes []Downtime `json:"downtimes"`
	Config    Config     `json:"config"`
	Schedule  *Schedule  `json:"schedule"`
}

// NewPlanner creates an empty planner backed by path (JSON snapshot).
func NewPlanner(path string, config Config) (*Planner, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	planner := &Planner{path: path, config: config, orders: map[string]Order{}, downtimes: map[string]Downtime{}}
	planner.replanLocked(time.Now().UTC())
	return planner, nil
}

// LoadPlanner restores a planner from path, falling back to an empty planner
// when no snapshot exists yet.
func LoadPlanner(path string, config Config) (*Planner, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return NewPlanner(path, config)
	}
	if err != nil {
		return nil, err
	}
	var snapshot plannerSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, fmt.Errorf("parse schedule snapshot: %w", err)
	}
	planner := &Planner{
		path:      path,
		config:    config,
		orders:    map[string]Order{},
		downtimes: map[string]Downtime{},
		schedule:  snapshot.Schedule,
	}
	// A valid stored config wins so saved line settings survive restarts.
	if err := snapshot.Config.Validate(); err == nil {
		planner.config = snapshot.Config
	}
	for _, order := range snapshot.Orders {
		planner.orders[order.ID] = order
	}
	for _, window := range snapshot.Downtimes {
		planner.downtimes[window.ID] = window
	}
	return planner, nil
}

// OrderInput carries the fields an operator can set on a print ticket.
type OrderInput struct {
	DueAt           time.Time
	PrintDuration   time.Duration
	Plate           string
	Colors          []string
	DryerTargetTemp float64
}

func (input OrderInput) validate() error {
	order := Order{ID: "x", DueAt: input.DueAt, PrintDuration: input.PrintDuration, Plate: input.Plate, Colors: input.Colors, DryerTargetTemp: input.DryerTargetTemp}
	return order.Validate()
}

func (planner *Planner) CreateOrder(input OrderInput, now time.Time) (Order, Schedule, error) {
	if err := input.validate(); err != nil {
		return Order{}, Schedule{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	planner.mu.Lock()
	defer planner.mu.Unlock()
	now = now.UTC()
	order := Order{
		ID:              uuid.NewString(),
		DueAt:           input.DueAt.UTC(),
		PrintDuration:   input.PrintDuration,
		Plate:           input.Plate,
		Colors:          normalizedColors(input.Colors),
		DryerTargetTemp: input.DryerTargetTemp,
		Status:          StatusPending,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	planner.orders[order.ID] = order
	if err := planner.persistLocked(); err != nil {
		return Order{}, Schedule{}, err
	}
	return order, *planner.replanLocked(now), nil
}

// ReviseOrder changes a ticket that has not started yet. Running or completed
// orders are frozen and must not be revised here.
func (planner *Planner) ReviseOrder(id string, input OrderInput, now time.Time) (Order, Schedule, error) {
	if err := input.validate(); err != nil {
		return Order{}, Schedule{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	planner.mu.Lock()
	defer planner.mu.Unlock()
	order, ok := planner.orders[id]
	if !ok {
		return Order{}, Schedule{}, fmt.Errorf("%w: order %q", ErrNotFound, id)
	}
	if order.Status != StatusPending {
		return Order{}, Schedule{}, fmt.Errorf("%w: order %q is %s and can no longer be revised", ErrConflict, id, order.Status)
	}
	now = now.UTC()
	order.DueAt = input.DueAt.UTC()
	order.PrintDuration = input.PrintDuration
	order.Plate = input.Plate
	order.Colors = normalizedColors(input.Colors)
	order.DryerTargetTemp = input.DryerTargetTemp
	order.UpdatedAt = now
	planner.orders[id] = order
	if err := planner.persistLocked(); err != nil {
		return Order{}, Schedule{}, err
	}
	return order, *planner.replanLocked(now), nil
}

func (planner *Planner) CancelOrder(id string, now time.Time) (Schedule, error) {
	planner.mu.Lock()
	defer planner.mu.Unlock()
	order, ok := planner.orders[id]
	if !ok {
		return Schedule{}, fmt.Errorf("%w: order %q", ErrNotFound, id)
	}
	if order.Status == StatusCompleted || order.Status == StatusCancelled {
		return Schedule{}, fmt.Errorf("%w: order %q is %s and cannot be cancelled", ErrConflict, id, order.Status)
	}
	now = now.UTC()
	order.Status = StatusCancelled
	order.UpdatedAt = now
	planner.orders[id] = order
	if err := planner.persistLocked(); err != nil {
		return Schedule{}, err
	}
	return *planner.replanLocked(now), nil
}

// StartOrder freezes a pending order as running from now with its remaining
// print duration; only later orders are re-sequenced.
func (planner *Planner) StartOrder(id string, now time.Time) (Order, Schedule, error) {
	planner.mu.Lock()
	defer planner.mu.Unlock()
	order, ok := planner.orders[id]
	if !ok {
		return Order{}, Schedule{}, fmt.Errorf("%w: order %q", ErrNotFound, id)
	}
	if order.Status != StatusPending {
		return Order{}, Schedule{}, fmt.Errorf("%w: order %q is %s, not pending", ErrConflict, id, order.Status)
	}
	now = now.UTC()
	order.Status = StatusRunning
	order.StartedAt = now
	order.ProjectedCompleteAt = now.Add(order.PrintDuration)
	order.UpdatedAt = now
	planner.orders[id] = order
	if err := planner.persistLocked(); err != nil {
		return Order{}, Schedule{}, err
	}
	return order, *planner.replanLocked(now), nil
}

func (planner *Planner) CompleteOrder(id string, now time.Time) (Order, Schedule, error) {
	planner.mu.Lock()
	defer planner.mu.Unlock()
	order, ok := planner.orders[id]
	if !ok {
		return Order{}, Schedule{}, fmt.Errorf("%w: order %q", ErrNotFound, id)
	}
	if order.Status != StatusRunning {
		return Order{}, Schedule{}, fmt.Errorf("%w: order %q is %s, not running", ErrConflict, id, order.Status)
	}
	now = now.UTC()
	if order.ProjectedCompleteAt.IsZero() {
		order.ProjectedCompleteAt = now
	}
	order.Status = StatusCompleted
	order.CompletedAt = now
	order.UpdatedAt = now
	planner.orders[id] = order
	if err := planner.persistLocked(); err != nil {
		return Order{}, Schedule{}, err
	}
	return order, *planner.replanLocked(now), nil
}

// AddDowntime registers an equipment outage and replans the open queue around
// it. Historical windows ending before now are kept for the audit trail.
func (planner *Planner) AddDowntime(start, end time.Time, reason string, now time.Time) (Downtime, Schedule, error) {
	window := Downtime{ID: uuid.NewString(), Start: start.UTC(), End: end.UTC(), Reason: reason, CreatedAt: now.UTC()}
	if err := window.Validate(); err != nil {
		return Downtime{}, Schedule{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	planner.mu.Lock()
	defer planner.mu.Unlock()
	planner.downtimes[window.ID] = window
	if err := planner.persistLocked(); err != nil {
		return Downtime{}, Schedule{}, err
	}
	return window, *planner.replanLocked(now.UTC()), nil
}

func (planner *Planner) RemoveDowntime(id string, now time.Time) (Schedule, error) {
	planner.mu.Lock()
	defer planner.mu.Unlock()
	if _, ok := planner.downtimes[id]; !ok {
		return Schedule{}, fmt.Errorf("%w: downtime %q", ErrNotFound, id)
	}
	delete(planner.downtimes, id)
	if err := planner.persistLocked(); err != nil {
		return Schedule{}, err
	}
	return *planner.replanLocked(now.UTC()), nil
}

// Replan regenerates the plan for the current state, e.g. via an explicit
// operator request.
func (planner *Planner) Replan(now time.Time) Schedule {
	planner.mu.Lock()
	defer planner.mu.Unlock()
	return *planner.replanLocked(now.UTC())
}

func (planner *Planner) replanLocked(now time.Time) *Schedule {
	orders := planner.orderList()
	downtimes := planner.downtimeList()
	previous := planner.schedule
	generated := Plan(orders, downtimes, planner.config, now, now, previous)
	version := uint64(1)
	if previous != nil {
		version = previous.Version + 1
	}
	generated.Version = version
	planner.schedule = &generated
	_ = planner.persistLocked()
	return planner.schedule
}

func (planner *Planner) Schedule() Schedule {
	planner.mu.RLock()
	defer planner.mu.RUnlock()
	if planner.schedule == nil {
		return Schedule{Version: 0, GeneratedAt: time.Now().UTC()}
	}
	return *planner.schedule
}

func (planner *Planner) Order(id string) (Order, bool) {
	planner.mu.RLock()
	defer planner.mu.RUnlock()
	order, ok := planner.orders[id]
	return order, ok
}

func (planner *Planner) Orders() []Order {
	planner.mu.RLock()
	defer planner.mu.RUnlock()
	return planner.orderList()
}

func (planner *Planner) Downtimes() []Downtime {
	planner.mu.RLock()
	defer planner.mu.RUnlock()
	return planner.downtimeList()
}

func (planner *Planner) orderList() []Order {
	values := make([]Order, 0, len(planner.orders))
	for _, order := range planner.orders {
		values = append(values, order)
	}
	sort.Slice(values, func(i, j int) bool {
		if !values[i].CreatedAt.Equal(values[j].CreatedAt) {
			return values[i].CreatedAt.Before(values[j].CreatedAt)
		}
		return values[i].ID < values[j].ID
	})
	return values
}

func (planner *Planner) downtimeList() []Downtime {
	values := make([]Downtime, 0, len(planner.downtimes))
	for _, window := range planner.downtimes {
		values = append(values, window)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Start.Before(values[j].Start) })
	return values
}

func (planner *Planner) persistLocked() error {
	if planner.path == "" {
		return nil
	}
	snapshot := plannerSnapshot{Orders: planner.orderList(), Downtimes: planner.downtimeList(), Config: planner.config, Schedule: planner.schedule}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(planner.path), 0o755); err != nil {
		return err
	}
	temporary := planner.path + ".next"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, planner.path)
}

func normalizedColors(colors []string) []string {
	seen := map[string]bool{}
	values := make([]string, 0, len(colors))
	for _, color := range colors {
		if seen[color] {
			continue
		}
		seen[color] = true
		values = append(values, color)
	}
	sort.Strings(values)
	return values
}
