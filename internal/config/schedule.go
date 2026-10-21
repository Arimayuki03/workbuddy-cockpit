// Package config 存放跨命令共享的配置段与默认值逻辑。
//
// 起因 issue #49：cmd/activity 一次性触发器曾自复制一份精简 schedule 结构体，
// 只 json.Unmarshal 无默认值，键缺席 → Go 零值 0 → scheduler 归一为 1，
// 与 cmd/server 主程序缺省 5 条漂移。把 Schedule 段 + 默认值/归一化抽到本包，
// 两个命令共用同一份定义，消除漂移源头。
package config

import "fmt"

// 任务奖励补扫周期常量（分钟）。与 internal/panel.MinTaskSweepInterval 的 15m
// 同口径：面板启动函数仍会兜底钳制，配置层提前给出可定位的键名错误。
const (
	DefaultTaskSweepIntervalMinutes = 15
	MinTaskSweepIntervalMinutes     = 15
)

// Schedule 排程配置段（对应 config.json 的 "schedule" 对象）。
//
// 多类独立排程：签到 / 活跃上报 / 猫猫旅行 / token keepalive / 开学季 / 夜猫子 /
// 任务中心执行队列 / 令牌续期 / 活动补偿（最后一类缺省关、小时表默认空且不回落，
// 见 CompensationHours 注释）。cmd/server 与 cmd/activity 共用本结构，默认值由
// DefaultSchedule 填充、缺省归一由 Normalize 完成——两命令走同一份语义，
// 不再各自复制。
type Schedule struct {
	CheckinHours   []int `json:"checkin_hours"`   // [9,21]
	TravelHours    []int `json:"travel_hours"`    // [9,21]
	ActivityHours  []int `json:"activity_hours"`  // [10]
	KeepaliveHours []int `json:"keepalive_hours"` // [22]
	SchoolHours    []int `json:"school_hours"`    // [12] 开学季任务（迁移自 school/cat 两条系统 crontab）
	CatHours       []int `json:"cat_hours"`       // [1] 夜猫窗口 23-08 CST，01:00 窗口内补 1 次
	QueueHours     []int `json:"queue_hours"`     // [10] 任务中心执行队列（成长任务 + 开学季闭环）
	// RenewHours Token 独立续期巡检的触发小时表（renew_enabled=true 时生效，默认 [3]）。
	// 与 keepalive_hours 的差异：保活是「到点全量刷一遍」，续期巡检是「按剩余寿命挑号刷」
	// ——只对临期（pool.expiring_soon 口径）账号主动 refresh，覆盖长期闲置无对话流量、
	// 保活与签到都覆盖不到的空档（wbm services/renew.py 吸收件）。默认凌晨 3 点：
	// 避开签到/活跃/任务队列的高峰时点。
	RenewHours []int `json:"renew_hours"`
	// CheckinEnabled/TravelEnabled/ActivityEnabled/KeepaliveEnabled/SchoolEnabled/CatEnabled
	// 显式禁用开关（缺省 true）。
	//
	// 为什么用独立 bool 而不是空数组/哨兵值表意"禁用"：
	//   - 空数组与 null 在老语义里已被"未配置 → 回落默认"占用，改判会静默翻转
	//     所有老 config 的行为（用户只想删掉一行，结果关掉了签到）；bool 缺省 true
	//     则对老配置零影响，向后完全兼容。
	//   - 开关与取值解耦：禁用时仍保留用户显式配的小时，重新启用无需补配。
	//   - 无需猜测哨兵（[-1] 之类），非法小时一律报错并提示改用本开关。
	CheckinEnabled   bool `json:"checkin_enabled"`   // 缺省 true；false = 关签到
	TravelEnabled    bool `json:"travel_enabled"`    // 缺省 true；false = 完全停猫猫旅行
	ActivityEnabled  bool `json:"activity_enabled"`  // 缺省 true；false = 停活跃上报
	KeepaliveEnabled bool `json:"keepalive_enabled"` // 缺省 true；false = 关 token 保活
	SchoolEnabled    bool `json:"school_enabled"`    // 缺省 true；false = 停开学季任务
	CatEnabled       bool `json:"cat_enabled"`       // 缺省 true；false = 停夜猫子任务
	// QueueEnabled 任务中心执行队列排程开关。与上面六个缺省 true 不同：队列对
	// 全账号执行真实任务动作链（含专家/技能真实对话，消耗上游配额），是"一键完成"
	// 的自动化版，不适合无感默认开启——缺省 false，由用户显式打开。
	QueueEnabled bool `json:"queue_enabled"` // 缺省 false；true = 按 queue_hours 到点自动跑执行队列
	// RenewEnabled Token 独立续期巡检排程开关（schedule.renew_enabled）。与 QueueEnabled
	// 同风格缺省 false（opt-in）：续期对临期账号主动打 refresh 写接口（上游凭据轮换），
	// 由用户显式打开；关闭时保活/签到/选号路径的既有刷新行为完全不受影响。
	// 开启后按 renew_hours 触发，仅刷新「有效期不足 pool.expiring_soon 窗口」的账号
	//（面板手动停用的账号跳过；仅熔断/冷却/状态位异常的照常续）。
	RenewEnabled bool `json:"renew_enabled"`
	// CompensationEnabled 活动补偿自动领取开关（schedule.compensation_enabled，
	// 缺省 **false**，opt-in）。补偿是「有则领、没有就跳过」的幂等写（平台发给账号的
	// 故障/活动补偿，官方网页「用量管理」也要用户手点一次），但公告常带截止日期——
	// 面板「立即执行」当场领最稳，故不无感默认开启（与 queue/renew 同风格）。
	CompensationEnabled bool `json:"compensation_enabled"`
	// CompensationHours 活动补偿自动领取的整点表（schedule.compensation_hours）。
	//
	// **默认空数组，且是全表唯一的「空数组不回落默认」例外**：空 = 只手动执行、不自动跑
	//（前端设置页与任务页卡片的「留空 = 只手动执行」文案即此语义）。其余七类小时表沿用
	//「空数组/null = 未配置 → 回落默认」的老语义（见 Normalize），这里若照做，用户留空
	// 会变成「静默每天自动领」——与用户写下的意图相反，且界面上看不出任何提示。
	//
	// enabled=true 但本表为空 → 不排程（等价「只手动」），由 scheduler.New 记一行 WARN
	// 说明；不报错也不回落（用户可能是先开开关、还没填时刻）。
	CompensationHours []int `json:"compensation_hours"`
	// TaskSweepEnabled 任务奖励「只领取」补扫开关（schedule.task_sweep_enabled）。
	// 缺省 **false**（opt-in）：该循环会周期性扫描全池成长任务列表，只对已经
	// 达标（Claimable）的奖励执行 accept/claim，不跑对话、不执行任务动作；
	// 与 queue/compensation 是不同路径，默认不开启以免无感放大上游请求。
	TaskSweepEnabled bool `json:"task_sweep_enabled"`
	// TaskSweepIntervalMinutes 补扫周期（分钟）。缺省/0 = 15 分钟；显式小于
	// 15 分钟在 Normalize 直接报错（不能静默改成别的值）。15 分钟是产品
	// 保守下限：claim/accept 都是幂等轻写，但全账号扫描仍会放大上游请求。
	TaskSweepIntervalMinutes int `json:"task_sweep_interval_minutes"`
	// ActivityReportCount 每号每次活跃上报的条数：领猫前置需 5 次对话，
	// 默认 5 条把 chat_5 刷满；0/缺省=1 兼容旧行为。
	ActivityReportCount int `json:"activity_report_count"`
	// RandomDelayMinutes 每日随机延后窗口（分钟，0-60，缺省 0 = 关）。
	//
	// 开启后每个排程槽位不再卡在整点，而是在「原定整点 + 0..N 分钟」内的一个固定偏移
	// 触发（偏移由部署私有种子确定性派生，见 scheduler.scheduledSlot）：同一部署重启后
	// 偏移不变、不同部署互不相同，用于把固定整点的批量流量摊开（吸收 workbuddy-manager
	// 补丁 0003 / CHANGELOG 1.0.91）。0（缺省/旧配置）行为逐字不变。
	RandomDelayMinutes int `json:"random_delay_minutes"`
	// AccountDelayMinSeconds/AccountDelayMaxSeconds 同一批任务的账号错峰等待区间
	//（秒，0-600，缺省 0/0 = 关）：处理完一个账号后随机等 [min,max] 秒再处理下一个账号，
	// 避免整批账号在同一秒打上游。只作用于定时排程——手动「立即执行」不受影响。
	// 两者必须成对满足 0 <= min <= max（min>max 报错，而不是静默对调语义）。
	AccountDelayMinSeconds int `json:"account_delay_min_seconds"`
	AccountDelayMaxSeconds int `json:"account_delay_max_seconds"`
	// 猫猫旅行已退役 travel_interval_minutes：旅行现为独立排程（travel_hours）。
	// 旧 config 里的该键因 JSON 未知字段而自然忽略，不报错。
}

// DefaultSchedule 返回排程段的默认值。
//
// 开关「缺省 true」靠这里实现：调用方先取 DefaultSchedule 再用 json.Unmarshal 覆盖，
// 键缺席（或为 null）时字段原样保留 true，只有显式 false 才关。
// ActivityReportCount 默认 5：领猫前置需 5 次对话，5 连发刷满 chat_5。
func DefaultSchedule() Schedule {
	return Schedule{
		CheckinHours:        []int{9, 21},
		TravelHours:         []int{9, 21},
		ActivityHours:       []int{10},
		KeepaliveHours:      []int{22},
		SchoolHours:         []int{12},
		CatHours:            []int{1},
		QueueHours:          []int{10},
		RenewHours:          []int{3}, // 续期巡检默认凌晨 3 点：避开签到/活跃/队列高峰
		CheckinEnabled:      true,
		TravelEnabled:       true,
		ActivityEnabled:     true,
		KeepaliveEnabled:    true,
		SchoolEnabled:       true,
		CatEnabled:          true,
		ActivityReportCount: 5, // 领猫前置需 5 次对话，5 连发刷满 chat_5
		// TaskSweepEnabled 零值 = 关闭（opt-in）；周期预置 15 分钟，便于
		// 用户只打开开关就得到安全默认值。
		TaskSweepIntervalMinutes: DefaultTaskSweepIntervalMinutes,
		// RandomDelayMinutes/AccountDelayMinSeconds/AccountDelayMaxSeconds 缺省保持
		// 零值 0（关）＝旧行为逐字不变：随机延后与账号错峰都是 opt-in 的「摊开流量」
		// 开关，与六个 *_enabled 的「缺省 true」不同，不需要在此显式置 0。
		// QueueEnabled 缺省 false（零值即关）：队列是全账号真实任务动作的批量执行，
		// opt-in 才安全——与六个 *_enabled 的"缺省 true"相反，是有意的不对称。
		// CompensationEnabled / CompensationHours 同样刻意保持零值（false / 空表）：
		// 「默认不自动跑」是本功能的用户口径（补偿公告带截止日期，手动「立即执行」最稳），
		// 而空小时表另有一层含义——「只手动执行」（见 Normalize 的不回落例外）。
	}
}

// Normalize 归一化排程段：空数组/null 回落默认小时，ActivityReportCount 归一，校验小时范围。
//
// 空数组与 null 反序列化后覆盖掉 DefaultSchedule 的排程值（键缺席才保留），在此补齐。
// 空 = 未配置 → 回落默认；「禁用」一律走 *_enabled=false，两者互不混淆。
//
// ActivityReportCount：0/负数 → 1 条（兼容旧行为：每号每天 1 条上报点亮连登）。
// 注意这是「显式配 0 = 旧行为」的兼容语义，与 scheduler.New 的 <=0 → 1 归一一致；
// 「缺省 = 5」由 DefaultSchedule 在 Unmarshal 前置入，是另一条路径，两者不合并。
func (s *Schedule) Normalize() error {
	// 随机延后/账号错峰先校验：这两个值是「时长」不是「时点」，越界没有安全的回落值
	// 可猜（配了 90 分钟想要什么？），快速失败比静默改语义诚实（口径同 validateHours）。
	if err := s.validateDelays(); err != nil {
		return err
	}
	if len(s.CheckinHours) == 0 {
		s.CheckinHours = []int{9, 21}
	}
	if len(s.TravelHours) == 0 {
		s.TravelHours = []int{9, 21}
	}
	if len(s.ActivityHours) == 0 {
		s.ActivityHours = []int{10}
	}
	if len(s.KeepaliveHours) == 0 {
		s.KeepaliveHours = []int{22}
	}
	if len(s.SchoolHours) == 0 {
		s.SchoolHours = []int{12}
	}
	if len(s.CatHours) == 0 {
		s.CatHours = []int{1}
	}
	if len(s.QueueHours) == 0 {
		s.QueueHours = []int{10}
	}
	if len(s.RenewHours) == 0 {
		s.RenewHours = []int{3}
	}
	// CompensationHours **刻意不在这里补默认**：空数组 = 「只手动执行」（前端
	// 「留空 = 不自动跑」的语义）。其余七类的空数组是「未配置 → 回落默认」，若这里
	// 照做，用户在前端留空会静默变成「每天自动领补偿」——界面看不出任何变化。
	// enabled=true 且为空是合法配置（先开开关、暂不排程），不报错，只由 scheduler.New
	// 记一行 WARN 说明；非法整点仍在 validateHours 里照常报错。
	// 0/负数 → 1 条（兼容旧行为：每号每天 1 条上报点亮连登）。
	if s.ActivityReportCount <= 0 {
		s.ActivityReportCount = 1
	}
	// 任务补扫周期：0 = 未配置 → 15 分钟；显式小于 15 直接报错（键名可定位）。
	// 不能静默钳制：用户写下 5 分钟却实际 15 分钟，会误判「配置没生效」。
	if s.TaskSweepIntervalMinutes == 0 {
		s.TaskSweepIntervalMinutes = DefaultTaskSweepIntervalMinutes
	}
	if s.TaskSweepIntervalMinutes < MinTaskSweepIntervalMinutes {
		return fmt.Errorf("schedule.task_sweep_interval_minutes: %d 分钟低于安全下限 %d（默认 %d）；请配置 >= %d 分钟",
			s.TaskSweepIntervalMinutes, MinTaskSweepIntervalMinutes,
			DefaultTaskSweepIntervalMinutes, MinTaskSweepIntervalMinutes)
	}
	return s.validateHours()
}

// validateDelays 校验随机延后窗口与账号错峰区间。
//
// 为什么不像非法小时那样「钳掉就算」：钳值会静默改变用户写下的语义（把 90 分钟悄悄
// 变成 60 分钟、把 min>max 悄悄对调），而这两个键正是用来标记部署间差异的——错配的
// 部署会让人误判「随机没生效」。故一律报错，且错误信息带键名，用户能直接定位
// config.json 的哪一行（与 checkHourRange 同口径）。
func (s *Schedule) validateDelays() error {
	if s.RandomDelayMinutes < 0 || s.RandomDelayMinutes > 60 {
		return fmt.Errorf("schedule.random_delay_minutes: %d 不是合法分钟数（0-60）；0 = 关闭随机延后",
			s.RandomDelayMinutes)
	}
	lo, hi := s.AccountDelayMinSeconds, s.AccountDelayMaxSeconds
	if lo < 0 || hi < 0 || hi > 600 || lo > hi {
		return fmt.Errorf("schedule.account_delay_min_seconds/account_delay_max_seconds: %d/%d 不是合法区间"+
			"（需 0 <= min <= max <= 600 秒）；0 = 关闭账号错峰", lo, hi)
	}
	return nil
}

// validateHours 校验排程小时落在 0-23。
//
// 为什么不用 `[-1]` 之类的哨兵值表意"禁用"：非法小时被静默吞掉时，用户以为关掉了签到，
// 实际可能被当成另一个整点照常执行；这里直接快速失败，并在错误信息里指向正确的开关
// （checkin_enabled / keepalive_enabled），避免用户靠猜哨兵值来配。
func (s *Schedule) validateHours() error {
	if err := checkHourRange("schedule.checkin_hours", "checkin_enabled", s.CheckinHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.travel_hours", "travel_enabled", s.TravelHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.activity_hours", "activity_enabled", s.ActivityHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.keepalive_hours", "keepalive_enabled", s.KeepaliveHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.school_hours", "school_enabled", s.SchoolHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.cat_hours", "cat_enabled", s.CatHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.queue_hours", "queue_enabled", s.QueueHours); err != nil {
		return err
	}
	// compensation_hours 的「空」是合法语义（= 只手动，见 Normalize 的不回落例外），
	// 但一旦给了值就必须是合法整点——越界小时会被 nextFire 归一化成意外时点
	//（25 点静默漂移为次日 1 点），故与其余七类同一口径快速失败。
	return checkHourRange("schedule.compensation_hours", "compensation_enabled", s.CompensationHours)
	// renew_hours 不在校验链里：renew_enabled 缺省 false，巡检关闭时 renew_hours
	// 是惰性配置；开启路径由 scheduler.New 的 clampHours 钳非法小时（与其余七类同口径）。
}

func checkHourRange(field, switchKey string, hours []int) error {
	for _, h := range hours {
		if h < 0 || h > 23 {
			return fmt.Errorf("%s: %d 不是合法小时（0-23）；如要关闭该任务请设 schedule.%s=false", field, h, switchKey)
		}
	}
	return nil
}
