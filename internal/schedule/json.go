package schedule

import (
	"encoding/json"
	"time"
)

// Durations are exchanged as seconds so operators and clients do not deal
// with nanosecond integers; internally the package keeps time.Duration.

type orderJSON struct {
	ID                   string      `json:"id"`
	DueAt                time.Time   `json:"due_at"`
	PrintDurationSeconds float64     `json:"print_duration_seconds"`
	Plate                string      `json:"plate"`
	Colors               []string    `json:"colors"`
	DryerTargetTemp      float64     `json:"dryer_target_temp"`
	Status               OrderStatus `json:"status"`
	CreatedAt            time.Time   `json:"created_at"`
	UpdatedAt            time.Time   `json:"updated_at"`
	StartedAt            *time.Time  `json:"started_at,omitempty"`
	ProjectedCompleteAt  *time.Time  `json:"projected_complete_at,omitempty"`
	CompletedAt          *time.Time  `json:"completed_at,omitempty"`
}

func (order Order) MarshalJSON() ([]byte, error) {
	return json.Marshal(orderJSON{
		ID:                   order.ID,
		DueAt:                order.DueAt,
		PrintDurationSeconds: order.PrintDuration.Seconds(),
		Plate:                order.Plate,
		Colors:               order.Colors,
		DryerTargetTemp:      order.DryerTargetTemp,
		Status:               order.Status,
		CreatedAt:            order.CreatedAt,
		UpdatedAt:            order.UpdatedAt,
		StartedAt:            pointerTime(order.StartedAt),
		ProjectedCompleteAt:  pointerTime(order.ProjectedCompleteAt),
		CompletedAt:          pointerTime(order.CompletedAt),
	})
}

func (order *Order) UnmarshalJSON(data []byte) error {
	var value orderJSON
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*order = Order{
		ID:              value.ID,
		DueAt:           value.DueAt,
		PrintDuration:   seconds(value.PrintDurationSeconds),
		Plate:           value.Plate,
		Colors:          value.Colors,
		DryerTargetTemp: value.DryerTargetTemp,
		Status:          value.Status,
		CreatedAt:       value.CreatedAt,
		UpdatedAt:       value.UpdatedAt,
	}
	derefTime(&order.StartedAt, value.StartedAt)
	derefTime(&order.ProjectedCompleteAt, value.ProjectedCompleteAt)
	derefTime(&order.CompletedAt, value.CompletedAt)
	return nil
}

type configJSON struct {
	PlateChangeSeconds       float64 `json:"plate_change_seconds"`
	InkChangePerColorSeconds float64 `json:"ink_change_per_color_seconds"`
	DryerRampPerCSeconds     float64 `json:"dryer_ramp_per_c_seconds"`
	DryerMinPreheatSeconds   float64 `json:"dryer_min_preheat_seconds"`
}

func (config Config) MarshalJSON() ([]byte, error) {
	return json.Marshal(configJSON{
		PlateChangeSeconds:       config.PlateChange.Seconds(),
		InkChangePerColorSeconds: config.InkChangePerColor.Seconds(),
		DryerRampPerCSeconds:     config.DryerRampPerC.Seconds(),
		DryerMinPreheatSeconds:   config.DryerMinPreheat.Seconds(),
	})
}

func (config *Config) UnmarshalJSON(data []byte) error {
	var value configJSON
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*config = Config{
		PlateChange:       seconds(value.PlateChangeSeconds),
		InkChangePerColor: seconds(value.InkChangePerColorSeconds),
		DryerRampPerC:     seconds(value.DryerRampPerCSeconds),
		DryerMinPreheat:   seconds(value.DryerMinPreheatSeconds),
	}
	return nil
}

type plannedOrderJSON struct {
	OrderID              string      `json:"order_id"`
	Sequence             int         `json:"sequence"`
	Status               OrderStatus `json:"status"`
	Plate                string      `json:"plate"`
	Colors               []string    `json:"colors"`
	DryerTargetTemp      float64     `json:"dryer_target_temp"`
	PrintDurationSeconds float64     `json:"print_duration_seconds"`
	Start                time.Time   `json:"start"`
	PressStart           time.Time   `json:"press_start"`
	End                  time.Time   `json:"end"`
	DueAt                time.Time   `json:"due_at"`
	OnTime               bool        `json:"on_time"`
	LatenessSeconds      float64     `json:"lateness_seconds"`
	Steps                []Step      `json:"steps"`
	Frozen               bool        `json:"frozen"`
}

func (entry PlannedOrder) MarshalJSON() ([]byte, error) {
	return json.Marshal(plannedOrderJSON{
		OrderID:              entry.OrderID,
		Sequence:             entry.Sequence,
		Status:               entry.Status,
		Plate:                entry.Plate,
		Colors:               entry.Colors,
		DryerTargetTemp:      entry.DryerTargetTemp,
		PrintDurationSeconds: entry.PrintDuration.Seconds(),
		Start:                entry.Start,
		PressStart:           entry.PressStart,
		End:                  entry.End,
		DueAt:                entry.DueAt,
		OnTime:               entry.OnTime,
		LatenessSeconds:      entry.Lateness.Seconds(),
		Steps:                entry.Steps,
		Frozen:               entry.Frozen,
	})
}

func (entry *PlannedOrder) UnmarshalJSON(data []byte) error {
	var value plannedOrderJSON
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*entry = PlannedOrder{
		OrderID:         value.OrderID,
		Sequence:        value.Sequence,
		Status:          value.Status,
		Plate:           value.Plate,
		Colors:          value.Colors,
		DryerTargetTemp: value.DryerTargetTemp,
		PrintDuration:   seconds(value.PrintDurationSeconds),
		Start:           value.Start,
		PressStart:      value.PressStart,
		End:             value.End,
		DueAt:           value.DueAt,
		OnTime:          value.OnTime,
		Lateness:        seconds(value.LatenessSeconds),
		Steps:           value.Steps,
		Frozen:          value.Frozen,
	}
	return nil
}

type impactJSON struct {
	OrderID          string     `json:"order_id"`
	DueAt            time.Time  `json:"due_at"`
	PreviousEnd      *time.Time `json:"previous_end,omitempty"`
	ScheduledEnd     time.Time  `json:"scheduled_end"`
	DeltaSeconds     float64    `json:"delta_seconds"`
	PreviouslyOnTime bool       `json:"previously_on_time"`
	OnTime           bool       `json:"on_time"`
	Reasons          []string   `json:"reasons"`
}

func (impact Impact) MarshalJSON() ([]byte, error) {
	return json.Marshal(impactJSON{
		OrderID:          impact.OrderID,
		DueAt:            impact.DueAt,
		PreviousEnd:      impact.PreviousEnd,
		ScheduledEnd:     impact.ScheduledEnd,
		DeltaSeconds:     impact.Delta.Seconds(),
		PreviouslyOnTime: impact.PreviouslyOnTime,
		OnTime:           impact.OnTime,
		Reasons:          impact.Reasons,
	})
}

func (impact *Impact) UnmarshalJSON(data []byte) error {
	var value impactJSON
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*impact = Impact{
		OrderID:          value.OrderID,
		DueAt:            value.DueAt,
		PreviousEnd:      value.PreviousEnd,
		ScheduledEnd:     value.ScheduledEnd,
		Delta:            seconds(value.DeltaSeconds),
		PreviouslyOnTime: value.PreviouslyOnTime,
		OnTime:           value.OnTime,
		Reasons:          value.Reasons,
	}
	return nil
}

func seconds(value float64) time.Duration {
	return time.Duration(value * float64(time.Second))
}

func pointerTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

func derefTime(target *time.Time, value *time.Time) {
	if value != nil {
		*target = *value
	}
}
