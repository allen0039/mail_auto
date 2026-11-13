package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	_ "time/tzdata"

	"mail-auto/internal/admin"
	"mail-auto/internal/agent"
	"mail-auto/internal/config"
	"mail-auto/internal/credentials"
	remote "mail-auto/internal/imap"
	"mail-auto/internal/mail"
	"mail-auto/internal/network"
	"mail-auto/internal/store"
	"mail-auto/internal/telegram"
)

func main() {
	if e := run(os.Args[1:]); e != nil {
		slog.Error("mail-auto stopped", "error", e)
		os.Exit(1)
	}
}
func run(args []string) error {
	c, cmd, e := config.Load(args)
	if e != nil {
		return e
	}
	if len(cmd) == 1 && cmd[0] == "version" {
		fmt.Println(admin.Version)
		return nil
	}
	if len(cmd) == 1 && cmd[0] == "healthcheck" {
		client := network.HTTPClient((network.Route{}).Transport(), 3*time.Second)
		host, port, _ := net.SplitHostPort(c.Listen)
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		resp, e := client.Get("http://" + net.JoinHostPort(host, port) + "/readyz")
		if e != nil {
			return e
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return errors.New("health check failed")
		}
		return nil
	}
	isServe := len(cmd) == 0 || (len(cmd) == 1 && cmd[0] == "serve")
	isShow := len(cmd) == 2 && cmd[0] == "bootstrap" && cmd[1] == "show"
	isRecover := len(cmd) == 2 && cmd[0] == "bootstrap" && cmd[1] == "recover"
	isBackup := len(cmd) == 2 && cmd[0] == "backup"
	if !isServe && !isShow && !isRecover && !isBackup {
		return errors.New("usage: mail-auto [flags] [serve | version | bootstrap show | bootstrap recover | backup PATH | healthcheck]")
	}
	if !isServe {
		if _, e = os.Stat(filepath.Join(c.DataDir, "mail-auto.db")); e != nil {
			return errors.New("instance database not found; start the service first")
		}
	}
	st, e := store.Open(c.DataDir)
	if e != nil {
		return e
	}
	defer st.Close()
	initialized, e := st.Initialized()
	if e != nil {
		return e
	}
	vault, e := credentials.Open(c.KeyDir, isServe && !initialized)
	if e != nil {
		return e
	}
	auth := &admin.Auth{Store: st, Vault: vault, TTL: c.SessionTTL()}
	if e = auth.Initialize(); e != nil {
		return e
	}
	if isShow {
		b, e := auth.Show()
		if e != nil {
			return e
		}
		fmt.Print(b.String())
		return nil
	}
	if isRecover {
		if e = auth.Recover(); e != nil {
			return e
		}
		if e = telegram.RecoverOffline(st); e != nil {
			return e
		}
		fmt.Println("All sessions revoked. Telegram bindings, pending agent proposals and queued agent work were revoked; re-bind after recovery. New temporary credentials are available via bootstrap show.")
		return nil
	}
	if isBackup {
		if e = st.Backup(cmd[1]); e != nil {
			return e
		}
		fmt.Println("Consistent database snapshot created. Back up master.key separately.")
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	networks := network.New(st, vault)
	defer networks.CloseIdle()
	adapter := remote.NewAdapter()
	adapter.Network = networks
	service := &mail.Service{Store: st, Vault: vault, Network: networks, Remote: adapter, SecretDir: c.SecretDir, Context: ctx, NotificationPublicURL: c.PublicURL}
	if e = service.RecoverSync(ctx); e != nil {
		return e
	}
	defer service.Stop()
	if e = service.StartOperations(); e != nil {
		return e
	}
	if e = service.StartTaskAutomation(); e != nil {
		return e
	}
	if c.AutomaticSync {
		if e = service.StartAutomaticSync(time.Duration(c.SyncIntervalSeconds) * time.Second); e != nil {
			return e
		}
	}
	// The agent and Telegram workers start idle: nothing polls, calls a model
	// or writes until an administrator enables and binds them.
	runtime := agent.New(st, vault, service)
	interactive := telegram.New(st, vault, service, runtime)
	runtime.Start(ctx)
	interactive.Start(ctx)
	defer func() {
		runtime.Wait()
		interactive.Wait()
		interactive.ReleaseLease()
	}()
	adminServer := admin.New(auth, service, c)
	adminServer.Agent, adminServer.Telegram = runtime, interactive
	handler := adminServer.Handler()
	server := &http.Server{Addr: c.Listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 130 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	cleanupDone := make(chan struct{})
	go func() {
		defer close(cleanupDone)
		st.Maintain(ctx)
	}()
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	slog.Info("mail-auto started", "version", admin.Version, "listen", c.Listen, "mode", "controlled-organizer", "mcp_enabled", c.MCPEnabled)
	if _, e = auth.Show(); e == nil {
		slog.Info("Initial credentials available locally: mail-auto bootstrap show; first login requires changing username and password")
	}
	select {
	case e = <-errCh:
		stop()
		<-cleanupDone
		if !errors.Is(e, http.ErrServerClosed) {
			return e
		}
		return nil
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		e = server.Shutdown(shutdown)
		if e != nil {
			_ = server.Close()
		}
		<-cleanupDone
		return e
	}
}
