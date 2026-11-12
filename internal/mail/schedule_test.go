package mail

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestParseCron(t *testing.T) {
	for _, expr := range []string{"0 2 * * *", "*/15 * * * *", "0,30 9-18 * * 1-5", "5 4 1 */2 *", "0 8 * * 7", "10-50/20 * * * *"} {
		if _, ok := parseCron(expr); !ok {
			t.Fatal("rejected", expr)
		}
	}
	for _, expr := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 8", "a * * * *", "*/0 * * * *", "5-1 * * * *", "* * * * * *"} {
		if _, ok := parseCron(expr); ok {
			t.Fatal("accepted", expr)
		}
	}
	spec, _ := parseCron("0 8 * * 7")
	if !spec.dow.has(0) {
		t.Fatal("7 must mean Sunday")
	}
}

func TestCronSchedule(t *testing.T) {
	shanghai := TaskSchedule{Frequency: "cron", Cron: "30 9 * * 1-5", Timezone: "Asia/Shanghai"}
	// Friday 2026-11-13 10:00 Shanghai -> next Monday 2026-11-16 09:30.
	after := time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)
	n, e := nextSchedule(shanghai, after)
	if e != nil || time.Unix(n, 0).UTC() != time.Date(2026, 11, 16, 1, 30, 0, 0, time.UTC) {
		t.Fatal(time.Unix(n, 0).UTC(), e)
	}
	// Day-of-month OR day-of-week when both are restricted.
	either := TaskSchedule{Frequency: "cron", Cron: "0 0 1 * 0", Timezone: "UTC"}
	n, e = nextSchedule(either, time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC))
	if e != nil || time.Unix(n, 0).UTC() != time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC) {
		t.Fatal("dom|dow", time.Unix(n, 0).UTC(), e)
	}
	// Rare schedules are found within a year.
	leap := TaskSchedule{Frequency: "cron", Cron: "0 12 29 2 *", Timezone: "UTC"}
	if _, e = nextSchedule(leap, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)); !errors.Is(e, ErrInput) {
		t.Fatal("no Feb 29 within a year must fail", e)
	}
	// Autumn DST: the repeated 01:30 runs once.
	ny := TaskSchedule{Frequency: "cron", Cron: "30 1 * * *", Timezone: "America/New_York"}
	first, _ := nextSchedule(ny, time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC))
	second, _ := nextSchedule(ny, time.Unix(first, 0))
	if second-first < 23*3600 {
		t.Fatal("ambiguous time ran twice", time.Unix(first, 0).UTC(), time.Unix(second, 0).UTC())
	}
}

func TestIntervalAndSpacing(t *testing.T) {
	c := TaskSchedule{Frequency: "interval", Every: 15, Timezone: "Asia/Shanghai"}
	n, e := nextSchedule(c, time.Date(2026, 11, 12, 3, 7, 30, 0, time.UTC))
	if e != nil || time.Unix(n, 0).UTC() != time.Date(2026, 11, 12, 3, 15, 0, 0, time.UTC) {
		t.Fatal(time.Unix(n, 0).UTC(), e)
	}
	times, e := scheduleTimes(c)
	if e != nil || len(times) != 5 || times[1]-times[0] != 900 {
		t.Fatal(times, e)
	}
	for _, bad := range []TaskSchedule{
		{Frequency: "interval", Every: 4, Timezone: "UTC"},
		{Frequency: "interval", Every: 1441, Timezone: "UTC"},
		{Frequency: "cron", Cron: "* * * * *", Timezone: "UTC"},
		{Frequency: "cron", Cron: "*/2 3 * * *", Timezone: "UTC"},
		{Frequency: "cron", Cron: "0 2 * * *", Timezone: "Mars/Base"},
		{Frequency: "minutely", Timezone: "UTC"},
	} {
		if _, e = scheduleTimes(bad); !errors.Is(e, ErrInput) {
			t.Fatal("accepted", bad, e)
		}
	}
	if times, e = scheduleTimes(TaskSchedule{Frequency: "on_new_mail", Timezone: "Asia/Shanghai"}); e != nil || len(times) != 0 {
		t.Fatal(times, e)
	}
}

func TestNewMailTriggerSchedulesTaskAfterStabilityWindow(t *testing.T) {
	s, _, admin, _, task, m := taskFixture(t, "execute", "internal", "rules")
	task.Schedule = TaskSchedule{Frequency: "on_new_mail", Timezone: "Asia/Shanghai"}
	task, e := s.SaveTask(admin, task)
	if e != nil {
		t.Fatal(e)
	}
	if task.NextRunAt != 0 || len(task.NextTimes) != 0 {
		t.Fatal("event task must wait for mail", task)
	}
	s.triggerNewMailTasks(m.AccountID, "Archive")
	if current, _ := s.loadTask(task.ID); current.NextRunAt != 0 {
		t.Fatal("unwatched folder triggered task")
	}
	before := time.Now().Unix()
	s.triggerNewMailTasks(m.AccountID, "INBOX")
	current, _ := s.loadTask(task.ID)
	if current.NextRunAt < before+30 || current.NextRunAt > before+60 {
		t.Fatal("unexpected due time", current.NextRunAt-before)
	}
	// A later arrival keeps the earlier pending run (coalescing).
	due := current.NextRunAt
	time.Sleep(1100 * time.Millisecond)
	s.triggerNewMailTasks(m.AccountID, "INBOX")
	if current, _ = s.loadTask(task.ID); current.NextRunAt != due {
		t.Fatal("burst moved run later", current.NextRunAt, due)
	}
	// The run starts from the newest mail and the task returns to waiting.
	if _, e = s.Store.DB.Exec("UPDATE automation_tasks SET next_run_at=? WHERE id=?", time.Now().Unix()-1, task.ID); e != nil {
		t.Fatal(e)
	}
	current, _ = s.loadTask(task.ID)
	current.Cursor = "stale-cursor"
	if _, e = s.Store.DB.Exec("UPDATE automation_tasks SET payload=? WHERE id=?", taskJSON(current), task.ID); e != nil {
		t.Fatal(e)
	}
	s.taskWorkers = map[string]bool{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.taskTick(ctx)
	runs, e := s.TaskRuns(admin, task.ID)
	if e != nil || len(runs) == 0 || runs[0].Source != "internal" {
		t.Fatal(runs, e)
	}
	if run, _ := s.loadTaskRun(runs[0].ID); run.ScheduledAt == 0 {
		t.Fatal("scheduled time missing")
	}
	if current, _ = s.loadTask(task.ID); current.NextRunAt != 0 {
		t.Fatal("event task rescheduled itself", current.NextRunAt)
	}
	cancel()
	s.Stop()
}

func TestIncrementalSyncOnlyTriggersForGenuinelyNewMail(t *testing.T) {
	s, r, a := newAutomaticFixture(t)
	session := &fixtureSession{r}
	ctx := context.Background()
	if e := s.incrementalFolder(ctx, session, a, "INBOX", 0); e != nil {
		t.Fatal(e)
	}
	now := time.Now().Unix()
	task := Task{ID: "t1", AccountID: a.ID, Name: "new", Version: 1, Enabled: true, Trigger: "internal", Engine: "rules", Mode: "observe", SourceFolders: []string{"INBOX"}, Schedule: TaskSchedule{Frequency: "on_new_mail", Timezone: "UTC"}}
	if _, e := s.Store.DB.Exec("INSERT INTO automation_tasks VALUES(?,?,?,?,?,?,?,?,?)", task.ID, a.ID, 1, "svc:x", taskJSON(task), 1, "internal", 0, now); e != nil {
		t.Fatal(e)
	}
	next := func() int64 {
		var n int64
		s.Store.DB.QueryRow("SELECT next_run_at FROM automation_tasks WHERE id='t1'").Scan(&n)
		return n
	}
	// Re-reading legacy metadata must not look like an arrival.
	if _, e := s.Store.DB.Exec("UPDATE messages SET metadata_version=1"); e != nil {
		t.Fatal(e)
	}
	if e := s.incrementalFolder(ctx, session, a, "INBOX", 0); e != nil || next() != 0 {
		t.Fatal("metadata refresh triggered task", next(), e)
	}
	r.boxes["INBOX"] = Snapshot{UIDValidity: 7, Messages: append(r.boxes["INBOX"].Messages, Message{UID: 9, Unread: true})}
	if e := s.incrementalFolder(ctx, session, a, "INBOX", 0); e != nil || next() == 0 {
		t.Fatal("new mail did not trigger task", e)
	}
}
