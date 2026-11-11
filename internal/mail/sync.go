package mail

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/store"
)

type SyncResult struct {
	Folders  int `json:"folders"`
	Messages int `json:"messages"`
}

type SyncJob struct {
	AccountID    string `json:"account_id"`
	State        string `json:"state"`
	Folder       string `json:"folder"`
	FoldersTotal int    `json:"folders_total"`
	FoldersDone  int    `json:"folders_done"`
	Messages     int    `json:"messages"`
	StartedAt    int64  `json:"started_at"`
	UpdatedAt    int64  `json:"updated_at"`
	Error        string `json:"error,omitempty"`
}

// RecoverSync runs once when the serving process starts, never from maintenance CLI commands.
func (s *Service) RecoverSync(ctx context.Context) error {
	if _, e := s.Store.DB.ExecContext(ctx, "DELETE FROM sync_stage"); e != nil {
		return e
	}
	_, e := s.Store.DB.ExecContext(ctx, "UPDATE sync_jobs SET state='interrupted',error='服务已重启，请重新同步',updated_at=? WHERE state IN ('running','queued')", time.Now().Unix())
	return e
}

func (s *Service) SyncJobs() ([]SyncJob, error) {
	rows, e := s.Store.DB.Query("SELECT account_id,state,folder,folders_total,folders_done,messages,started_at,updated_at,error FROM sync_jobs ORDER BY started_at DESC")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	jobs := []SyncJob{}
	for rows.Next() {
		var job SyncJob
		if e = rows.Scan(&job.AccountID, &job.State, &job.Folder, &job.FoldersTotal, &job.FoldersDone, &job.Messages, &job.StartedAt, &job.UpdatedAt, &job.Error); e != nil {
			return nil, e
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Service) saveJob(ctx context.Context, job *SyncJob) error {
	job.UpdatedAt = time.Now().Unix()
	_, e := s.Store.DB.ExecContext(ctx, `INSERT INTO sync_jobs VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(account_id) DO UPDATE SET state=excluded.state,folder=excluded.folder,folders_total=excluded.folders_total,folders_done=excluded.folders_done,messages=excluded.messages,started_at=excluded.started_at,updated_at=excluded.updated_at,error=excluded.error`, job.AccountID, job.State, job.Folder, job.FoldersTotal, job.FoldersDone, job.Messages, job.StartedAt, job.UpdatedAt, job.Error)
	return e
}

func (s *Service) StartSync(id string) (SyncJob, error) {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return SyncJob{}, errors.New("服务正在关闭")
	}
	if s.pending[id] {
		s.mu.Unlock()
		return SyncJob{AccountID: id, State: "queued"}, nil
	}
	if s.active[id] || s.automaticHolds[id] > 0 {
		s.mu.Unlock()
		return SyncJob{}, ErrBusy
	}
	if s.pending == nil {
		s.pending = map[string]bool{}
	}
	s.pending[id] = true
	if s.epoch == nil {
		s.epoch = map[string]uint64{}
	}
	s.epoch[id]++
	if s.Context == nil {
		s.Context = context.Background()
	}
	if s.cancel == nil {
		s.Context, s.cancel = context.WithCancel(s.Context)
	}
	ctx := s.Context
	s.wg.Add(1)
	s.mu.Unlock()
	started := false
	defer func() {
		if !started {
			s.mu.Lock()
			delete(s.pending, id)
			s.mu.Unlock()
			s.wg.Done()
		}
	}()
	if _, e := s.Account(id); e != nil {
		return SyncJob{}, e
	}
	job := SyncJob{AccountID: id, State: "queued", StartedAt: time.Now().Unix()}
	if e := s.saveJob(ctx, &job); e != nil {
		return job, e
	}
	initial := job
	started = true
	go func() {
		defer s.wg.Done()
		defer func() { s.mu.Lock(); delete(s.pending, id); s.mu.Unlock() }()
		var release func()
		var e error
		for {
			release, e = s.acquire(id)
			if e == nil {
				break
			}
			select {
			case <-ctx.Done():
				e = ctx.Err()
			case <-time.After(200 * time.Millisecond):
				continue
			}
			break
		}
		if e == nil {
			defer release()
			job.State = "running"
			e = syncStep("保存同步进度", s.saveJob(ctx, &job))
			if e == nil {
				_, e = s.sync(ctx, id, func(folder string, total, done, messages int) error {
					job.Folder, job.FoldersTotal, job.FoldersDone, job.Messages = folder, total, done, messages
					return syncStep("保存同步进度", s.saveJob(ctx, &job))
				})
			}
		}
		job.State = "completed"
		if e != nil {
			job.State = "failed"
			job.Error = syncErrorMessage(e) + "；未完成文件夹保留原索引"
			if ctx.Err() != nil {
				job.State = "interrupted"
				job.Error = "同步已中断，请重新同步"
			}
			slog.Warn("邮箱同步未完成", "account_id", id, "folder", job.Folder, "messages", job.Messages, "folders_done", job.FoldersDone, "reason", job.Error)
		}
		finish, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if e := s.saveJob(finish, &job); e != nil {
			slog.Error("保存最终同步状态失败", "account_id", id)
		}
	}()
	// Return a separate value: the worker may already be updating its progress.
	return initial, nil
}

func (s *Service) Stop() {
	s.mu.Lock()
	s.stopping = true
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Service) Sync(ctx context.Context, id string) (SyncResult, error) {
	release, e := s.acquire(id)
	if e != nil {
		return SyncResult{}, e
	}
	defer release()
	return s.sync(ctx, id, func(string, int, int, int) error { return nil })
}

func (s *Service) sync(ctx context.Context, id string, progress func(string, int, int, int) error) (out SyncResult, err error) {
	a, e := s.Account(id)
	if e != nil {
		return out, syncStep("读取邮箱配置", e)
	}
	pw, e := s.password(a)
	if e != nil {
		return out, syncStep("读取邮箱凭据", e)
	}
	folders := a.Folders
	if len(folders) == 0 {
		folders, e = s.Remote.Folders(ctx, a, pw)
		if e != nil {
			return out, e
		}
	}
	defer func() {
		outcome := "ok"
		if err != nil {
			outcome = "failed"
		}
		_ = store.Audit(s.Store.DB, "mail.sync", id, outcome)
	}()
	for _, folder := range folders {
		if e = progress(folder, len(folders), out.Folders, out.Messages); e != nil {
			return out, e
		}
		count, e := s.scanFolder(ctx, a, pw, folder, func(n int) error { return progress(folder, len(folders), out.Folders, out.Messages+n) })
		if e != nil {
			return out, e
		}
		out.Folders++
		out.Messages += count
		if e = progress(folder, len(folders), out.Folders, out.Messages); e != nil {
			return out, e
		}
	}
	// Only reconcile removed folders after every selected folder has completed.
	boxes, e := s.Mailboxes(id)
	if e != nil {
		return out, e
	}
	allowed := map[string]bool{}
	for _, folder := range folders {
		allowed[folder] = true
	}
	for _, box := range boxes {
		if !allowed[box.Name] {
			tx, e := s.Store.DB.BeginTx(ctx, nil)
			if e != nil {
				return out, e
			}
			if _, e = tx.ExecContext(ctx, "DELETE FROM messages WHERE account_id=? AND folder=?", id, box.Name); e == nil {
				_, e = tx.ExecContext(ctx, "DELETE FROM mailboxes WHERE account_id=? AND name=?", id, box.Name)
			}
			if e != nil {
				tx.Rollback()
				return out, e
			}
			if e = tx.Commit(); e != nil {
				return out, e
			}
			s.revision.Add(1)
			s.Store.RequestReclaim()
		}
	}
	return out, nil
}

func (s *Service) scanFolder(ctx context.Context, a Account, pw, folder string, progress func(int) error) (int, error) {
	token := credentials.Random(24)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if result, e := s.Store.DB.ExecContext(cleanup, "DELETE FROM sync_stage WHERE token=?", token); e == nil {
			if n, _ := result.RowsAffected(); n > 0 {
				s.Store.RequestReclaim()
			}
		}
	}()
	var validity uint32
	count := 0
	e := s.Remote.Scan(ctx, a, pw, folder, func(batch Snapshot) error {
		if batch.UIDValidity == 0 || (validity != 0 && validity != batch.UIDValidity) {
			return errors.New("服务器返回无效或变化的 UIDVALIDITY")
		}
		validity = batch.UIDValidity
		tx, e := s.Store.DB.BeginTx(ctx, nil)
		if e != nil {
			return syncStep("开始暂存邮件", e)
		}
		defer tx.Rollback()
		for _, m := range batch.Messages {
			if m.UID == 0 {
				return errors.New("服务器返回无效 UID")
			}
			if _, e = tx.ExecContext(ctx, "INSERT INTO sync_stage VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", token, credentials.Random(24), a.ID, folder, validity, m.UID, m.Subject, m.Sender, m.SentAt, m.Unread, m.Size, m.ReceivedAt, m.Flagged, m.FlagsKnown, time.Now().Unix(), 1); e != nil {
				return syncStep("写入邮件暂存区", e)
			}
		}
		if e = tx.Commit(); e != nil {
			return syncStep("提交邮件暂存区", e)
		}
		count += len(batch.Messages)
		return progress(count)
	})
	if e != nil {
		return count, e
	}
	if validity == 0 {
		return count, errors.New("服务器未返回 UIDVALIDITY")
	}
	tx, e := s.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		return count, syncStep("开始发布文件夹索引", e)
	}
	defer tx.Rollback()
	var exists int
	if e = tx.QueryRowContext(ctx, "SELECT count(*) FROM accounts WHERE id=?", a.ID).Scan(&exists); e != nil {
		return count, syncStep("检查邮箱配置", e)
	}
	if exists != 1 {
		return count, sql.ErrNoRows
	}
	if _, e = tx.ExecContext(ctx, `DELETE FROM messages WHERE account_id=? AND folder=? AND (uidvalidity<>? OR uid NOT IN (SELECT uid FROM sync_stage WHERE token=?))`, a.ID, folder, validity, token); e != nil {
		return count, syncStep("校正文件夹索引", e)
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO messages SELECT ref,account_id,folder,uidvalidity,uid,subject,sender,sent_at,unread,size,received_at,flagged,flags_known,changed_at,metadata_version FROM sync_stage WHERE token=? ON CONFLICT(account_id,folder,uidvalidity,uid) DO UPDATE SET changed_at=CASE WHEN messages.subject<>excluded.subject OR messages.sender<>excluded.sender OR messages.unread<>excluded.unread OR messages.flagged<>excluded.flagged OR messages.flags_known<>excluded.flags_known OR messages.received_at<>excluded.received_at THEN excluded.changed_at ELSE messages.changed_at END,subject=excluded.subject,sender=excluded.sender,sent_at=excluded.sent_at,unread=excluded.unread,size=excluded.size,received_at=excluded.received_at,flagged=excluded.flagged,flags_known=excluded.flags_known,metadata_version=excluded.metadata_version`, token); e != nil {
		return count, syncStep("发布文件夹索引", e)
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO mailboxes VALUES(?,?,?,?) ON CONFLICT(account_id,name) DO UPDATE SET uidvalidity=excluded.uidvalidity,synced_at=excluded.synced_at`, a.ID, folder, validity, time.Now().Unix()); e != nil {
		return count, syncStep("保存文件夹同步时间", e)
	}
	if e = tx.Commit(); e != nil {
		return count, syncStep("提交文件夹索引", e)
	}
	s.revision.Add(1)
	s.Store.RequestReclaim()
	return count, nil
}
