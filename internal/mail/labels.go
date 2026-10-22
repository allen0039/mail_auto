package mail

import (
	"database/sql"
	"mail-auto/internal/credentials"
	"mail-auto/internal/store"
	"regexp"
	"strings"
	"time"
	"unicode"
)

var labelColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func validateLabel(name, color string) error {
	if name == "" || len(name) > 64 || strings.IndexFunc(name, unicode.IsControl) >= 0 || (color != "" && !labelColor.MatchString(color)) {
		return ErrLabelInput
	}
	return nil
}

func (s *Service) Labels() ([]Label, error) {
	rows, e := s.Store.DB.Query("SELECT id,name,color,created_at FROM labels ORDER BY name,id")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Label{}
	for rows.Next() {
		var l Label
		if e = rows.Scan(&l.ID, &l.Name, &l.Color, &l.CreatedAt); e != nil {
			return nil, e
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Service) CreateLabel(name, color string) (Label, error) {
	name = strings.TrimSpace(name)
	if e := validateLabel(name, color); e != nil {
		return Label{}, e
	}
	id := credentials.Random(18)
	now := time.Now().Unix()
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return Label{}, e
	}
	defer tx.Rollback()
	_, e = tx.Exec("INSERT INTO labels(id,name,color,created_at) VALUES(?,?,?,?)", id, name, color, now)
	if e != nil {
		if strings.Contains(strings.ToLower(e.Error()), "unique") {
			return Label{}, ErrConflict
		}
		return Label{}, e
	}
	if e = store.Audit(tx, "label.create", id, "ok"); e != nil {
		return Label{}, e
	}
	if e = tx.Commit(); e != nil {
		return Label{}, e
	}
	s.revision.Add(1)
	return Label{ID: id, Name: name, Color: color, CreatedAt: now}, nil
}

func (s *Service) RenameLabel(id, name, color string) error {
	name = strings.TrimSpace(name)
	if e := validateLabel(name, color); e != nil {
		return e
	}
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	res, e := tx.Exec("UPDATE labels SET name=?,color=? WHERE id=?", name, color, id)
	if e != nil {
		if strings.Contains(strings.ToLower(e.Error()), "unique") {
			return ErrConflict
		}
		return e
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	if e = store.Audit(tx, "label.rename", id, "ok"); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	s.revision.Add(1)
	return nil
}

func (s *Service) DeleteLabel(id string) error {
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	res, e := tx.Exec("DELETE FROM labels WHERE id=?", id)
	if e != nil {
		return e
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	if e = store.Audit(tx, "label.delete", id, "ok"); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	s.revision.Add(1)
	return nil
}

func (s *Service) messageLabels(ref string) ([]Label, error) {
	rows, e := s.Store.DB.Query("SELECT l.id,l.name,l.color,l.created_at FROM labels l JOIN message_labels ml ON ml.label_id=l.id WHERE ml.message_ref=? ORDER BY l.name,l.id", ref)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Label{}
	for rows.Next() {
		var l Label
		if e = rows.Scan(&l.ID, &l.Name, &l.Color, &l.CreatedAt); e != nil {
			return nil, e
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Service) SetMessageLabels(ref string, add, remove []string) error {
	return s.setMessageLabels(ref, add, remove, "", 0)
}
func (s *Service) setMessageLabels(ref string, add, remove []string, operation string, position int) error {
	if len(add) > 50 || len(remove) > 50 {
		return ErrInput
	}
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var exists int
	if e = tx.QueryRow("SELECT count(*) FROM messages WHERE ref=?", ref).Scan(&exists); e != nil {
		return e
	}
	if exists != 1 {
		return sql.ErrNoRows
	}
	for _, name := range append(append([]string{}, add...), remove...) {
		name = strings.TrimSpace(name)
		if e = validateLabel(name, ""); e != nil {
			return e
		}
		var n int
		if e = tx.QueryRow("SELECT count(*) FROM labels WHERE name=?", name).Scan(&n); e != nil {
			return e
		}
		if n != 1 {
			return ErrInput
		}
	}
	for _, name := range remove {
		if _, e = tx.Exec("DELETE FROM message_labels WHERE message_ref=? AND label_id IN (SELECT id FROM labels WHERE name=?)", ref, strings.TrimSpace(name)); e != nil {
			return e
		}
	}
	for _, name := range add {
		name = strings.TrimSpace(name)
		if e = validateLabel(name, ""); e != nil {
			return e
		}
		if _, e = tx.Exec("INSERT OR IGNORE INTO message_labels(message_ref,label_id) SELECT ?,id FROM labels WHERE name=?", ref, name); e != nil {
			return e
		}
	}
	if e = store.Audit(tx, "mail.labels", ref, "ok"); e != nil {
		return e
	}
	if operation != "" {
		if _, e = tx.Exec("UPDATE operation_items SET state='completed',error_code='' WHERE operation_id=? AND position=?", operation, position); e != nil {
			return e
		}
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	s.revision.Add(1)
	return nil
}
