package imap

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/server"
	"mail-auto/internal/credentials"
	"mail-auto/internal/mail"
	"mail-auto/internal/network"
	"mail-auto/internal/store"
)

func TestIMAPTLSAndAutomaticSyncThroughProxy(t *testing.T) {
	if endpoint := os.Getenv("MAIL_AUTO_TEST_IMAP_PROXY"); endpoint != "" {
		database, err := store.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer database.Close()
		vault, err := credentials.Open(t.TempDir(), true)
		if err != nil {
			t.Fatal(err)
		}
		manager := network.New(database, vault)
		defer manager.CloseIdle()
		host, portString, _ := net.SplitHostPort(endpoint)
		port, _ := strconv.Atoi(portString)
		settings, _ := manager.Settings()
		_, err = manager.Save(network.Input{
			Global:   network.Policy{Mode: "proxy", ProxyID: "test"},
			Telegram: settings.Telegram, Agent: settings.Agent,
			Profiles: []network.ProfileInput{{Profile: network.Profile{ID: "test", Name: "IMAP test", Host: host, Port: port, Protocol: "http"}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		adapter := NewAdapter()
		adapter.Network = manager
		account := mail.Account{ID: "test-account", Host: os.Getenv("MAIL_AUTO_TEST_IMAP_HOST"), Port: 993, Username: "username"}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		folders, err := adapter.Folders(ctx, account, "password")
		if err != nil || len(folders) != 1 || folders[0] != "INBOX" {
			t.Fatal("proxy login/list", folders, err)
		}
		var messages []mail.Message
		err = adapter.Scan(ctx, account, "password", "INBOX", func(snapshot mail.Snapshot) error {
			messages = append(messages, snapshot.Messages...)
			return nil
		})
		if err != nil || len(messages) == 0 {
			t.Fatal("proxy metadata scan", err)
		}
		content, err := adapter.Read(ctx, account, "password", messages[0])
		if err != nil || content.Text == "" {
			t.Fatal("proxy read", err)
		}
		session, err := adapter.OpenAutomatic(ctx, account, "password")
		if err != nil {
			t.Fatal("proxy automatic session", err)
		}
		defer session.Close()
		if _, err = session.Folders(ctx); err != nil {
			t.Fatal(err)
		}
		inventory, err := session.Refresh(ctx, "INBOX", mail.FolderState{}, func(mail.Snapshot) error { return nil })
		if err != nil || inventory.Total == 0 {
			t.Fatal("proxy automatic refresh", err)
		}
		return
	}
	seed := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	certificate := seed.Certificate()
	tlsConfig := seed.TLS.Clone()
	seed.Close()
	if len(certificate.DNSNames) == 0 {
		t.Fatal("TLS fixture has no DNS name")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	imapServer := server.New(memory.New())
	imapServer.ErrorLog = log.New(io.Discard, "", 0)
	done := make(chan error, 1)
	go func() { done <- imapServer.Serve(tls.NewListener(listener, tlsConfig)) }()
	t.Cleanup(func() { imapServer.Close(); listener.Close(); <-done })
	var tunnels atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != "CONNECT" || request.Host != net.JoinHostPort(certificate.DNSNames[0], "993") {
			w.WriteHeader(400)
			return
		}
		target, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
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
		tunnels.Add(1)
		buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		buffered.Flush()
		go io.Copy(target, conn)
		io.Copy(conn, target)
	}))
	defer proxy.Close()
	certPath := filepath.Join(t.TempDir(), "imap-ca.pem")
	if err = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestIMAPTLSAndAutomaticSyncThroughProxy$", "-test.timeout=20s")
	command.Env = append(os.Environ(), "SSL_CERT_FILE="+certPath, "MAIL_AUTO_TEST_IMAP_PROXY="+strings.TrimPrefix(proxy.URL, "http://"), "MAIL_AUTO_TEST_IMAP_HOST="+certificate.DNSNames[0])
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	if tunnels.Load() != 4 {
		t.Fatal("not all IMAP paths used proxy", tunnels.Load())
	}
}
