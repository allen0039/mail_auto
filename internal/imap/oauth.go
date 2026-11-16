package imap

import (
	"errors"
	"fmt"
	"mail-auto/internal/mail"
	"strconv"
	"strings"
)

type xoauth2 struct{ username, token string }

func (auth *xoauth2) Start() (string, []byte, error) {
	if auth.username == "" || auth.token == "" || strings.ContainsAny(auth.username+auth.token, "\x00\x01\r\n") {
		return "", nil, errors.New("invalid XOAUTH2 credential")
	}
	return "XOAUTH2", []byte("user=" + auth.username + "\x01auth=Bearer " + auth.token + "\x01\x01"), nil
}
func (auth *xoauth2) Next([]byte) ([]byte, error) { return []byte{}, nil }

func accountAuthKey(account mail.Account) string {
	if account.OAuth == nil {
		return "password"
	}
	return fmt.Sprintf("%d:%d", account.OAuth.Generation, account.OAuth.ConfigVersion)
}
func (adapter *Adapter) InvalidateCredentials(id string) {
	if adapter.pool == nil {
		return
	}
	adapter.pool.mu.Lock()
	defer adapter.pool.mu.Unlock()
	for accountID, conn := range adapter.pool.conns {
		if id == "" || id == accountID {
			conn.close()
			delete(adapter.pool.conns, accountID)
		}
	}
}

func gmailID(value any) string {
	var encoded string
	switch number := value.(type) {
	case string:
		encoded = number
	case uint64:
		encoded = strconv.FormatUint(number, 10)
	case uint32:
		encoded = strconv.FormatUint(uint64(number), 10)
	case int64:
		if number > 0 {
			encoded = strconv.FormatInt(number, 10)
		}
	case int:
		if number > 0 {
			encoded = strconv.Itoa(number)
		}
	}
	parsed, err := strconv.ParseUint(encoded, 10, 64)
	if err != nil || parsed == 0 || strconv.FormatUint(parsed, 10) != encoded {
		return ""
	}
	return encoded
}
