package mailtools

import (
	"fmt"
	"strings"
	"time"

	"mail-auto/internal/mail"
)

var shanghai = time.FixedZone("Asia/Shanghai", 8*3600)

var operatorLabels = map[string]string{"equals": "等于", "contains": "包含", "not_contains": "不包含", "starts_with": "开头是", "ends_with": "结尾是", "regex": "匹配正则", "gte": "≥", "lte": "≤", "gt": ">", "lt": "<", "is": "是"}
var actionLabels = map[string]string{"move": "移动到", "archive": "归档", "trash": "移到回收站", "keep": "保留原位（保护）", "ai": "交给外部 AI 判断", "mark_read": "标记已读", "mark_unread": "标记未读", "flag": "加星标", "unflag": "取消星标"}

func describeGroup(g mail.RuleGroup) string {
	parts := []string{}
	for _, c := range g.Conditions {
		op := operatorLabels[c.Operator]
		if op == "" {
			op = c.Operator
		}
		parts = append(parts, fmt.Sprintf("%s %s %q", mail.RuleFieldLabel(c.Field), op, c.Value))
	}
	for _, child := range g.Groups {
		parts = append(parts, "("+describeGroup(child)+")")
	}
	switch g.Mode {
	case "any":
		return strings.Join(parts, " 或 ")
	case "not":
		return "非 " + strings.Join(parts, "")
	}
	if len(parts) == 0 {
		return "全部邮件"
	}
	return strings.Join(parts, " 且 ")
}

// DescribeRule renders a rule in fixed server text.
func DescribeRule(r mail.Rule) string {
	action := actionLabels[r.Action]
	if action == "" {
		action = r.Action
	}
	if r.Action == "move" {
		action += " " + r.Destination
	}
	for _, x := range r.Also {
		if l := actionLabels[x]; l != "" {
			action += "，并" + l
		}
	}
	state := "禁用"
	if r.Enabled {
		state = "启用"
	}
	return fmt.Sprintf("「%s」(%s，优先级 %d) 来源 %s；条件：%s；动作：%s", r.Name, state, r.Priority, strings.Join(r.SourceFolders, "、"), describeGroup(r.Match), action)
}

var weekdays = []string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}

func describeSchedule(t mail.Task) string {
	s := t.Schedule
	switch t.Trigger {
	case "manual":
		return "仅手动触发"
	case "external":
		return "外部 MCP 客户端触发"
	}
	tz := s.Timezone
	if tz == "" {
		tz = "Asia/Shanghai"
	}
	switch s.Frequency {
	case "hourly":
		return fmt.Sprintf("每小时第 %d 分（%s）", s.Minute, tz)
	case "daily":
		return fmt.Sprintf("每天 %02d:%02d（%s）", s.Hour, s.Minute, tz)
	case "weekly":
		day := ""
		if s.Weekday >= 0 && s.Weekday < 7 {
			day = weekdays[s.Weekday]
		}
		return fmt.Sprintf("每%s %02d:%02d（%s）", day, s.Hour, s.Minute, tz)
	case "interval":
		return fmt.Sprintf("每 %d 分钟（%s）", s.Every, tz)
	case "cron":
		return fmt.Sprintf("cron %q（%s）", s.Cron, tz)
	case "on_new_mail":
		return "有新邮件时（内置定时）"
	}
	return "内置定时：" + s.Frequency
}

// DescribeTask renders a task with its effective values.
func DescribeTask(t mail.Task, p mail.AutomationPolicy) string {
	mode := "观察模式（只记录，不写原邮箱）"
	if t.Mode == "execute" {
		mode = "执行模式（按规则写原邮箱）"
	}
	state := "暂停"
	if t.Enabled {
		state = "启用"
	}
	binding := "跟随账号全部启用规则（未来启用的规则也会影响该任务）"
	if len(t.RuleIDs) > 0 {
		binding = "绑定规则 " + strings.Join(t.RuleIDs, "、")
	}
	maxBatch := t.MaxBatch
	if maxBatch == 0 || maxBatch > p.MaxBatch {
		maxBatch = min(20, p.MaxBatch)
	}
	unread := p.ProtectUnread || t.ProtectUnread
	flagged := p.ProtectFlagged || t.ProtectFlagged
	text := fmt.Sprintf("任务「%s」(%s) 来源 %s；%s；%s；每批最多 %d 封；保护未读：%s，保护星标：%s；无每日处理额度", t.Name, state, strings.Join(t.SourceFolders, "、"), mode, binding, maxBatch, yesNo(unread), yesNo(flagged))
	return text + "；触发：" + describeSchedule(t)
}

// NextTimesText lists upcoming occurrences in Asia/Shanghai.
func NextTimesText(t mail.Task) string {
	if t.Trigger != "internal" {
		return ""
	}
	times, err := mail.ScheduleTimes(t.Schedule)
	if err != nil || len(times) == 0 {
		return ""
	}
	parts := []string{}
	for _, x := range times {
		parts = append(parts, time.Unix(x, 0).In(shanghai).Format("01-02 15:04"))
	}
	return "未来 5 次（北京时间）：" + strings.Join(parts, "，")
}

func yesNo(b bool) string {
	if b {
		return "是"
	}
	return "否"
}
