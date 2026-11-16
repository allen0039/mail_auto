package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mail-auto/internal/credentials"
	"mail-auto/internal/mailoauth"
	"mail-auto/internal/store"
)

func TestOAuthApplicationExportCommand(t *testing.T) {
	dataDir, keyDir := t.TempDir(), t.TempDir()
	database, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	vault, err := credentials.Open(keyDir, true)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	manager := &mailoauth.Manager{Store: database, Vault: vault}
	if _, err = manager.SaveApp("google", mailoauth.AppInput{ClientID: "sample-cli-client", ClientSecret: "sample-cli-secret", Origin: "https://cli.test"}); err != nil {
		database.Close()
		t.Fatal(err)
	}
	database.Close()
	path := filepath.Join(t.TempDir(), "apps.json")
	args := []string{"--data-dir", dataDir, "--key-dir", keyDir, "mail-oauth", "export", path}
	if err = run(args); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var bundle struct {
		Version int                                   `json:"version"`
		Apps    map[string]map[string]json.RawMessage `json:"apps"`
	}
	if err = json.Unmarshal(payload, &bundle); err != nil || bundle.Version != 1 || len(bundle.Apps) != 1 || len(bundle.Apps["google"]) != 4 {
		t.Fatal("CLI export did not use portable application format", err)
	}
	if err = run(args); mailoauth.Code(err) != "oauth_bundle_export_failed" {
		t.Fatal("CLI overwrote private deployment export")
	}
}

func TestServeRejectsBadOAuthBundleBeforeWorkers(t *testing.T) {
	dataDir, keyDir := t.TempDir(), t.TempDir()
	path := filepath.Join(t.TempDir(), "apps.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"apps":{"google":{"client_id":"sample","client_secret":"sample-never-log","unexpected":"private-error-marker"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"--data-dir", dataDir, "--key-dir", keyDir, "--public-url", "https://cli.test", "--mail-oauth-apps-file", path, "--automatic-sync=false", "serve"})
	if mailoauth.Code(err) != "oauth_bundle_invalid" || strings.Contains(err.Error(), "private-error-marker") || strings.Contains(err.Error(), "sample-never-log") {
		t.Fatal("invalid app config did not safely stop startup")
	}
	database, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var count int
	if err = database.DB.QueryRow("SELECT count(*) FROM mail_oauth_apps").Scan(&count); err != nil || count != 0 {
		t.Fatal("invalid startup imported applications", err)
	}
}
