package mail

import "database/sql"

func (service *Service) credentialsChanged(id string) {
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
