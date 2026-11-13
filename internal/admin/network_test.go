package admin

import (
	"encoding/json"
	"net"
	"net/http"
	"testing"

	"mail-auto/internal/network"
)

func TestNetworkAPIAccessRedactionAndDiagnosticFailure(t *testing.T) {
	client, server := loggedIn(t)
	anonymous := &browser{t: t, h: server.Handler(), cookies: map[string]*http.Cookie{}}
	if response := anonymous.request("GET", "/api/network", "", false); response.Code != 401 {
		t.Fatal("unauthenticated network access", response.Code)
	}
	request := client.request("GET", "/api/network", "", false)
	if request.Code != 200 {
		t.Fatal(request.Code, request.Body)
	}
	var settings network.Settings
	json.Unmarshal(request.Body.Bytes(), &settings)
	if settings.Global.Mode != "direct" || settings.Telegram.Mode != "inherit" || settings.Agent.Mode != "inherit" {
		t.Fatal("unexpected defaults", settings)
	}
	body := `{"version":0,"global":{"mode":"direct","proxy_id":""},"telegram":{"mode":"inherit","proxy_id":""},"agent":{"mode":"inherit","proxy_id":""},"profiles":[{"id":"nas","name":"NAS","protocol":"http","host":"127.0.0.1","port":1,"username":"user","password":"synthetic-network-password"}]}`
	if response := client.request("PUT", "/api/network", body, false); response.Code != 403 {
		t.Fatal("network update bypassed CSRF", response.Code)
	}
	response := client.request("PUT", "/api/network", body, true)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	var raw map[string]any
	json.Unmarshal(response.Body.Bytes(), &raw)
	profile := raw["profiles"].([]any)[0].(map[string]any)
	if _, exists := profile["password"]; exists || profile["password_set"] != true {
		t.Fatal("credential not redacted", profile)
	}
	if response = client.request("PUT", "/api/network", body, true); response.Code != 409 {
		t.Fatal("stale save accepted", response.Code)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().(*net.TCPAddr)
	port := address.Port
	listener.Close()
	settings, _ = server.Mail.Network.Settings()
	input := network.Input{Version: settings.Version, Global: settings.Global, Telegram: settings.Telegram, Agent: settings.Agent,
		Profiles: []network.ProfileInput{{Profile: network.Profile{ID: "nas", Name: "NAS", Protocol: "http", Host: "127.0.0.1", Port: port}}}}
	input.Profiles[0].ClearPassword = true
	if _, err = server.Mail.Network.Save(input); err != nil {
		t.Fatal(err)
	}
	response = client.request("POST", "/api/network/check", `{"scope":"global","mode":"proxy","proxy_id":"nas"}`, true)
	if response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body)
	}
	var result struct {
		OK       bool            `json:"ok"`
		Fallback bool            `json:"fallback"`
		Checks   []network.Check `json:"checks"`
	}
	if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.OK || result.Fallback || len(result.Checks) != 1 || result.Checks[0].Code != "proxy_connection_failed" {
		t.Fatal("failed proxy diagnostic", response.Body)
	}
}
