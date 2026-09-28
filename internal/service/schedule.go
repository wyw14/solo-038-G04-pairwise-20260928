package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/wyw14/solo-038/internal/schedule"
)

// Duration 在 JSON 上支持 "30m" / "2h45m" 这样的时长字符串，也接受纳秒数字。
type Duration time.Duration

func (duration Duration) Duration() time.Duration { return time.Duration(duration) }

func (duration Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(duration).String())
}

func (duration *Duration) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		parsed, err := time.ParseDuration(text)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", text, err)
		}
		*duration = Duration(parsed)
		return nil
	}
	var nanos int64
	if err := json.Unmarshal(data, &nanos); err != nil {
		return errors.New("duration must be a string like 30m or a nanosecond number")
	}
	*duration = Duration(nanos)
	return nil
}

// OrderRequest 是录入一张印刷单的请求体。
type OrderRequest struct {
	Name            string    `json:"name"`
	DueAt           time.Time `json:"due_at"`
	Plate           string    `json:"plate"`
	Colors          []string  `json:"colors"`
	DryerProfile    string    `json:"dryer_profile"`
	DryerTargetTemp float64   `json:"dryer_target_temp"`
	DryerPreheat    Duration  `json:"dryer_preheat"`
	Setup           Duration  `json:"setup"`
	Printing        Duration  `json:"printing"`
	Finishing       Duration  `json:"finishing"`
	Priority        int       `json:"priority"`
}

func (request OrderRequest) toOrder(now time.Time) (schedule.Order, error) {
	return schedule.NewOrder(
		request.Name,
		request.DueAt,
		request.Plate,
		request.Colors,
		schedule.DryerConfig{Profile: request.DryerProfile, TargetTemp: request.DryerTargetTemp, Preheat: request.DryerPreheat.Duration()},
		schedule.ProcessDurations{Setup: request.Setup.Duration(), Printing: request.Printing.Duration(), Finishing: request.Finishing.Duration()},
		request.Priority,
		now,
	)
}

// OrderUpdateRequest 的每个字段为空指针表示不变；烘干字段任一给出即整体替换烘干配置。
type OrderUpdateRequest struct {
	Name            *string    `json:"name"`
	DueAt           *time.Time `json:"due_at"`
	Plate           *string    `json:"plate"`
	Colors          *[]string  `json:"colors"`
	DryerProfile    *string    `json:"dryer_profile"`
	DryerTargetTemp *float64   `json:"dryer_target_temp"`
	DryerPreheat    *Duration  `json:"dryer_preheat"`
	Setup           *Duration  `json:"setup"`
	Printing        *Duration  `json:"printing"`
	Finishing       *Duration  `json:"finishing"`
	Priority        *int       `json:"priority"`
}

// DowntimeRequest 录入设备停机窗口。
type DowntimeRequest struct {
	Reason  string    `json:"reason"`
	StartAt time.Time `json:"start_at"`
	EndAt   time.Time `json:"end_at"`
}

func (runtime *Runtime) schedulerPath() string {
	return filepath.Join(runtime.config.DataDir, "schedule.json")
}

func (runtime *Runtime) saveSchedule() error {
	return runtime.scheduler.Save(runtime.schedulerPath())
}

// SchedulePlan 返回当前排程（开工顺序、换线窗口、交期影响）。
func (runtime *Runtime) SchedulePlan() schedule.Plan {
	return runtime.scheduler.Plan()
}

// ScheduleOrders 返回已录入的全部印刷单。
func (runtime *Runtime) ScheduleOrders() []schedule.Order {
	return runtime.scheduler.Orders()
}

// ScheduleDowntimes 返回已登记的设备停机窗口。
func (runtime *Runtime) ScheduleDowntimes() []schedule.Downtime {
	return runtime.scheduler.Downtimes()
}

// CreateScheduleOrder 录入新印刷单并重排。
func (runtime *Runtime) CreateScheduleOrder(request OrderRequest) (schedule.Plan, error) {
	order, err := request.toOrder(time.Now())
	if err != nil {
		return schedule.Plan{}, err
	}
	plan, err := runtime.scheduler.AddOrder(order, time.Now())
	if err != nil {
		return schedule.Plan{}, err
	}
	return plan, runtime.saveSchedule()
}

// UpdateScheduleOrder 修改未开工单据并重排。
func (runtime *Runtime) UpdateScheduleOrder(id string, request OrderUpdateRequest) (schedule.Plan, error) {
	update := schedule.OrderUpdate{
		Name:     request.Name,
		DueAt:    request.DueAt,
		Plate:    request.Plate,
		Colors:   request.Colors,
		Priority: request.Priority,
	}
	if request.Setup != nil || request.Printing != nil || request.Finishing != nil {
		current, ok := runtime.scheduler.Order(id)
		if !ok {
			return schedule.Plan{}, fmt.Errorf("order %s does not exist", id)
		}
		durations := current.Durations
		if request.Setup != nil {
			durations.Setup = request.Setup.Duration()
		}
		if request.Printing != nil {
			durations.Printing = request.Printing.Duration()
		}
		if request.Finishing != nil {
			durations.Finishing = request.Finishing.Duration()
		}
		update.Durations = &durations
	}
	if request.DryerProfile != nil || request.DryerTargetTemp != nil || request.DryerPreheat != nil {
		current, ok := runtime.scheduler.Order(id)
		if !ok {
			return schedule.Plan{}, fmt.Errorf("order %s does not exist", id)
		}
		dryerCfg := current.Dryer
		if request.DryerProfile != nil {
			dryerCfg.Profile = *request.DryerProfile
		}
		if request.DryerTargetTemp != nil {
			dryerCfg.TargetTemp = *request.DryerTargetTemp
		}
		if request.DryerPreheat != nil {
			dryerCfg.Preheat = request.DryerPreheat.Duration()
		}
		update.Dryer = &dryerCfg
	}
	plan, err := runtime.scheduler.UpdateOrder(id, update, time.Now())
	if err != nil {
		return schedule.Plan{}, err
	}
	return plan, runtime.saveSchedule()
}

// StartScheduleOrder 冻结单据窗口（按计划开工）。
func (runtime *Runtime) StartScheduleOrder(id string) (schedule.Plan, error) {
	plan, err := runtime.scheduler.StartOrder(id, time.Now())
	if err != nil {
		return schedule.Plan{}, err
	}
	return plan, runtime.saveSchedule()
}

// CompleteScheduleOrder 标记单据完工。
func (runtime *Runtime) CompleteScheduleOrder(id string) (schedule.Plan, error) {
	plan, err := runtime.scheduler.CompleteOrder(id, time.Now())
	if err != nil {
		return schedule.Plan{}, err
	}
	return plan, runtime.saveSchedule()
}

// CancelScheduleOrder 取消单据并重排。
func (runtime *Runtime) CancelScheduleOrder(id string) (schedule.Plan, error) {
	plan, err := runtime.scheduler.CancelOrder(id, time.Now())
	if err != nil {
		return schedule.Plan{}, err
	}
	return plan, runtime.saveSchedule()
}

// AddScheduleDowntime 登记设备停机并重排（已开工单据不动）。
func (runtime *Runtime) AddScheduleDowntime(request DowntimeRequest) (schedule.Plan, error) {
	downtime, err := schedule.NewDowntime(request.Reason, request.StartAt, request.EndAt, time.Now())
	if err != nil {
		return schedule.Plan{}, err
	}
	plan, err := runtime.scheduler.AddDowntime(downtime, time.Now())
	if err != nil {
		return schedule.Plan{}, err
	}
	return plan, runtime.saveSchedule()
}

// RemoveScheduleDowntime 撤销停机窗口并重排。
func (runtime *Runtime) RemoveScheduleDowntime(id string) (schedule.Plan, error) {
	plan, err := runtime.scheduler.RemoveDowntime(id, time.Now())
	if err != nil {
		return schedule.Plan{}, err
	}
	return plan, runtime.saveSchedule()
}
