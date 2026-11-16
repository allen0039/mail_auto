package mail

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"mail-auto/internal/mailoauth"
	"mail-auto/internal/network"
)

func (s *Service) accountNetwork(account *Account) error {
	if err := s.accountOAuth(account, s.Store.DB); err != nil {
		return err
	}
	if s.Network == nil {
		return nil
	}
	policy, err := s.Network.AccountPolicy(account.ID)
	if err != nil {
		return err
	}
	route, err := s.Network.Resolve("account", account.ID)
	if err != nil {
		return err
	}
	account.Proxy = policy
	account.NetworkRevision = route.Key
	account.NetworkLabel = route.Label()
	return nil
}

func (s *Service) accountNetworkIn(tx *sql.Tx, account *Account) error {
	if err := s.accountOAuth(account, tx); err != nil {
		return err
	}
	if s.Network == nil {
		return nil
	}
	route, err := s.Network.ResolveIn(tx, "account", account.ID)
	if err != nil {
		return err
	}
	policy := network.Policy{Mode: "inherit"}
	err = tx.QueryRow("SELECT mode,proxy_id FROM account_network WHERE account_id=?", account.ID).Scan(&policy.Mode, &policy.ProxyID)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	account.Proxy, account.NetworkRevision, account.NetworkLabel = policy, route.Key, route.Label()
	return nil
}

func (s *Service) NetworkChanged() { s.OAuthManager().Invalidate("") }

func (s *Service) DiagnoseNetwork(ctx context.Context, id string, route network.Route) ([]string, error) {
	release, err := s.acquire(id)
	if err != nil {
		return nil, err
	}
	defer release()
	account, err := s.Account(id)
	if err != nil {
		return nil, err
	}
	if account.OAuth != nil {
		account.AuthSource = &diagnosticCredential{manager: s.OAuthManager(), route: route}
	}
	password, err := s.password(account)
	if err != nil {
		return nil, err
	}
	remote, ok := s.Remote.(interface {
		DiagnoseRoute(context.Context, Account, string, network.Route) ([]string, error)
	})
	if !ok {
		return nil, ErrCapability
	}
	return remote.DiagnoseRoute(ctx, account, password, route)
}

type diagnosticCredential struct {
	manager *mailoauth.Manager
	route   network.Route
}

func (source *diagnosticCredential) Credential(ctx context.Context, id, expectedRoute string) (mailoauth.Credential, error) {
	if expectedRoute != source.route.Key {
		return mailoauth.Credential{}, mailoauth.Failure("oauth_config_changed")
	}
	return source.manager.CredentialForRoute(ctx, id, source.route)
}

func (s *Service) TelegramHTTP(timeout time.Duration) (*http.Client, error) {
	route := network.Route{Key: "direct"}
	var err error
	if s.Network != nil {
		route, err = s.Network.Resolve("telegram", "")
		if err != nil {
			return nil, err
		}
		return network.HTTPClient(s.Network.Transport(route), timeout), nil
	}
	return network.HTTPClient(route.Transport(), timeout), nil
}
