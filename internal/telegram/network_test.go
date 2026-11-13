package telegram

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mail-auto/internal/network"
)

func TestSharedProxyCoversAllBotMethodsAndPreservesBinding(t *testing.T) {
	env := newTGEnv(t, nil)
	env.bind(t)
	env.f.Service.Network = network.New(env.f.Store, env.f.Vault)
	var tunnels atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != "CONNECT" {
			w.WriteHeader(400)
			return
		}
		tunnels.Add(1)
		target, err := net.DialTimeout("tcp", request.Host, time.Second)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer target.Close()
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		buffered.Flush()
		go io.Copy(target, conn)
		io.Copy(conn, target)
	}))
	defer proxy.Close()
	host, portString, _ := net.SplitHostPort(strings.TrimPrefix(proxy.URL, "http://"))
	port, _ := strconv.Atoi(portString)
	settings, _ := env.f.Service.Network.Settings()
	in := network.Input{Version: settings.Version, Global: settings.Global, Agent: settings.Agent, Telegram: network.Policy{Mode: "proxy", ProxyID: "p"},
		Profiles: []network.ProfileInput{{Profile: network.Profile{ID: "p", Name: "TG proxy", Protocol: "http", Host: host, Port: port}}}}
	old, err := env.tg.Get(env.f.Admin)
	if err != nil || old.Binding == nil {
		t.Fatal("fixture not bound", err)
	}
	notification, _ := env.f.Service.Notifications(env.f.Admin)
	if _, err = env.f.Service.Network.Save(in); err != nil {
		t.Fatal(err)
	}
	production := New(env.f.Store, env.f.Vault, env.f.Service, env.agent)
	client := production.NewClient(testToken)
	client.BaseURL = env.srv.URL
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err = client.GetMe(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = client.GetUpdates(ctx, 0, 0); err != nil {
		t.Fatal(err)
	}
	messageID, err := client.SendMessage(ctx, 42, "proxy test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.EditMessage(ctx, 42, messageID, "edit", nil); err != nil {
		t.Fatal(err)
	}
	if err = client.AnswerCallback(ctx, "callback", "ok"); err != nil {
		t.Fatal(err)
	}
	notificationHTTP, err := env.f.Service.TelegramHTTP(10 * time.Second)
	interactionHTTP, err2 := client.ResolveHTTP()
	if err != nil || err2 != nil || notificationHTTP.Transport != interactionHTTP.Transport || tunnels.Load() == 0 {
		t.Fatal("notification and interaction do not share the same proxy", err, err2)
	}
	current, _ := env.tg.Get(env.f.Admin)
	after, _ := env.f.Service.Notifications(env.f.Admin)
	if current.Binding == nil || current.Binding.ID != old.Binding.ID || current.Epoch != old.Epoch || after.Version != notification.Version || after.TokenGeneration != notification.TokenGeneration {
		t.Fatal("proxy save changed binding/token/channel generation")
	}
	env.f.Service.Network.CloseIdle()
}
