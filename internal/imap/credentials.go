package imap

import (
	"mail-auto/internal/credentials"
	"mail-auto/internal/mail"
	"strconv"
)

func accountAuthKey(account mail.Account) string {
	return credentials.Digest(string(account.Secret) + "\x00" + account.SecretFile + "\x00" + account.Username + "\x00" + account.Host + "\x00" + strconv.Itoa(account.Port))
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
