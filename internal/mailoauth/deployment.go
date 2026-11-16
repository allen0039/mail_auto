package mailoauth

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"time"
)

type deploymentApp struct {
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	Authority     string `json:"authority"`
	SecretExpires int64  `json:"secret_expires"`
}

type deploymentApps struct {
	Version int                      `json:"version"`
	Apps    map[string]deploymentApp `json:"apps"`
}

func readDeploymentApps(path string) (deploymentApps, error) {
	var bundle deploymentApps
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return bundle, Failure("oauth_bundle_unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return bundle, Failure("oauth_bundle_unavailable")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0077 != 0 {
		return bundle, Failure("oauth_bundle_unavailable")
	}
	payload, err := io.ReadAll(io.LimitReader(file, 32769))
	if err != nil || len(payload) > 32768 {
		return bundle, Failure("oauth_bundle_invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&bundle); err != nil || bundle.Version != 1 || len(bundle.Apps) == 0 || len(bundle.Apps) > 2 {
		return bundle, Failure("oauth_bundle_invalid")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return bundle, Failure("oauth_bundle_invalid")
	}
	return bundle, nil
}

func (manager *Manager) ProvisionApps(path, origin string) error {
	bundle, err := readDeploymentApps(path)
	if err != nil {
		return err
	}
	inputs := map[string]AppInput{}
	for provider, app := range bundle.Apps {
		input, normalizeErr := normalizeAppInput(provider, AppInput{ClientID: app.ClientID, ClientSecret: app.ClientSecret, Authority: app.Authority, SecretExpires: app.SecretExpires, Origin: origin})
		if normalizeErr != nil || input.ClientSecret == "" || input.SecretExpires < 0 || (input.SecretExpires > 0 && input.SecretExpires <= time.Now().Unix()) {
			return Failure("oauth_bundle_invalid")
		}
		inputs[provider] = input
	}
	tx, err := manager.Store.DB.Begin()
	if err != nil {
		return Failure("oauth_bundle_apply_failed")
	}
	defer tx.Rollback()
	changed := false
	for _, provider := range []string{"google", "microsoft"} {
		input, exists := inputs[provider]
		if !exists {
			continue
		}
		updated, saveErr := manager.saveAppIn(tx, provider, input, true)
		if saveErr != nil {
			return Failure("oauth_bundle_apply_failed")
		}
		changed = changed || updated
	}
	if err = tx.Commit(); err != nil {
		return Failure("oauth_bundle_apply_failed")
	}
	if changed {
		manager.Invalidate("")
	}
	return nil
}

func (manager *Manager) ExportApps(path string) error {
	if path == "" || path == "-" {
		return Failure("oauth_bundle_export_failed")
	}
	bundle := deploymentApps{Version: 1, Apps: map[string]deploymentApp{}}
	tx, err := manager.Store.DB.Begin()
	if err != nil {
		return Failure("oauth_bundle_export_failed")
	}
	defer tx.Rollback()
	rows, err := tx.Query("SELECT provider,client_id,secret,authority,secret_expires FROM mail_oauth_apps ORDER BY provider")
	if err != nil {
		return Failure("oauth_bundle_export_failed")
	}
	defer rows.Close()
	for rows.Next() {
		var provider string
		var encrypted []byte
		var app deploymentApp
		if err = rows.Scan(&provider, &app.ClientID, &encrypted, &app.Authority, &app.SecretExpires); err != nil || !IsProvider(provider) {
			return Failure("oauth_bundle_export_failed")
		}
		app.ClientSecret, err = manager.Vault.Decrypt(encrypted, "mail-oauth-app:"+provider)
		if err != nil {
			return Failure("oauth_bundle_export_failed")
		}
		bundle.Apps[provider] = app
	}
	if rows.Err() != nil || len(bundle.Apps) == 0 {
		return Failure("oauth_bundle_export_failed")
	}
	if err = tx.Commit(); err != nil {
		return Failure("oauth_bundle_export_failed")
	}
	payload, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil || len(payload)+1 > 32768 {
		return Failure("oauth_bundle_export_failed")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return Failure("oauth_bundle_export_failed")
	}
	complete := false
	defer func() {
		file.Close()
		if !complete {
			os.Remove(path)
		}
	}()
	if _, err = file.Write(append(payload, '\n')); err != nil || file.Sync() != nil || file.Close() != nil {
		return Failure("oauth_bundle_export_failed")
	}
	complete = true
	return nil
}
