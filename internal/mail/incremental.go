package mail

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"time"

	"mail-auto/internal/credentials"
)

var errAutomaticConfig = errors.New("邮箱配置已更新")

func (s *Service) folderState(ctx context.Context, id, folder string) (FolderState, error) {
	state := FolderState{Unread: map[uint32]bool{}}
	e := s.Store.DB.QueryRowContext(ctx, "SELECT uidvalidity FROM mailboxes WHERE account_id=? AND name=?", id, folder).Scan(&state.UIDValidity)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return state, syncStep("读取文件夹状态", e)
	}
	rows, e := s.Store.DB.QueryContext(ctx, "SELECT uid,unread FROM messages WHERE account_id=? AND folder=? AND uidvalidity=? AND metadata_version>=?", id, folder, state.UIDValidity, CurrentMetadataVersion)
	if e != nil {
		return state, syncStep("读取邮件索引状态", e)
	}
	defer rows.Close()
	for rows.Next() {
		var uid uint32
		var unread bool
		if e = rows.Scan(&uid, &unread); e != nil {
			return state, syncStep("读取邮件索引状态", e)
		}
		state.Unread[uid] = unread
	}
	return state, syncStep("读取邮件索引状态", rows.Err())
}

// The service mutex spans publication, so a manual request cannot start between
// the epoch check and commit. Config changes invalidate in-flight remote reads.
func (s *Service) automaticTx(ctx context.Context, a Account, epoch uint64) (*sql.Tx, error) {
	if s.stopping || ctx.Err() != nil {
		return nil, context.Canceled
	}
	if s.active[a.ID] || s.pending[a.ID] || s.automaticHolds[a.ID] > 0 || s.epoch[a.ID] != epoch {
		return nil, ErrBusy
	}
	tx, e := s.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		return nil, syncStep("开始更新自动同步索引", e)
	}
	current, e := scanAccount(tx.QueryRowContext(ctx, "SELECT * FROM accounts WHERE id=?", a.ID))
	if e == nil {
		e = s.accountNetworkIn(tx, &current)
	}
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		tx.Rollback()
		return nil, syncStep("校验自动同步配置", e)
	}
	if e != nil || !reflect.DeepEqual(current, a) {
		tx.Rollback()
		return nil, errAutomaticConfig
	}
	return tx, nil
}

func (s *Service) incrementalFolder(ctx context.Context, session AutomaticSession, a Account, folder string, epoch uint64) error {
	state, e := s.folderState(ctx, a.ID, folder)
	if e != nil {
		return e
	}
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
	staged := map[uint32]bool{}
	inventory, e := session.Refresh(ctx, folder, state, func(batch Snapshot) error {
		if batch.UIDValidity == 0 || (validity != 0 && validity != batch.UIDValidity) {
			return syncStep("校验增量邮件标识", errors.New("invalid UIDVALIDITY"))
		}
		validity = batch.UIDValidity
		if len(batch.Messages) == 0 {
			return nil
		}
		s.mu.Lock()
		stale := s.active[a.ID] || s.pending[a.ID] || s.automaticHolds[a.ID] > 0 || s.epoch[a.ID] != epoch
		s.mu.Unlock()
		if stale {
			return ErrBusy
		}
		tx, e := s.Store.DB.BeginTx(ctx, nil)
		if e != nil {
			return syncStep("开始暂存增量邮件", e)
		}
		defer tx.Rollback()
		for _, m := range batch.Messages {
			if m.UID == 0 || staged[m.UID] {
				return syncStep("校验增量邮件标识", errors.New("invalid UID"))
			}
			staged[m.UID] = true
			if e = stageMessage(ctx, tx, token, a.ID, folder, validity, m); e != nil {
				return syncStep("暂存增量邮件", e)
			}
		}
		return syncStep("提交增量邮件暂存区", tx.Commit())
	})
	if e != nil {
		return e
	}
	if validity == 0 || inventory.UIDValidity != validity {
		return syncStep("校验增量邮件标识", errors.New("missing UIDVALIDITY"))
	}
	present := make(map[uint32]bool, len(inventory.UIDs))
	for _, uid := range inventory.UIDs {
		if uid == 0 || present[uid] {
			return syncStep("校验增量邮件标识", errors.New("invalid UID inventory"))
		}
		present[uid] = true
		if _, ok := inventory.Unread[uid]; !ok {
			return syncStep("校验增量邮件状态", errors.New("missing flags"))
		}
		if _, ok := state.Unread[uid]; (!ok || state.UIDValidity != validity) && !staged[uid] {
			return syncStep("校验增量邮件元数据", errors.New("missing metadata"))
		}
	}
	for uid := range staged {
		if !present[uid] {
			return syncStep("校验增量邮件标识", errors.New("unexpected metadata UID"))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, e := s.automaticTx(ctx, a, epoch)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	rows, e := tx.QueryContext(ctx, "SELECT uidvalidity,uid,unread FROM messages WHERE account_id=? AND folder=?", a.ID, folder)
	if e != nil {
		return syncStep("读取待校正索引", e)
	}
	var remove, seen, unseen []uint32
	for rows.Next() {
		var oldValidity, uid uint32
		var unread bool
		if e = rows.Scan(&oldValidity, &uid, &unread); e != nil {
			break
		}
		if oldValidity != validity || !present[uid] {
			remove = append(remove, uid)
		} else if current, ok := inventory.Unread[uid]; ok && current != unread {
			if current {
				unseen = append(unseen, uid)
			} else {
				seen = append(seen, uid)
			}
		}
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return syncStep("读取待校正索引", e)
	}
	for _, uid := range remove {
		if _, e = tx.ExecContext(ctx, "DELETE FROM messages WHERE account_id=? AND folder=? AND uid=?", a.ID, folder, uid); e != nil {
			return syncStep("移除已删除邮件的索引", e)
		}
	}
	for _, group := range []struct {
		uids   []uint32
		unread bool
	}{{seen, false}, {unseen, true}} {
		for _, uid := range group.uids {
			if _, e = tx.ExecContext(ctx, "UPDATE messages SET unread=?,changed_at=? WHERE account_id=? AND folder=? AND uidvalidity=? AND uid=?", group.unread, time.Now().Unix(), a.ID, folder, validity, uid); e != nil {
				return syncStep("校正邮件已读状态", e)
			}
		}
	}
	flagChanges := 0
	for uid, flagged := range inventory.Flagged {
		result, err := tx.ExecContext(ctx, "UPDATE messages SET flagged=?,flags_known=1,changed_at=? WHERE account_id=? AND folder=? AND uidvalidity=? AND uid=? AND (flagged<>? OR flags_known=0)", flagged, time.Now().Unix(), a.ID, folder, validity, uid, flagged)
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		flagChanges += int(n)
	}
	var arrived int
	if e = tx.QueryRowContext(ctx, "SELECT count(*) FROM sync_stage s WHERE s.token=? AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.account_id=s.account_id AND m.folder=s.folder AND m.uidvalidity=s.uidvalidity AND m.uid=s.uid)", token).Scan(&arrived); e != nil {
		return syncStep("统计新到邮件", e)
	}
	result, e := tx.ExecContext(ctx, publishStageSQL, token)
	if e != nil {
		return syncStep("发布增量邮件索引", e)
	}
	added, _ := result.RowsAffected()
	if _, e = tx.ExecContext(ctx, `INSERT INTO mailboxes VALUES(?,?,?,?) ON CONFLICT(account_id,name) DO UPDATE SET uidvalidity=excluded.uidvalidity,synced_at=excluded.synced_at`, a.ID, folder, validity, time.Now().Unix()); e != nil {
		return syncStep("更新自动同步时间", e)
	}
	if e = tx.Commit(); e != nil {
		return syncStep("提交自动同步索引", e)
	}
	if added > 0 || len(remove)+len(seen)+len(unseen)+flagChanges > 0 {
		s.revision.Add(1)
	}
	if len(remove) > 0 {
		s.Store.RequestReclaim()
	}
	// A UIDVALIDITY reset re-indexes the whole folder; that is not new mail.
	if arrived > 0 && state.UIDValidity == validity {
		s.triggerNewMailTasks(a.ID, folder)
	}
	return nil
}

func (s *Service) reconcileAutomaticFolders(ctx context.Context, a Account, folders []string, epoch uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, e := s.automaticTx(ctx, a, epoch)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	rows, e := tx.QueryContext(ctx, "SELECT name FROM mailboxes WHERE account_id=?", a.ID)
	if e != nil {
		return syncStep("读取自动同步文件夹", e)
	}
	allowed := map[string]bool{}
	for _, f := range folders {
		allowed[f] = true
	}
	var removed []string
	for rows.Next() {
		var name string
		if e = rows.Scan(&name); e != nil {
			break
		}
		if !allowed[name] {
			removed = append(removed, name)
		}
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return syncStep("读取自动同步文件夹", e)
	}
	for _, folder := range removed {
		if _, e = tx.ExecContext(ctx, "DELETE FROM messages WHERE account_id=? AND folder=?", a.ID, folder); e != nil {
			return syncStep("移除已删除文件夹的索引", e)
		}
		if _, e = tx.ExecContext(ctx, "DELETE FROM mailboxes WHERE account_id=? AND name=?", a.ID, folder); e != nil {
			return syncStep("移除已删除文件夹的索引", e)
		}
	}
	if e = tx.Commit(); e != nil {
		return syncStep("提交文件夹校正", e)
	}
	if len(removed) > 0 {
		s.revision.Add(1)
		s.Store.RequestReclaim()
	}
	return nil
}
