package mail

import (
	"database/sql"
	"mail-auto/internal/mailoauth"
)

func (service *Service) OAuthManager() *mailoauth.Manager {
	service.oauthOnce.Do(func() {
		service.oauth = &mailoauth.Manager{Store: service.Store, Vault: service.Vault, Network: service.Network, Changed: service.oauthChanged}
	})
	return service.oauth
}

func (service *Service) oauthChanged(id string) {
	service.mu.Lock()
	if service.epoch == nil {
		service.epoch = map[string]uint64{}
	}
	var closeSessions []*automaticSessionHandle
	if service.automatic != nil {
		for accountID, worker := range service.automatic.workers {
			if id != "" && id != accountID {
				continue
			}
			service.epoch[accountID]++
			worker.cancel()
			if worker.activeSession != nil {
				closeSessions = append(closeSessions, worker.activeSession)
			}
		}
	}
	service.mu.Unlock()
	for _, session := range closeSessions {
		session.Close()
	}
	if remote, ok := service.Remote.(interface{ InvalidateCredentials(string) }); ok {
		remote.InvalidateCredentials(id)
	}
	service.wakeAutomatic()
}

func (service *Service) accountOAuth(account *Account, query interface{ QueryRow(string, ...any) *sql.Row }) error {
	account.Authentication = "password"
	if !mailoauth.IsProvider(account.Provider) {
		return nil
	}
	account.Authentication, account.CredentialSource = "oauth2", "oauth2"
	var auth mailoauth.Account
	auth.ID, auth.Provider = account.ID, account.Provider
	err := query.QueryRow("SELECT generation,config_version,status,error_code FROM mail_oauth_accounts WHERE account_id=?", account.ID).Scan(&auth.Generation, &auth.ConfigVersion, &auth.Status, &auth.ErrorCode)
	if err != nil {
		return err
	}
	account.OAuth = &auth
	if auth.ConfigVersion == 0 {
		account.OAuth.Status = "configuration_missing"
	}
	account.AuthSource = service.OAuthManager()
	return nil
}

func (service *Service) checkSnapshot(account Account) error {
	current, err := service.Account(account.ID)
	if err != nil {
		return err
	}
	if accountIdentity(current) != accountIdentity(account) {
		return ErrConflict
	}
	return nil
}

func (service *Service) checkSnapshotIn(tx *sql.Tx, account Account) error {
	current, err := scanAccount(tx.QueryRow("SELECT "+accountColumns+" FROM accounts WHERE id=?", account.ID))
	if err != nil {
		return err
	}
	if err = service.accountNetworkIn(tx, &current); err != nil {
		return err
	}
	if accountIdentity(current) != accountIdentity(account) {
		return ErrConflict
	}
	return nil
}
