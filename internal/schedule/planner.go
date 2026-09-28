package schedule

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ChangeoverConfig 配置换线与烘干的工艺时长经验值。
type ChangeoverConfig struct {
	// PlateChange 换一块印版（含拆卸、装版、初对版）所需时长。
	PlateChange time.Duration
	// InkChange 换一套墨色（清洗、排墨、重新上墨）所需基础时长。
	InkChange time.Duration
	// InkPerColor 在基础时长之外，每多换一个色组的增量时长。
	InkPerColor time.Duration
	// ReheatFactor 相邻单据可复用余温时，预热时长相对冷态的折减比例（0~1）。
	ReheatFactor float64
	// DryerTempTolerance 相邻单据目标温差不超过该值（摄氏度）即视为可复用余温。
	DryerTempTolerance float64
}

// DefaultChangeoverConfig 给出车间常用的换线时长假设。
func DefaultChangeoverConfig() ChangeoverConfig {
	return ChangeoverConfig{
		PlateChange:        25 * time.Minute,
		InkChange:          15 * time.Minute,
		InkPerColor:        5 * time.Minute,
		ReheatFactor:       0.4,
		DryerTempTolerance: 10,
	}
}

func (config ChangeoverConfig) validate() error {
	if config.PlateChange < 0 || config.InkChange < 0 || config.InkPerColor < 0 {
		return fmt.Errorf("changeover durations cannot be negative")
	}
	if config.ReheatFactor < 0 || config.ReheatFactor > 1 {
		return fmt.Errorf("reheat factor must be within 0..1")
	}
	if config.DryerTempTolerance < 0 {
		return fmt.Errorf("dryer temperature tolerance cannot be negative")
	}
	return nil
}

// Window 是一个半开时间区间 [Start, End)。
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

func (window Window) duration() time.Duration {
	return window.End.Sub(window.Start)
}

// Changeover 描述两张相邻单据之间的换线安排（首单为冷机启动）。
type Changeover struct {
	// FromOrder 上一张单据 ID；首单冷启动时为空。
	FromOrder string `json:"from_order,omitempty"`
	// ColdStart 表示班前/冷机首次开机。
	ColdStart bool `json:"cold_start"`

	PlateChanged  bool     `json:"plate_changed"`
	InkChanged    bool     `json:"ink_changed"`
	ColorsAdded   []string `json:"colors_added,omitempty"`
	ColorsKept    []string `json:"colors_kept,omitempty"`
	ColorsRemoved []string `json:"colors_removed,omitempty"`

	DryerProfileChanged  bool          `json:"dryer_profile_changed"`
	DryerReusedWasteHeat bool          `json:"dryer_reused_waste_heat"`
	EffectivePreheat     string        `json:"effective_preheat"`
	effectivePreheat     time.Duration `json:"-"`

	PlateWindow   *Window `json:"plate_window,omitempty"`
	InkWindow     *Window `json:"ink_window,omitempty"`
	PreheatWindow *Window `json:"preheat_window,omitempty"`

	plateDuration time.Duration `json:"-"`
	inkDuration   time.Duration `json:"-"`
}

// TotalDuration 换版、换墨串行且与烘干预热并行后，实际占用的换线时长。
func (changeover Changeover) TotalDuration() time.Duration {
	manual := changeover.plateDuration + changeover.inkDuration
	if changeover.effectivePreheat > manual {
		return changeover.effectivePreheat
	}
	return manual
}

// ScheduledOrder 是一张单据在排程结果中的时间安排。
type ScheduledOrder struct {
	OrderID    string        `json:"order_id"`
	Name       string        `json:"name"`
	Position   int           `json:"position"`
	Plate      string        `json:"plate"`
	Colors     []string      `json:"colors"`
	DueAt      time.Time     `json:"due_at"`
	SetupStart time.Time     `json:"setup_start"`
	PrintStart time.Time     `json:"print_start"`
	FinishAt   time.Time     `json:"finish_at"`
	Frozen     bool          `json:"frozen"`
	OnTime     bool          `json:"on_time"`
	Lateness   string        `json:"lateness,omitempty"`
	lateness   time.Duration `json:"-"`
	Changeover *Changeover   `json:"changeover,omitempty"`
}

// DeliveryImpact 解释一张单据的交期相对上一版排程受到了什么影响。
type DeliveryImpact struct {
	OrderID        string     `json:"order_id"`
	Name           string     `json:"name"`
	DueAt          time.Time  `json:"due_at"`
	PreviousFinish *time.Time `json:"previous_finish,omitempty"`
	NewFinish      time.Time  `json:"new_finish"`
	Shift          string     `json:"shift"`
	shift          time.Duration
	BecameLate     bool     `json:"became_late"`
	StillLate      bool     `json:"still_late"`
	Causes         []string `json:"causes"`
}

// Plan 是一次排程的完整结果。
type Plan struct {
	GeneratedAt       time.Time        `json:"generated_at"`
	EarliestStart     time.Time        `json:"earliest_start"`
	Orders            []ScheduledOrder `json:"orders"`
	LateDeliveries    []DeliveryImpact `json:"late_deliveries"`
	ChangedDeliveries []DeliveryImpact `json:"changed_deliveries"`
}

// BuildPlan 依据在制单据与停机窗口，给出不冲突的开工顺序与换线窗口。
// 已开工/已完工单据的窗口冻结不变，只重排其后的未执行部分。
func BuildPlan(orders []Order, downtimes []Downtime, previous []ScheduledOrder, earliest time.Time, now time.Time, config ChangeoverConfig) (Plan, error) {
	if err := config.validate(); err != nil {
		return Plan{}, err
	}
	earliest = earliest.UTC()
	now = now.UTC()

	previousByID := map[string]ScheduledOrder{}
	for _, scheduled := range previous {
		previousByID[scheduled.OrderID] = scheduled
	}

	frozen, pending := splitOrders(orders)
	frozenSchedule := anchorFrozen(frozen, previousByID, now)
	sort.SliceStable(pending, func(i, j int) bool { return orderBefore(pending[i], pending[j]) })

	scheduled := make([]ScheduledOrder, 0, len(orders))
	var anchor time.Time
	var prev *Order
	if len(frozenSchedule) > 0 {
		scheduled = append(scheduled, frozenSchedule...)
		last := frozenSchedule[len(frozenSchedule)-1]
		anchor = last.FinishAt
		prevOrder, _ := findOrder(orders, last.OrderID)
		if anchor.Before(earliest) {
			anchor = earliest
		}
		prev = &prevOrder
	} else {
		anchor = earliest
	}

	for i := range pending {
		next := pending[i]
		changeover := deriveChangeover(prev, &next, config)
		setupStart := scheduleAroundDowntime(anchor, changeover.TotalDuration(), next.Durations.total(), downtimes)
		entry := buildScheduled(next, len(scheduled)+1, setupStart, changeover)
		scheduled = append(scheduled, entry)
		anchor = entry.FinishAt
		prev = &next
	}

	plan := Plan{GeneratedAt: now, EarliestStart: earliest, Orders: scheduled}
	plan.LateDeliveries = explainLateDeliveries(scheduled, downtimes)
	plan.ChangedDeliveries = explainChanges(previous, scheduled, orders, downtimes, now)
	return plan, nil
}

// splitOrders 把单据分成已冻结（开工/完工）与待排两部分，同时剔除取消单。
func splitOrders(orders []Order) ([]Order, []Order) {
	frozen := make([]Order, 0)
	pending := make([]Order, 0)
	for _, value := range orders {
		switch value.State {
		case OrderCancelled, OrderComplete:
			if value.State == OrderComplete {
				frozen = append(frozen, value)
			}
		case OrderStarted:
			frozen = append(frozen, value)
		default:
			pending = append(pending, value)
		}
	}
	return frozen, pending
}

// anchorFrozen 复用上一版排程中已冻结单据的窗口；缺失记录时无法定位，直接报错。
func anchorFrozen(frozen []Order, previous map[string]ScheduledOrder, now time.Time) []ScheduledOrder {
	scheduled := make([]ScheduledOrder, 0, len(frozen))
	for _, order := range frozen {
		existing, ok := previous[order.ID]
		if !ok {
			// 已完工但没有历史窗口的单据不充当锚点（例如恢复后的旧数据）。
			if order.State == OrderComplete && order.CompletedAt != nil {
				continue
			}
			// 已开工却没有窗口记录：以开工时刻为起点现场补排，并保持冻结语义。
			start := now
			if order.StartedAt != nil {
				start = *order.StartedAt
			}
			existing = ScheduledOrder{OrderID: order.ID, Name: order.Name, Position: len(scheduled) + 1, Plate: order.Plate, Colors: order.Colors, DueAt: order.DueAt, SetupStart: start, PrintStart: start.Add(order.Durations.Setup), FinishAt: start.Add(order.Durations.total())}
		}
		existing.Frozen = true
		existing.OnTime = !existing.FinishAt.After(order.DueAt)
		if !existing.OnTime {
			existing.lateness = existing.FinishAt.Sub(order.DueAt)
			existing.Lateness = existing.lateness.String()
		}
		existing.Changeover = nil
		scheduled = append(scheduled, existing)
	}
	sort.SliceStable(scheduled, func(i, j int) bool { return scheduled[i].SetupStart.Before(scheduled[j].SetupStart) })
	for i := range scheduled {
		scheduled[i].Position = i + 1
	}
	return scheduled
}

// orderBefore 实现 EDD 规则：交期早优先，交期相同则优先级高（数值大）先，再按创建时间与 ID 兜底。
func orderBefore(left, right Order) bool {
	if !left.DueAt.Equal(right.DueAt) {
		return left.DueAt.Before(right.DueAt)
	}
	if left.Priority != right.Priority {
		return left.Priority > right.Priority
	}
	if !left.CreatedAt.Equal(right.CreatedAt) {
		return left.CreatedAt.Before(right.CreatedAt)
	}
	return left.ID < right.ID
}

func findOrder(orders []Order, id string) (Order, bool) {
	for _, value := range orders {
		if value.ID == id {
			return value, true
		}
	}
	return Order{}, false
}

// deriveChangeover 依据相邻两张单据的版、色、烘干配置推导换线窗口内容。
func deriveChangeover(prev, next *Order, config ChangeoverConfig) *Changeover {
	changeover := &Changeover{ColorsAdded: []string{}, ColorsKept: []string{}, ColorsRemoved: []string{}}
	if prev == nil {
		changeover.ColdStart = true
		changeover.PlateChanged = true
		changeover.InkChanged = true
		changeover.ColorsAdded = append([]string{}, next.Colors...)
		changeover.plateDuration = config.PlateChange
		changeover.inkDuration = inkDuration(config, len(next.Colors))
		changeover.effectivePreheat = next.Dryer.Preheat
		changeover.EffectivePreheat = changeover.effectivePreheat.String()
		return changeover
	}

	changeover.FromOrder = prev.ID
	changeover.PlateChanged = prev.Plate != next.Plate
	if changeover.PlateChanged {
		changeover.plateDuration = config.PlateChange
	}

	prevColors := map[string]bool{}
	for _, color := range prev.Colors {
		prevColors[color] = true
	}
	nextColors := map[string]bool{}
	for _, color := range next.Colors {
		nextColors[color] = true
	}
	for _, color := range next.Colors {
		if prevColors[color] {
			changeover.ColorsKept = append(changeover.ColorsKept, color)
		} else {
			changeover.ColorsAdded = append(changeover.ColorsAdded, color)
		}
	}
	for _, color := range prev.Colors {
		if !nextColors[color] {
			changeover.ColorsRemoved = append(changeover.ColorsRemoved, color)
		}
	}
	sort.Strings(changeover.ColorsAdded)
	sort.Strings(changeover.ColorsKept)
	sort.Strings(changeover.ColorsRemoved)
	changeover.InkChanged = len(changeover.ColorsAdded) > 0 || len(changeover.ColorsRemoved) > 0
	if changeover.InkChanged {
		changedCount := len(changeover.ColorsAdded) + len(changeover.ColorsRemoved)
		changeover.inkDuration = inkDuration(config, changedCount)
	}

	changeover.DryerProfileChanged = prev.Dryer.Profile != next.Dryer.Profile
	tempDelta := prev.Dryer.TargetTemp - next.Dryer.TargetTemp
	if tempDelta < 0 {
		tempDelta = -tempDelta
	}

	reused := !changeover.DryerProfileChanged && tempDelta <= config.DryerTempTolerance
	changeover.DryerReusedWasteHeat = reused
	changeover.effectivePreheat = next.Dryer.Preheat
	if reused {
		changeover.effectivePreheat = scaleDuration(next.Dryer.Preheat, config.ReheatFactor)
	}
	changeover.EffectivePreheat = changeover.effectivePreheat.String()
	return changeover
}

func inkDuration(config ChangeoverConfig, changedColors int) time.Duration {
	if changedColors <= 0 {
		return 0
	}
	return config.InkChange + config.InkPerColor*time.Duration(changedColors-1)
}

func scaleDuration(value time.Duration, factor float64) time.Duration {
	return time.Duration(float64(value) * factor)
}

// scheduleAroundDowntime 保证生产段（上机准备到收尾完工）不与任何停机窗口相交；
// 换版、换墨、预热允许安排在停机期间并行完成。
func scheduleAroundDowntime(anchor time.Time, gap, total time.Duration, downtimes []Downtime) time.Time {
	candidate := anchor
	for {
		ready := candidate
		if limit := anchor.Add(gap); ready.Before(limit) {
			ready = limit
		}
		finish := ready.Add(total)
		clash := firstClashingDowntime(ready, finish, downtimes)
		if clash == nil {
			return ready
		}
		candidate = clash.EndAt
	}
}

func firstClashingDowntime(start, end time.Time, downtimes []Downtime) *Downtime {
	sorted := append([]Downtime(nil), downtimes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].StartAt.Before(sorted[j].StartAt) })
	for i := range sorted {
		if sorted[i].overlaps(start, end) {
			return &sorted[i]
		}
	}
	return nil
}

func buildScheduled(order Order, position int, setupStart time.Time, changeover *Changeover) ScheduledOrder {
	printStart := setupStart.Add(order.Durations.Setup)
	finishAt := printStart.Add(order.Durations.Printing).Add(order.Durations.Finishing)

	if changeover != nil {
		changeStart := setupStart.Add(-changeover.TotalDuration())
		cursor := changeStart
		if changeover.PlateChanged {
			changeover.PlateWindow = &Window{Start: cursor, End: cursor.Add(changeover.plateDuration)}
			cursor = changeover.PlateWindow.End
		}
		if changeover.InkChanged {
			changeover.InkWindow = &Window{Start: cursor, End: cursor.Add(changeover.inkDuration)}
		}
		if changeover.effectivePreheat > 0 {
			changeover.PreheatWindow = &Window{Start: printStart.Add(-changeover.effectivePreheat), End: printStart}
		}
	}

	scheduled := ScheduledOrder{
		OrderID:    order.ID,
		Name:       order.Name,
		Position:   position,
		Plate:      order.Plate,
		Colors:     append([]string{}, order.Colors...),
		DueAt:      order.DueAt,
		SetupStart: setupStart,
		PrintStart: printStart,
		FinishAt:   finishAt,
		Changeover: changeover,
	}
	scheduled.OnTime = !finishAt.After(order.DueAt)
	if !scheduled.OnTime {
		scheduled.lateness = finishAt.Sub(order.DueAt)
		scheduled.Lateness = scheduled.lateness.String()
	}
	return scheduled
}

// explainLateDeliveries 给出当前排程下每张逾期单的现场可读原因。
func explainLateDeliveries(scheduled []ScheduledOrder, downtimes []Downtime) []DeliveryImpact {
	impacts := []DeliveryImpact{}
	for index := range scheduled {
		entry := scheduled[index]
		if entry.OnTime {
			continue
		}
		causes := []string{}
		if index == 0 && entry.Changeover != nil && entry.Changeover.ColdStart {
			causes = append(causes, fmt.Sprintf("冷机启动需换版、换墨并完成 %s 烘干预热", entry.Changeover.EffectivePreheat))
		}
		causes = append(causes, fmt.Sprintf("前面还有 %d 张在制/待产单，排队累计占用到 %s", index, entry.SetupStart.Format("01-02 15:04")))
		if dt := downtimeEndingAt(entry.SetupStart, downtimes); dt != nil {
			causes = append(causes, fmt.Sprintf("避让设备停机「%s」（%s ~ %s），顺延至停机结束后开工", dt.Reason, dt.StartAt.Format("01-02 15:04"), dt.EndAt.Format("15:04")))
		}
		impacts = append(impacts, DeliveryImpact{
			OrderID:   entry.OrderID,
			Name:      entry.Name,
			DueAt:     entry.DueAt,
			NewFinish: entry.FinishAt,
			Shift:     entry.Lateness,
			shift:     entry.lateness,
			StillLate: true,
			Causes:    causes,
		})
	}
	return impacts
}

// downtimeEndingAt 报告生产段是否恰好被推到某段停机结束之后开工。
func downtimeEndingAt(start time.Time, downtimes []Downtime) *Downtime {
	sorted := append([]Downtime(nil), downtimes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].EndAt.After(sorted[j].EndAt) })
	for i := range sorted {
		if sorted[i].EndAt.Equal(start) && sorted[i].StartAt.Before(start) {
			return &sorted[i]
		}
	}
	return nil
}

// explainChanges 对比新旧两版排程，解释每张单据交期受到的影响及原因。
func explainChanges(previous, current []ScheduledOrder, orders []Order, downtimes []Downtime, now time.Time) []DeliveryImpact {
	oldByID := map[string]ScheduledOrder{}
	oldIndex := map[string]int{}
	for i, entry := range previous {
		oldByID[entry.OrderID] = entry
		oldIndex[entry.OrderID] = i
	}
	prevOrderByID := map[string]Order{}
	for _, order := range orders {
		prevOrderByID[order.ID] = order
	}

	impacts := []DeliveryImpact{}
	for newIndex := range current {
		entry := current[newIndex]
		old, existed := oldByID[entry.OrderID]
		if existed && old.FinishAt.Equal(entry.FinishAt) {
			continue
		}
		impact := DeliveryImpact{
			OrderID:   entry.OrderID,
			Name:      entry.Name,
			DueAt:     entry.DueAt,
			NewFinish: entry.FinishAt,
			Causes:    []string{},
		}
		if existed {
			previousFinish := old.FinishAt
			impact.PreviousFinish = &previousFinish
			impact.shift = entry.FinishAt.Sub(old.FinishAt)
			impact.Shift = impact.shift.String()
			impact.BecameLate = old.OnTime && !entry.OnTime
			impact.StillLate = !old.OnTime && !entry.OnTime
		} else {
			impact.Shift = "new"
		}

		// 原因 1：有单据新插入或被提前到它前面。
		for j := 0; j < newIndex; j++ {
			ahead := current[j]
			oldPos, wasBefore := oldIndex[ahead.OrderID]
			if !wasBefore || oldPos > newIndex {
				verb := "新插入"
				if wasBefore {
					verb = "交期/优先级调整后提前"
				}
				impact.Causes = append(impact.Causes, fmt.Sprintf("单据「%s」（交期 %s）%s到第 %d 位", ahead.Name, ahead.DueAt.Format("01-02 15:04"), verb, ahead.Position))
			}
		}
		// 原因 2：设备停机把生产段推迟到停机结束后。
		if existed && impact.shift > 0 {
			if dt := downtimeEndingAt(entry.SetupStart, downtimes); dt != nil {
				impact.Causes = append(impact.Causes, fmt.Sprintf("新增/变更设备停机「%s」（%s ~ %s），生产段顺延", dt.Reason, dt.StartAt.Format("01-02 15:04"), dt.EndAt.Format("15:04")))
			}
		}
		// 原因 3：自身工艺参数变更。
		if order, ok := prevOrderByID[entry.OrderID]; ok && existed {
			selfNotes := []string{}
			if old, hasOld := oldByID[order.ID]; hasOld {
				oldTotal := old.FinishAt.Sub(old.PrintStart)
				newTotal := order.Durations.total() - order.Durations.Setup
				if newTotal > oldTotal {
					selfNotes = append(selfNotes, fmt.Sprintf("工序时长增加 %s", newTotal-oldTotal))
				}
			}
			if len(selfNotes) > 0 {
				impact.Causes = append(impact.Causes, "本单变更："+strings.Join(selfNotes, "、"))
			}
		}
		// 原因 4：纯顺延链。
		if len(impact.Causes) == 0 && impact.shift > 0 {
			impact.Causes = append(impact.Causes, "前序换版/换墨/烘干预热窗口后移，顺延开工")
		}
		if impact.BecameLate {
			impact.Causes = append(impact.Causes, fmt.Sprintf("预计 %s 完工，晚于交期 %s，逾期 %s", entry.FinishAt.Format("01-02 15:04"), entry.DueAt.Format("01-02 15:04"), entry.Lateness))
		}
		if len(impact.Causes) > 0 || impact.BecameLate || impact.StillLate {
			impacts = append(impacts, impact)
		}
	}
	return impacts
}
