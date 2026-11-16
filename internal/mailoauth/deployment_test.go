package mailoauth

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func deploymentFile(t *testing.T, bundle deploymentApps) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oauth-apps.json")
	payload, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, payload, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func deploymentFixture() deploymentApps {
	return deploymentApps{Version: 1, Apps: map[string]deploymentApp{
		"google":    {ClientID: "shared-google-app", ClientSecret: "sample-google-secret", Authority: "consumers"},
		"microsoft": {ClientID: "shared-microsoft-app", ClientSecret: "sample-microsoft-secret", Authority: "consumers", SecretExpires: time.Now().Add(24 * time.Hour).Unix()},
	}}
}

func TestDeploymentAppsExportAndReuse(t *testing.T) {
	source, session := fixture(t, "google")
	bindFixture(t, source, session, "google")
	if _, err := source.SaveApp("microsoft", AppInput{ClientID: "shared-microsoft-app", ClientSecret: "sample-microsoft-secret", Origin: "https://original.test", SecretExpires: time.Now().Add(time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "oauth-apps.json")
	if err := source.ExportApps(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("export permissions", err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"fixture-refresh", "fixture-subject", "fixture@example.test", "https://original.test", "refresh", "access_token", "origin"} {
		if bytes.Contains(payload, []byte(forbidden)) {
			t.Fatal("deployment export contains mailbox data or deployment origin")
		}
	}
	target, _ := fixture(t, "microsoft")
	if err = target.ProvisionApps(path, "https://new-deployment.test"); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"google", "microsoft"} {
		original, err := source.App(provider)
		if err != nil {
			t.Fatal(err)
		}
		imported, err := target.App(provider)
		if err != nil || !imported.Configured || imported.ClientID != original.ClientID || imported.Callback != "https://new-deployment.test/api/mail-oauth/"+provider+"/callback" || imported.SecretExpires != original.SecretExpires {
			t.Fatal("application identity or callback not preserved", err)
		}
		plain, err := target.Vault.Decrypt(imported.Secret, "mail-oauth-app:"+provider)
		expected, originalErr := source.Vault.Decrypt(original.Secret, "mail-oauth-app:"+provider)
		if err != nil || originalErr != nil || plain != expected || bytes.Equal(imported.Secret, original.Secret) {
			t.Fatal("secret not re-encrypted with destination vault")
		}
		encoded, err := json.Marshal(imported)
		if err != nil || bytes.Contains(encoded, []byte(plain)) {
			t.Fatal("application response leaks secret")
		}
	}
	before := append([]byte(nil), payload...)
	if err = source.ExportApps(path); Code(err) != "oauth_bundle_export_failed" {
		t.Fatal("existing export overwritten")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("existing export changed", err)
	}
	if err = source.ExportApps("-"); Code(err) != "oauth_bundle_export_failed" {
		t.Fatal("stdout export accepted")
	}
}

func TestDeploymentAppsRestartAndRotation(t *testing.T) {
	for _, change := range []string{"unchanged", "secret", "origin", "client", "expiry", "authority"} {
		t.Run(change, func(t *testing.T) {
			manager, session := fixture(t, "microsoft")
			bindFixture(t, manager, session, "microsoft")
			path := filepath.Join(t.TempDir(), "oauth-apps.json")
			if err := manager.ExportApps(path); err != nil {
				t.Fatal(err)
			}
			bundle, err := readDeploymentApps(path)
			if err != nil {
				t.Fatal(err)
			}
			app := bundle.Apps["microsoft"]
			origin := "https://mail.example.test"
			switch change {
			case "secret":
				app.ClientSecret = "rotated-sample-secret"
			case "origin":
				origin = "https://new-deployment.test"
			case "client":
				app.ClientID = "replacement-app"
			case "expiry":
				app.SecretExpires = time.Now().Add(time.Hour).Unix()
			case "authority":
				app.Authority = "common"
			}
			bundle.Apps["microsoft"] = app
			path = deploymentFile(t, bundle)
			before, err := manager.Account("account")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = manager.Start(context.Background(), "account", "microsoft", session); err != nil {
				t.Fatal(err)
			}
			invalidated := 0
			manager.Changed = func(string) { invalidated++ }
			if err = manager.ProvisionApps(path, origin); err != nil {
				t.Fatal(err)
			}
			after, err := manager.Account("account")
			if err != nil {
				t.Fatal(err)
			}
			var states int
			if err = manager.Store.DB.QueryRow("SELECT count(*) FROM mail_oauth_states").Scan(&states); err != nil {
				t.Fatal(err)
			}
			if change == "unchanged" {
				if after.Generation != before.Generation || after.ConfigVersion != before.ConfigVersion || !bytes.Equal(after.Refresh, before.Refresh) || states != 1 || invalidated != 0 {
					t.Fatal("restart invalidated active authorization")
				}
			} else {
				if after.Generation != before.Generation+1 || after.ConfigVersion != before.ConfigVersion+1 || states != 0 || invalidated != 1 {
					t.Fatal("config change did not invalidate old state and workers")
				}
				if change == "secret" || change == "expiry" {
					if !bytes.Equal(after.Refresh, before.Refresh) || after.Status != before.Status {
						t.Fatal("credential rotation lost mailbox authorization")
					}
				} else if len(after.Refresh) != 0 || after.Status != "reauthorize" {
					t.Fatal("identity change silently reused mailbox authorization")
				}
			}
			version := after.ConfigVersion
			if err = manager.ProvisionApps(path, origin); err != nil {
				t.Fatal(err)
			}
			again, err := manager.Account("account")
			if err != nil || again.ConfigVersion != version || again.Generation != after.Generation {
				t.Fatal("repeat provisioning not idempotent", err)
			}
		})
	}
}

func TestDeploymentAppsFailClosedAndRollback(t *testing.T) {
	for _, kind := range []string{"unknown_provider", "empty_secret", "invalid_authority", "expired", "negative_expiry", "http", "path_origin", "missing_origin", "missing_file", "world_readable", "symlink", "directory", "oversized", "unknown_field", "extra_document", "version", "empty", "database_failure"} {
		t.Run(kind, func(t *testing.T) {
			manager, session := fixture(t, "google")
			before, err := manager.App("google")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = manager.Start(context.Background(), "account", "google", session); err != nil {
				t.Fatal(err)
			}
			bundle := deploymentFixture()
			app := bundle.Apps["microsoft"]
			origin := "https://mail.example.test"
			switch kind {
			case "unknown_provider":
				delete(bundle.Apps, "microsoft")
				bundle.Apps["untrusted"] = app
			case "empty_secret":
				app.ClientSecret = ""
			case "invalid_authority":
				app.Authority = "arbitrary-tenant"
			case "expired":
				app.SecretExpires = time.Now().Add(-time.Hour).Unix()
			case "negative_expiry":
				app.SecretExpires = -1
			case "http":
				origin = "http://mail.example.test"
			case "path_origin":
				origin += "/path"
			case "missing_origin":
				origin = ""
			case "version":
				bundle.Version = 2
			case "empty":
				bundle.Apps = nil
			}
			if _, exists := bundle.Apps["microsoft"]; exists {
				bundle.Apps["microsoft"] = app
			}
			path := deploymentFile(t, bundle)
			switch kind {
			case "missing_file":
				path += ".absent"
			case "world_readable":
				err = os.Chmod(path, 0644)
			case "symlink":
				link := filepath.Join(t.TempDir(), "link")
				err = os.Symlink(path, link)
				path = link
			case "directory":
				path = t.TempDir()
			case "oversized":
				err = os.WriteFile(path, bytes.Repeat([]byte("x"), 32769), 0600)
			case "unknown_field":
				err = os.WriteFile(path, []byte(`{"version":1,"apps":{"google":{"client_id":"sample","client_secret":"do-not-leak-this-marker","token_endpoint":"https://untrusted.test"}}}`), 0600)
			case "extra_document":
				file, openErr := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				if openErr != nil {
					t.Fatal(openErr)
				}
				_, err = file.WriteString("\n{}")
				file.Close()
			case "database_failure":
				_, err = manager.Store.DB.Exec(`CREATE TRIGGER reject_microsoft BEFORE INSERT ON mail_oauth_apps WHEN NEW.provider='microsoft' BEGIN SELECT RAISE(ABORT,'do-not-leak-this-marker'); END`)
			}
			if err != nil {
				t.Fatal(err)
			}
			invalidations := 0
			manager.Changed = func(string) { invalidations++ }
			err = manager.ProvisionApps(path, origin)
			if err == nil || strings.Contains(err.Error(), "do-not-leak-this-marker") || strings.Contains(err.Error(), "sample-google-secret") {
				t.Fatal("unsafe deployment file accepted or sensitive error returned")
			}
			after, readErr := manager.App("google")
			if readErr != nil || after.Version != before.Version || after.ClientID != before.ClientID || !bytes.Equal(after.Secret, before.Secret) || invalidations != 0 {
				t.Fatal("failed import changed existing application", readErr)
			}
			var states, apps int
			if err = manager.Store.DB.QueryRow("SELECT count(*) FROM mail_oauth_states").Scan(&states); err != nil {
				t.Fatal(err)
			}
			if err = manager.Store.DB.QueryRow("SELECT count(*) FROM mail_oauth_apps").Scan(&apps); err != nil || states != 1 || apps != 1 {
				t.Fatal("failed import was not fully rolled back", err)
			}
		})
	}
}

func TestDeploymentAppsOmittedProviderPreserved(t *testing.T) {
	manager, _ := fixture(t, "microsoft")
	before, err := manager.App("microsoft")
	if err != nil {
		t.Fatal(err)
	}
	bundle := deploymentFixture()
	delete(bundle.Apps, "microsoft")
	if err = manager.ProvisionApps(deploymentFile(t, bundle), "https://mail.example.test"); err != nil {
		t.Fatal(err)
	}
	after, err := manager.App("microsoft")
	if err != nil || before.Version != after.Version || !bytes.Equal(before.Secret, after.Secret) {
		t.Fatal("omitted provider was changed", err)
	}
}
