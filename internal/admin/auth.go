package admin

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"mail-auto/internal/credentials"
	"mail-auto/internal/store"
)

var ErrAuth = errors.New("用户名或密码错误，临时凭据也可能已使用或到期")
var ErrSession = errors.New("会话已失效，请重新登录")
var ErrCurrentPassword = errors.New("当前密码不正确")
var ErrCredentialsInput = errors.New("账号设置无效")

type Auth struct {
	Store *store.Store
	Vault *credentials.Vault
	TTL   time.Duration
}
type Bootstrap struct {
	Username string    `json:"username"`
	Password string    `json:"password"`
	Expires  time.Time `json:"expires"`
}
type Session struct {
	Token      string `json:"-"`
	CSRF       string `json:"csrf"`
	Username   string `json:"username"`
	Restricted bool   `json:"must_change_credentials"`
	Version    int    `json:"-"`
	Expires    int64  `json:"expires"`
}

func (a *Auth) Initialize() error {
	tx, e := a.Store.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var fp string
	e = tx.QueryRow("SELECT key_fingerprint FROM instance WHERE id=1").Scan(&fp)
	if e == nil {
		if fp != a.Vault.Fingerprint {
			return errors.New("master key does not match this database")
		}
		var n int
		if e = tx.QueryRow("SELECT count(*) FROM admin").Scan(&n); e != nil {
			return e
		}
		if n != 1 {
			return errors.New("initialized database has no administrator; restore a valid backup")
		}
		return tx.Commit()
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	var n int
	if e = tx.QueryRow("SELECT (SELECT count(*) FROM admin)+(SELECT count(*) FROM accounts)+(SELECT count(*) FROM bootstrap)").Scan(&n); e != nil {
		return e
	}
	if n != 0 {
		return errors.New("inconsistent initialization state")
	}
	if _, e = tx.Exec("INSERT INTO instance VALUES(1,?,?)", a.Vault.Fingerprint, time.Now().Unix()); e != nil {
		return e
	}
	if _, e = a.newTemporary(tx, true); e != nil {
		return e
	}
	return tx.Commit()
}
func (a *Auth) newTemporary(tx *sql.Tx, fresh bool) (Bootstrap, error) {
	b := Bootstrap{Username: "admin-" + credentials.Random(6), Password: credentials.Random(24), Expires: time.Now().UTC().Add(24 * time.Hour)}
	encoded := credentials.Hash(b.Password)
	var e error
	if fresh {
		_, e = tx.Exec("INSERT INTO admin(id,username,password_hash,temp_expires) VALUES(1,?,?,?)", b.Username, encoded, b.Expires.Unix())
	} else {
		_, e = tx.Exec("UPDATE admin SET username=?,password_hash=?,must_change=1,version=version+1,temp_expires=?,consumed=0 WHERE id=1", b.Username, encoded, b.Expires.Unix())
	}
	if e != nil {
		return b, e
	}
	raw, e := json.Marshal(b)
	if e != nil {
		return b, e
	}
	cipher, e := a.Vault.Encrypt(string(raw), "bootstrap")
	if e != nil {
		return b, e
	}
	if _, e = tx.Exec("INSERT OR REPLACE INTO bootstrap VALUES(1,?)", cipher); e != nil {
		return b, e
	}
	if _, e = tx.Exec("DELETE FROM sessions"); e != nil {
		return b, e
	}
	action := "admin.recover"
	if fresh {
		action = "admin.initialize"
	}
	if e = store.Audit(tx, action, "administrator", "ok"); e != nil {
		return b, e
	}
	return b, nil
}
func (a *Auth) Show() (Bootstrap, error) {
	var b Bootstrap
	var raw []byte
	e := a.Store.DB.QueryRow("SELECT payload FROM bootstrap JOIN admin ON admin.id=bootstrap.id WHERE must_change=1 AND consumed=0 AND temp_expires>?", time.Now().Unix()).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return b, errors.New("no unused temporary credentials; use bootstrap recover locally")
	}
	if e != nil {
		return b, e
	}
	plain, e := a.Vault.Decrypt(raw, "bootstrap")
	if e != nil {
		return b, e
	}
	e = json.Unmarshal([]byte(plain), &b)
	return b, e
}
func (a *Auth) Recover() error {
	tx, e := a.Store.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var fp string
	if e = tx.QueryRow("SELECT key_fingerprint FROM instance WHERE id=1").Scan(&fp); e != nil {
		return e
	}
	if fp != a.Vault.Fingerprint {
		return errors.New("key mismatch")
	}
	var n int
	if e = tx.QueryRow("SELECT count(*) FROM admin").Scan(&n); e != nil {
		return e
	}
	if n != 1 {
		return errors.New("missing administrator; restore backup")
	}
	if _, e = a.newTemporary(tx, false); e != nil {
		return e
	}
	return tx.Commit()
}
func (a *Auth) Login(username, password string) (Session, error) {
	var s Session
	var hash, name string
	var change, version, consumed int
	var expires int64
	e := a.Store.DB.QueryRow("SELECT username,password_hash,must_change,version,temp_expires,consumed FROM admin WHERE id=1").Scan(&name, &hash, &change, &version, &expires, &consumed)
	if e != nil {
		return s, e
	}
	// Always perform the password hash work, including unknown usernames.
	valid := credentials.Verify(hash, password)
	if !valid || name != username || (change == 1 && (consumed == 1 || expires <= time.Now().Unix())) {
		_ = store.Audit(a.Store.DB, "auth.login", "administrator", "denied")
		return s, ErrAuth
	}
	tx, e := a.Store.DB.Begin()
	if e != nil {
		return s, e
	}
	defer tx.Rollback()
	var current, used int
	if e = tx.QueryRow("SELECT version,consumed FROM admin WHERE id=1").Scan(&current, &used); e != nil {
		return s, e
	}
	if current != version || (change == 1 && used == 1) {
		return s, ErrAuth
	}
	if change == 1 {
		res, e := tx.Exec("UPDATE admin SET consumed=1 WHERE id=1 AND temp_expires>?", time.Now().Unix())
		if e != nil {
			return s, e
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return s, ErrAuth
		}
		if _, e = tx.Exec("DELETE FROM bootstrap"); e != nil {
			return s, e
		}
	}
	ttl := a.TTL
	if change == 1 {
		ttl = 30 * time.Minute
	}
	s = Session{Token: credentials.Random(32), CSRF: credentials.Random(32), Username: name, Restricted: change == 1, Version: version, Expires: time.Now().Add(ttl).Unix()}
	if _, e = tx.Exec("INSERT INTO sessions VALUES(?,?,?,?,?)", credentials.Digest(s.Token), s.CSRF, version, change, s.Expires); e != nil {
		return s, e
	}
	if e = store.Audit(tx, "auth.login", "administrator", "ok"); e != nil {
		return s, e
	}
	return s, tx.Commit()
}
func (a *Auth) Session(token string) (Session, error) {
	var s Session
	var restricted int
	if len(token) != 43 {
		return s, ErrSession
	}
	e := a.Store.DB.QueryRow(`SELECT s.csrf,a.username,s.restricted,s.version,s.expires FROM sessions s JOIN admin a ON a.version=s.version WHERE s.token_hash=? AND s.expires>? AND s.restricted=a.must_change`, credentials.Digest(token), time.Now().Unix()).Scan(&s.CSRF, &s.Username, &restricted, &s.Version, &s.Expires)
	if e != nil {
		return s, ErrSession
	}
	s.Token = token
	s.Restricted = restricted == 1
	return s, nil
}
func (a *Auth) Logout(s Session) error {
	_, e := a.Store.DB.Exec("DELETE FROM sessions WHERE token_hash=?", credentials.Digest(s.Token))
	return e
}
func ValidateCredentials(username, password string) error {
	if len(username) < 3 || len(username) > 64 || strings.TrimSpace(username) != username {
		return errors.New("用户名须为 3–64 个字符，不能包含首尾空格")
	}
	for _, c := range username {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return errors.New("用户名仅支持英文字母、数字、点、下划线和横线")
		}
	}
	if utf8.RuneCountInString(password) < 12 || len(password) > 256 || !utf8.ValidString(password) || strings.TrimSpace(password) == "" {
		return errors.New("密码须至少 12 个字符，最多 256 字节")
	}
	return nil
}
func (a *Auth) Setup(s Session, username, password string) error {
	if !s.Restricted {
		return errors.New("only first-time setup is supported here")
	}
	if e := ValidateCredentials(username, password); e != nil {
		return e
	}
	var hash, old string
	var version, change int
	if e := a.Store.DB.QueryRow("SELECT username,password_hash,version,must_change FROM admin WHERE id=1").Scan(&old, &hash, &version, &change); e != nil {
		return e
	}
	if version != s.Version || change != 1 {
		return ErrSession
	}
	if username == old || credentials.Verify(hash, password) {
		return errors.New("请同时更换临时用户名和临时密码")
	}
	tx, e := a.Store.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	res, e := tx.Exec("UPDATE admin SET username=?,password_hash=?,must_change=0,version=version+1,temp_expires=0,consumed=1 WHERE id=1 AND version=? AND must_change=1", username, credentials.Hash(password), s.Version)
	if e != nil {
		return e
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrSession
	}
	for _, q := range []string{"DELETE FROM sessions", "DELETE FROM bootstrap"} {
		if _, e = tx.Exec(q); e != nil {
			return e
		}
	}
	if e = store.Audit(tx, "admin.setup", "administrator", "ok"); e != nil {
		return e
	}
	return tx.Commit()
}

// ChangeCredentials rotates the administrator version and revokes every session
// atomically. An empty password keeps the existing password hash.
func (a *Auth) ChangeCredentials(s Session, username, currentPassword, password string) error {
	if s.Restricted {
		return ErrSession
	}
	if currentPassword == "" || len(currentPassword) > 256 {
		return ErrCurrentPassword
	}
	var old, hash string
	var version, change int
	if e := a.Store.DB.QueryRow("SELECT username,password_hash,version,must_change FROM admin WHERE id=1").Scan(&old, &hash, &version, &change); e != nil {
		return e
	}
	if version != s.Version || change != 0 {
		return ErrSession
	}
	if !credentials.Verify(hash, currentPassword) {
		if e := store.Audit(a.Store.DB, "admin.credentials", "administrator", "denied"); e != nil {
			return e
		}
		return ErrCurrentPassword
	}
	validationPassword := password
	if validationPassword == "" {
		validationPassword = currentPassword
	}
	if e := ValidateCredentials(username, validationPassword); e != nil {
		return fmt.Errorf("%w：%v", ErrCredentialsInput, e)
	}
	if password != "" {
		if credentials.Verify(hash, password) {
			return fmt.Errorf("%w：新密码不能与当前密码相同", ErrCredentialsInput)
		}
		hash = credentials.Hash(password)
	} else if username == old {
		return fmt.Errorf("%w：请修改用户名或填写新密码", ErrCredentialsInput)
	}
	tx, e := a.Store.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	res, e := tx.Exec(`UPDATE admin SET username=?,password_hash=?,version=version+1
		WHERE id=1 AND version=? AND must_change=0 AND EXISTS (
		SELECT 1 FROM sessions WHERE token_hash=? AND version=? AND restricted=0 AND expires>?)`,
		username, hash, s.Version, credentials.Digest(s.Token), s.Version, time.Now().Unix())
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return ErrSession
	}
	if _, e = tx.Exec("DELETE FROM sessions"); e != nil {
		return e
	}
	if e = store.Audit(tx, "admin.credentials", "administrator", "ok"); e != nil {
		return e
	}
	return tx.Commit()
}
func (b Bootstrap) String() string {
	return fmt.Sprintf("Username: %s\nTemporary password: %s\nExpires (UTC): %s\n", b.Username, b.Password, b.Expires.Format(time.RFC3339))
}
