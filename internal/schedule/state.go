package schedule

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// State 是排程模块的并发安全存储：持有全部待产单、停机窗口与最近一版排程。
type State struct {
	mu        sync.Mutex
	config    ChangeoverConfig
	orders    map[string]Order
	downtimes map[string]Downtime
	plan      Plan
	horizon   time.Time
}

// NewState 创建空排程存储。
func NewState(config ChangeoverConfig) *State {
	return &State{config: config, orders: map[string]Order{}, downtimes: map[string]Downtime{}}
}

// Config 返回换线工艺配置。
func (state *State) Config() ChangeoverConfig {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.config
}

// AddOrder 录入一张新单据并立即重排。
func (state *State) AddOrder(order Order, now time.Time) (Plan, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := order.Validate(); err != nil {
		return Plan{}, err
	}
	if _, exists := state.orders[order.ID]; exists {
		return Plan{}, fmt.Errorf("order %s already exists", order.ID)
	}
	state.orders[order.ID] = order
	return state.rebuildLocked(now)
}

// OrderUpdate 携带可修改的单据字段，nil 表示不变。
type OrderUpdate struct {
	Name      *string
	DueAt     *time.Time
	Plate     *string
	Colors    *[]string
	Dryer     *DryerConfig
	Durations *ProcessDurations
	Priority  *int
}

// UpdateOrder 修改未开工单据的工艺输入；已开工/完工单据拒绝修改，只能先取消。
func (state *State) UpdateOrder(id string, update OrderUpdate, now time.Time) (Plan, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	order, ok := state.orders[id]
	if !ok {
		return Plan{}, fmt.Errorf("order %s does not exist", id)
	}
	if order.Frozen() {
		return Plan{}, fmt.Errorf("order %s is %s and can no longer be rescheduled", id, order.State)
	}
	if update.Name != nil {
		order.Name = *update.Name
	}
	if update.DueAt != nil {
		order.DueAt = update.DueAt.UTC()
	}
	if update.Plate != nil {
		order.Plate = *update.Plate
	}
	if update.Colors != nil {
		order.Colors = normalizeColors(*update.Colors)
	}
	if update.Dryer != nil {
		order.Dryer = *update.Dryer
	}
	if update.Durations != nil {
		order.Durations = *update.Durations
	}
	if update.Priority != nil {
		order.Priority = *update.Priority
	}
	order.Generation++
	order.UpdatedAt = now.UTC()
	if err := order.Validate(); err != nil {
		return Plan{}, err
	}
	state.orders[id] = order
	return state.rebuildLocked(now)
}

// StartOrder 按当前排程冻结一张单据的窗口，之后重排不再移动它。
func (state *State) StartOrder(id string, now time.Time) (Plan, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	order, ok := state.orders[id]
	if !ok {
		return Plan{}, fmt.Errorf("order %s does not exist", id)
	}
	updated, err := order.MarkStarted(now)
	if err != nil {
		return Plan{}, err
	}
	state.orders[id] = updated
	return state.rebuildLocked(now)
}

// CompleteOrder 将单据标记完工；完工单据继续占用它之前的窗口作为后续锚点。
func (state *State) CompleteOrder(id string, now time.Time) (Plan, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	order, ok := state.orders[id]
	if !ok {
		return Plan{}, fmt.Errorf("order %s does not exist", id)
	}
	updated, err := order.MarkComplete(now)
	if err != nil {
		return Plan{}, err
	}
	state.orders[id] = updated
	return state.rebuildLocked(now)
}

// CancelOrder 取消单据并释放它占用的计划位置。
func (state *State) CancelOrder(id string, now time.Time) (Plan, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	order, ok := state.orders[id]
	if !ok {
		return Plan{}, fmt.Errorf("order %s does not exist", id)
	}
	if order.State == OrderComplete {
		return Plan{}, fmt.Errorf("completed order %s cannot be cancelled", id)
	}
	updated, err := order.MarkCancelled(now)
	if err != nil {
		return Plan{}, err
	}
	state.orders[id] = updated
	return state.rebuildLocked(now)
}

// AddDowntime 登记设备停机窗口并重排（已开工单据不动）。
func (state *State) AddDowntime(downtime Downtime, now time.Time) (Plan, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := downtime.Validate(); err != nil {
		return Plan{}, err
	}
	if _, exists := state.downtimes[downtime.ID]; exists {
		return Plan{}, fmt.Errorf("downtime %s already exists", downtime.ID)
	}
	state.downtimes[downtime.ID] = downtime
	return state.rebuildLocked(now)
}

// RemoveDowntime 撤销一个停机窗口并重排。
func (state *State) RemoveDowntime(id string, now time.Time) (Plan, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if _, ok := state.downtimes[id]; !ok {
		return Plan{}, fmt.Errorf("downtime %s does not exist", id)
	}
	delete(state.downtimes, id)
	return state.rebuildLocked(now)
}

// Rebplan 在外部时钟推进后（如服务启动恢复）重新计算未执行部分。
func (state *State) Rebplan(now time.Time) (Plan, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.rebuildLocked(now)
}

// Plan 返回最近一版排程快照。
func (state *State) Plan() Plan {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.plan
}

// Order 读取单张单据。
func (state *State) Order(id string) (Order, bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	order, ok := state.orders[id]
	return order, ok
}

// Orders 返回全部单据，按创建时间排序。
func (state *State) Orders() []Order {
	state.mu.Lock()
	defer state.mu.Unlock()
	return ordersSorted(state.orders)
}

// Downtimes 返回全部停机窗口，按开始时间排序。
func (state *State) Downtimes() []Downtime {
	state.mu.Lock()
	defer state.mu.Unlock()
	return downtimesSorted(state.downtimes)
}

// rebuildLocked 重算排程：冻结已开工/完工窗口，只重排未执行部分。
func (state *State) rebuildLocked(now time.Time) (Plan, error) {
	now = now.UTC()
	if state.horizon.IsZero() {
		state.horizon = now
	}
	orders := ordersSorted(state.orders)
	for i := range orders {
		if orders[i].State == OrderPlanned {
			touched := orders[i].touch(now)
			state.orders[touched.ID] = touched
			orders[i] = touched
		}
	}
	plan, err := BuildPlan(orders, downtimesSorted(state.downtimes), state.plan.Orders, state.horizon, now, state.config)
	if err != nil {
		return Plan{}, err
	}
	state.plan = plan
	return plan, nil
}

func ordersSorted(values map[string]Order) []Order {
	orders := make([]Order, 0, len(values))
	for _, order := range values {
		orders = append(orders, order)
	}
	sort.Slice(orders, func(i, j int) bool {
		if !orders[i].CreatedAt.Equal(orders[j].CreatedAt) {
			return orders[i].CreatedAt.Before(orders[j].CreatedAt)
		}
		return orders[i].ID < orders[j].ID
	})
	return orders
}

func downtimesSorted(values map[string]Downtime) []Downtime {
	downtimes := make([]Downtime, 0, len(values))
	for _, downtime := range values {
		downtimes = append(downtimes, downtime)
	}
	sort.Slice(downtimes, func(i, j int) bool { return downtimes[i].StartAt.Before(downtimes[j].StartAt) })
	return downtimes
}

type persistedState struct {
	Version   int        `json:"version"`
	Horizon   time.Time  `json:"horizon"`
	Orders    []Order    `json:"orders"`
	Downtimes []Downtime `json:"downtimes"`
	Plan      *Plan      `json:"plan"`
}

// Save 把排程状态原子写入磁盘（临时文件 + rename）。
func (state *State) Save(path string) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	values := persistedState{Version: 1, Horizon: state.horizon, Orders: ordersSorted(state.orders), Downtimes: downtimesSorted(state.downtimes), Plan: &state.plan}
	data, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary := path + ".next"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	file, err := os.Open(temporary)
	if err != nil {
		return err
	}
	if err = file.Sync(); err == nil {
		err = file.Close()
	} else {
		_ = file.Close()
	}
	if err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// Load 从磁盘恢复排程状态，并用恢复出的窗口作为冻结锚点重算未执行部分。
func (state *State) Load(path string, now time.Time) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var values persistedState
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	if values.Version != 1 {
		return fmt.Errorf("unsupported schedule state version %d", values.Version)
	}
	orders := map[string]Order{}
	for _, order := range values.Orders {
		if err := order.Validate(); err != nil {
			return fmt.Errorf("restore order %s: %w", order.ID, err)
		}
		orders[order.ID] = order
	}
	downtimes := map[string]Downtime{}
	for _, downtime := range values.Downtimes {
		if err := downtime.Validate(); err != nil {
			return fmt.Errorf("restore downtime %s: %w", downtime.ID, err)
		}
		downtimes[downtime.ID] = downtime
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	state.orders = orders
	state.downtimes = downtimes
	if values.Plan != nil {
		state.plan = *values.Plan
	}
	state.horizon = values.Horizon
	_, err = state.rebuildLocked(now)
	return err
}
