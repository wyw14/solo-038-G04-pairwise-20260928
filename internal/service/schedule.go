package service

import (
	"time"

	"github.com/wyw14/solo-038/internal/schedule"
)

// OrderRequest is the operator-facing input for creating or revising a print
// order in the scheduling queue.
type OrderRequest struct {
	DueAt           time.Time `json:"due_at"`
	PrintDuration   float64   `json:"print_duration_seconds"`
	Plate           string    `json:"plate"`
	Colors          []string  `json:"colors"`
	DryerTargetTemp float64   `json:"dryer_target_temp"`
}

// DowntimeRequest registers an equipment outage window.
type DowntimeRequest struct {
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	Reason string    `json:"reason"`
}

func toOrderInput(request OrderRequest) schedule.OrderInput {
	return schedule.OrderInput{
		DueAt:           request.DueAt,
		PrintDuration:   time.Duration(request.PrintDuration * float64(time.Second)),
		Plate:           request.Plate,
		Colors:          request.Colors,
		DryerTargetTemp: request.DryerTargetTemp,
	}
}

func (runtime *Runtime) ScheduledOrders() []schedule.Order {
	return runtime.scheduler.Orders()
}

func (runtime *Runtime) Schedule() schedule.Schedule {
	return runtime.scheduler.Schedule()
}

func (runtime *Runtime) CreateScheduledOrder(request OrderRequest) (schedule.Order, schedule.Schedule, error) {
	return runtime.scheduler.CreateOrder(toOrderInput(request), time.Now())
}

func (runtime *Runtime) ReviseScheduledOrder(id string, request OrderRequest) (schedule.Order, schedule.Schedule, error) {
	return runtime.scheduler.ReviseOrder(id, toOrderInput(request), time.Now())
}

func (runtime *Runtime) CancelScheduledOrder(id string) (schedule.Schedule, error) {
	return runtime.scheduler.CancelOrder(id, time.Now())
}

func (runtime *Runtime) StartScheduledOrder(id string) (schedule.Order, schedule.Schedule, error) {
	return runtime.scheduler.StartOrder(id, time.Now())
}

func (runtime *Runtime) CompleteScheduledOrder(id string) (schedule.Order, schedule.Schedule, error) {
	return runtime.scheduler.CompleteOrder(id, time.Now())
}

func (runtime *Runtime) ScheduleDowntimes() []schedule.Downtime {
	return runtime.scheduler.Downtimes()
}

func (runtime *Runtime) AddScheduleDowntime(request DowntimeRequest) (schedule.Downtime, schedule.Schedule, error) {
	return runtime.scheduler.AddDowntime(request.Start, request.End, request.Reason, time.Now())
}

func (runtime *Runtime) RemoveScheduleDowntime(id string) (schedule.Schedule, error) {
	return runtime.scheduler.RemoveDowntime(id, time.Now())
}

func (runtime *Runtime) ReplanSchedule() schedule.Schedule {
	return runtime.scheduler.Replan(time.Now())
}
