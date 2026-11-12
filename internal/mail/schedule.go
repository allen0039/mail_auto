package mail

import (
	"slices"
	"strconv"
	"strings"
	"time"
)

// Minimum spacing between two scheduled runs. Shorter schedules would only
// coalesce with the still-running previous batch.
const minScheduleGap = 5 * 60

// cronField is a bitmap of allowed values for one cron field.
type cronField struct {
	allowed uint64
	any     bool
}

func (f cronField) has(v int) bool { return f.allowed&(1<<uint(v)) != 0 }

type cronSpec struct {
	minute, hour, dom, month, dow cronField
}

func parseCronField(raw string, lo, hi int) (cronField, bool) {
	if raw == "" {
		return cronField{}, false
	}
	f := cronField{any: raw == "*"}
	for _, part := range strings.Split(raw, ",") {
		step := 1
		if base, s, ok := strings.Cut(part, "/"); ok {
			n, e := strconv.Atoi(s)
			if e != nil || n < 1 || n > hi {
				return cronField{}, false
			}
			step, part = n, base
		}
		start, end := lo, hi
		if part != "*" {
			a, b, isRange := strings.Cut(part, "-")
			x, e := strconv.Atoi(a)
			if e != nil {
				return cronField{}, false
			}
			start, end = x, x
			if isRange {
				if end, e = strconv.Atoi(b); e != nil {
					return cronField{}, false
				}
			} else if step > 1 {
				end = hi
			}
		}
		if start < lo || end > hi || start > end {
			return cronField{}, false
		}
		for v := start; v <= end; v += step {
			f.allowed |= 1 << uint(v)
		}
	}
	return f, f.allowed != 0
}

func parseCron(expr string) (cronSpec, bool) {
	fields := strings.Fields(expr)
	if len(expr) > 120 || len(fields) != 5 {
		return cronSpec{}, false
	}
	var c cronSpec
	var ok [5]bool
	c.minute, ok[0] = parseCronField(fields[0], 0, 59)
	c.hour, ok[1] = parseCronField(fields[1], 0, 23)
	c.dom, ok[2] = parseCronField(fields[2], 1, 31)
	c.month, ok[3] = parseCronField(fields[3], 1, 12)
	c.dow, ok[4] = parseCronField(fields[4], 0, 7)
	if c.dow.has(7) {
		c.dow.allowed |= 1 // 7 is also Sunday.
	}
	return c, ok[0] && ok[1] && ok[2] && ok[3] && ok[4]
}

// dayMatches follows the classic cron rule: when both day-of-month and
// day-of-week are restricted, either one may match.
func (c cronSpec) dayMatches(l time.Time) bool {
	if !c.month.has(int(l.Month())) {
		return false
	}
	dom, dow := c.dom.has(l.Day()), c.dow.has(int(l.Weekday()))
	switch {
	case c.dom.any && c.dow.any:
		return true
	case c.dom.any:
		return dow
	case c.dow.any:
		return dom
	}
	return dom || dow
}

func validTimezone(name string) (*time.Location, bool) {
	loc, e := time.LoadLocation(name)
	return loc, e == nil
}

// nextSchedule returns the next occurrence strictly after `after`. A zero
// result with no error means the task is event-driven (on_new_mail).
func nextSchedule(c TaskSchedule, after time.Time) (int64, error) {
	loc, ok := validTimezone(c.Timezone)
	if !ok {
		return 0, ErrInput
	}
	switch c.Frequency {
	case "on_new_mail":
		return 0, nil
	case "interval":
		if c.Every < 5 || c.Every > 1440 {
			return 0, ErrInput
		}
		step := int64(c.Every) * 60
		return (after.Unix()/step + 1) * step, nil
	case "hourly", "daily", "weekly":
		if c.Hour < 0 || c.Hour > 23 || c.Minute < 0 || c.Minute > 59 || c.Weekday < 0 || c.Weekday > 6 {
			return 0, ErrInput
		}
		match := func(l time.Time) bool {
			return l.Minute() == c.Minute && (c.Frequency == "hourly" || l.Hour() == c.Hour) && (c.Frequency != "weekly" || int(l.Weekday()) == c.Weekday)
		}
		return scanMinutes(after, loc, 9*24*60, nil, match)
	case "cron":
		spec, ok := parseCron(c.Cron)
		if !ok {
			return 0, ErrInput
		}
		match := func(l time.Time) bool { return spec.minute.has(l.Minute()) && spec.hour.has(l.Hour()) }
		return scanMinutes(after, loc, 366*24*60, spec.dayMatches, match)
	}
	return 0, ErrInput
}

// scanMinutes walks real UTC minutes and evaluates them in local time. Days
// rejected by dayOK are skipped wholesale. The second occurrence of an
// ambiguous local time (autumn DST) is skipped; nonexistent local times
// (spring DST) never appear.
func scanMinutes(after time.Time, loc *time.Location, limit int, dayOK func(time.Time) bool, match func(time.Time) bool) (int64, error) {
	v := after.UTC().Truncate(time.Minute).Add(time.Minute)
	end := v.Add(time.Duration(limit) * time.Minute)
	for v.Before(end) {
		l := v.In(loc)
		if dayOK != nil && !dayOK(l) {
			next := time.Date(l.Year(), l.Month(), l.Day()+1, 0, 0, 0, 0, loc)
			if !next.After(v) {
				next = v.Add(time.Minute)
			}
			v = next.UTC()
			continue
		}
		if match(l) && !repeatedWallClock(v, loc) {
			return v.Unix(), nil
		}
		v = v.Add(time.Minute)
	}
	return 0, ErrInput
}

func repeatedWallClock(v time.Time, loc *time.Location) bool {
	l := v.In(loc)
	for j := 1; j <= 180; j++ {
		old := v.Add(-time.Duration(j) * time.Minute).In(loc)
		if old.Year() == l.Year() && old.YearDay() == l.YearDay() && old.Hour() == l.Hour() && old.Minute() == l.Minute() {
			return true
		}
	}
	return false
}

// scheduleTimes previews the next five runs and enforces the minimum spacing.
// Event-driven schedules have no fixed times.
func scheduleTimes(c TaskSchedule) ([]int64, error) {
	out := []int64{}
	if c.Frequency == "on_new_mail" {
		if _, ok := validTimezone(c.Timezone); !ok {
			return nil, ErrInput
		}
		return out, nil
	}
	at := time.Now()
	for i := 0; i < 5; i++ {
		n, e := nextSchedule(c, at)
		if e != nil {
			return nil, e
		}
		if i > 0 && n-out[i-1] < minScheduleGap {
			return nil, ErrInput
		}
		out = append(out, n)
		at = time.Unix(n, 0)
	}
	if c.Frequency == "cron" {
		// Also reject dense patterns that only show up later, e.g. "*/1 3 * * *".
		spec, _ := parseCron(c.Cron)
		previous := -1
		for m := 0; m < 60; m++ {
			if !spec.minute.has(m) {
				continue
			}
			if previous >= 0 && m-previous < minScheduleGap/60 {
				return nil, ErrInput
			}
			previous = m
		}
	}
	return out, nil
}

// triggerNewMailTasks schedules enabled on_new_mail tasks that watch folder.
// The run waits for the policy stability window so the new messages are not
// skipped as "still changing". An earlier pending run time is kept, which
// coalesces bursts of arrivals into one run. The version guard makes this safe
// against concurrent task edits without taking the task mutex.
func (s *Service) triggerNewMailTasks(accountID, folder string) {
	rows, e := s.Store.DB.Query("SELECT id FROM automation_tasks WHERE account_id=? AND enabled=1 AND trigger_mode='internal' AND json_extract(payload,'$.task.schedule.frequency')='on_new_mail' LIMIT 50", accountID)
	if e != nil {
		return
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if len(ids) == 0 {
		return
	}
	delay := int64(30)
	if p, err := s.loadAutomationPolicy(accountID); err == nil {
		delay = max(delay, int64(p.StableSeconds)+15)
	}
	due := time.Now().Unix() + delay
	for _, id := range ids {
		t, err := s.loadTask(id)
		if err != nil || !slices.Contains(t.SourceFolders, folder) {
			continue
		}
		_, _ = s.Store.DB.Exec("UPDATE automation_tasks SET next_run_at=? WHERE id=? AND version=? AND enabled=1 AND (next_run_at=0 OR next_run_at>?)", due, t.ID, t.Version, due)
	}
}
