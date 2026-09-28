package schedule

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// OrderState 描述一张印刷单在排程中的生命周期。
type OrderState string

const (
	// OrderPlanned 已录入、尚未排产。
	OrderPlanned OrderState = "planned"
	// OrderScheduled 已排入计划（已算出开工顺序与窗口）。
	OrderScheduled OrderState = "scheduled"
	// OrderStarted 已开工：此后窗口冻结，单据/停机变更不再移动它。
	OrderStarted OrderState = "started"
	// OrderComplete 已完工。
	OrderComplete OrderState = "complete"
	// OrderCancelled 已取消，不参与排程。
	OrderCancelled OrderState = "cancelled"
)

var orderTransitions = map[OrderState]map[OrderState]bool{
	OrderPlanned:   {OrderScheduled: true, OrderStarted: true, OrderCancelled: true},
	OrderScheduled: {OrderStarted: true, OrderCancelled: true, OrderPlanned: true},
	OrderStarted:   {OrderComplete: true, OrderCancelled: true},
	OrderComplete:  {},
	OrderCancelled: {},
}

// ProcessDurations 汇总一张单子在机器上需要的各段时长。
type ProcessDurations struct {
	// Setup 装版以外的上机准备（上纸、穿料等）。
	Setup time.Duration `json:"setup"`
	// Printing 正式印刷时长。
	Printing time.Duration `json:"printing"`
	// Finishing 收尾/折叠输出时长。
	Finishing time.Duration `json:"finishing"`
}

func (value ProcessDurations) total() time.Duration {
	return value.Setup + value.Printing + value.Finishing
}

func (value ProcessDurations) validate() error {
	if value.Setup < 0 || value.Printing < 0 || value.Finishing < 0 {
		return errors.New("process durations cannot be negative")
	}
	if value.Printing == 0 {
		return errors.New("printing duration is required")
	}
	return nil
}

// DryerConfig 描述该单所需的烘干配置。
type DryerConfig struct {
	// Profile 烘干曲线名称（与 dryer.Profile.Name 对应，如 production）。
	Profile string `json:"profile"`
	// TargetTemp 目标最高温度（摄氏度），用于判断相邻单据能否复用余温。
	TargetTemp float64 `json:"target_temp"`
	// Preheat 从冷态达到目标温度所需的预热时长。
	Preheat time.Duration `json:"preheat"`
}

func (value DryerConfig) validate() error {
	value.Profile = strings.TrimSpace(value.Profile)
	if value.Profile == "" {
		return errors.New("dryer profile is required")
	}
	if value.TargetTemp <= 0 {
		return errors.New("dryer target temperature must be positive")
	}
	if value.Preheat < 0 {
		return errors.New("dryer preheat cannot be negative")
	}
	return nil
}

// Order 是一张待产印刷单及其排程所需的全部工艺输入。
type Order struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	State       OrderState       `json:"state"`
	DueAt       time.Time        `json:"due_at"`
	Plate       string           `json:"plate"`
	Colors      []string         `json:"colors"`
	Dryer       DryerConfig      `json:"dryer"`
	Durations   ProcessDurations `json:"durations"`
	Priority    int              `json:"priority"`
	Generation  uint64           `json:"generation"`
	StartedAt   *time.Time       `json:"started_at,omitempty"`
	CompletedAt *time.Time       `json:"completed_at,omitempty"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
}

// NewOrder 生成一张待排产印刷单。
func NewOrder(name string, dueAt time.Time, plate string, colors []string, dryerCfg DryerConfig, durations ProcessDurations, priority int, now time.Time) (Order, error) {
	order := Order{
		ID:         uuid.NewString(),
		Name:       strings.TrimSpace(name),
		State:      OrderPlanned,
		DueAt:      dueAt.UTC(),
		Plate:      strings.TrimSpace(plate),
		Colors:     normalizeColors(colors),
		Dryer:      dryerCfg,
		Durations:  durations,
		Priority:   priority,
		Generation: 1,
		CreatedAt:  now.UTC(),
		UpdatedAt:  now.UTC(),
	}
	if err := order.Validate(); err != nil {
		return Order{}, err
	}
	return order, nil
}

// Validate 校验单据工艺输入是否完整。
func (order Order) Validate() error {
	if _, err := uuid.Parse(order.ID); err != nil {
		return fmt.Errorf("order id is invalid: %w", err)
	}
	if order.Name == "" {
		return errors.New("order name is required")
	}
	if order.DueAt.IsZero() {
		return errors.New("order due date is required")
	}
	if order.Plate == "" {
		return errors.New("plate identity is required")
	}
	if len(order.Colors) == 0 {
		return errors.New("at least one ink color is required")
	}
	for _, color := range order.Colors {
		if strings.TrimSpace(color) == "" {
			return errors.New("ink color cannot be blank")
		}
	}
	if err := order.Dryer.validate(); err != nil {
		return err
	}
	if err := order.Durations.validate(); err != nil {
		return err
	}
	if _, ok := orderTransitions[order.State]; !ok {
		return fmt.Errorf("order state %q is invalid", order.State)
	}
	if order.Generation == 0 {
		return errors.New("order generation is required")
	}
	return nil
}

func (order Order) transition(next OrderState, now time.Time) (Order, error) {
	if !orderTransitions[order.State][next] {
		return order, fmt.Errorf("cannot transition order %s from %s to %s", order.ID, order.State, next)
	}
	order.State = next
	order.Generation++
	order.UpdatedAt = now.UTC()
	return order, nil
}

// MarkStarted 将单据标记为已开工，并记录实际开工时刻。
func (order Order) MarkStarted(now time.Time) (Order, error) {
	updated, err := order.transition(OrderStarted, now)
	if err != nil {
		return updated, err
	}
	stamped := now.UTC()
	updated.StartedAt = &stamped
	return updated, nil
}

// MarkComplete 将单据标记为完工。
func (order Order) MarkComplete(now time.Time) (Order, error) {
	updated, err := order.transition(OrderComplete, now)
	if err != nil {
		return updated, err
	}
	stamped := now.UTC()
	updated.CompletedAt = &stamped
	return updated, nil
}

// MarkCancelled 取消单据。
func (order Order) MarkCancelled(now time.Time) (Order, error) {
	return order.transition(OrderCancelled, now)
}

// MarkScheduled 在重排后刷新版本号（窗口内容以 Plan 为准单独持久化）。
func (order Order) touch(now time.Time) Order {
	if order.State == OrderPlanned {
		order.State = OrderScheduled
	}
	order.UpdatedAt = now.UTC()
	return order
}

// Frozen 报告单据是否已开工/完工，其计划窗口不允许再被重排移动。
func (order Order) Frozen() bool {
	return order.State == OrderStarted || order.State == OrderComplete
}

func normalizeColors(colors []string) []string {
	seen := map[string]bool{}
	values := make([]string, 0, len(colors))
	for _, color := range colors {
		color = strings.ToLower(strings.TrimSpace(color))
		if color == "" || seen[color] {
			continue
		}
		seen[color] = true
		values = append(values, color)
	}
	return values
}

// Downtime 是设备停机/保养窗口：该时段机器不可用，排程必须绕行。
type Downtime struct {
	ID        string    `json:"id"`
	Reason    string    `json:"reason"`
	StartAt   time.Time `json:"start_at"`
	EndAt     time.Time `json:"end_at"`
	CreatedAt time.Time `json:"created_at"`
}

// NewDowntime 录入一个设备停机窗口。
func NewDowntime(reason string, startAt, endAt time.Time, now time.Time) (Downtime, error) {
	downtime := Downtime{
		ID:        uuid.NewString(),
		Reason:    strings.TrimSpace(reason),
		StartAt:   startAt.UTC(),
		EndAt:     endAt.UTC(),
		CreatedAt: now.UTC(),
	}
	if err := downtime.Validate(); err != nil {
		return Downtime{}, err
	}
	return downtime, nil
}

// Validate 校验停机窗口。
func (downtime Downtime) Validate() error {
	if _, err := uuid.Parse(downtime.ID); err != nil {
		return fmt.Errorf("downtime id is invalid: %w", err)
	}
	if downtime.StartAt.IsZero() || downtime.EndAt.IsZero() {
		return errors.New("downtime time range is required")
	}
	if !downtime.EndAt.After(downtime.StartAt) {
		return errors.New("downtime must end after it starts")
	}
	return nil
}

// overlaps 判断 [start,end) 是否与停机窗口相交。
func (downtime Downtime) overlaps(start, end time.Time) bool {
	return start.Before(downtime.EndAt) && downtime.StartAt.Before(end)
}
