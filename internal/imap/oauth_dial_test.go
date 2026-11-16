package imap

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
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

	"mail-auto/internal/mail"
	"mail-auto/internal/mailoauth"
	"mail-auto/internal/network"
)

type rotatingCredential struct{ calls int }

func (source *rotatingCredential) Credential(_ context.Context, _ string, route string) (mailoauth.Credential, error) {
	if route != "fixture-route" {
		return mailoauth.Credential{}, fmt.Errorf("unexpected route")
	}
	source.calls++
	return mailoauth.Credential{Mechanism: "XOAUTH2", Username: "fixture@example.test", Token: fmt.Sprintf("fixture-access-%d", source.calls)}, nil
}

func TestOAuthDialAndDiagnosticRefreshOnReconnect(t *testing.T) {
	if endpoint := os.Getenv("MAIL_AUTO_TEST_OAUTH_PROXY"); endpoint != "" {
		host, portText, err := net.SplitHostPort(endpoint)
		if err != nil {
			t.Fatal(err)
		}
		port, _ := strconv.Atoi(portText)
		route := network.Route{Key: "fixture-route", Profile: &network.Profile{Protocol: "http", Host: host, Port: port}}
		source := &rotatingCredential{}
		for _, provider := range []string{"google", "microsoft"} {
			account := mail.Account{ID: "fixture", Provider: provider, Host: os.Getenv("MAIL_AUTO_TEST_OAUTH_HOST"), Port: 993, Username: "fixture@example.test", AuthSource: source}
			adapter := Adapter{connect: func(ctx context.Context, current mail.Account, password string) (*connection, error) {
				return dialRoute(ctx, current, password, route)
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			folders, err := adapter.Folders(ctx, account, "password-must-not-be-used")
			if err != nil || len(folders) != 1 || folders[0] != "INBOX" {
				cancel()
				t.Fatal("OAuth central connection or retry failed", folders, err)
			}
			folders, err = adapter.DiagnoseRoute(ctx, account, "password-must-not-be-used", route)
			cancel()
			if err != nil || len(folders) != 1 || folders[0] != "INBOX" {
				t.Fatal("standalone OAuth diagnostic failed", folders, err)
			}
		}
		if source.calls != 5 {
			t.Fatal("reconnect reused captured credential", source.calls)
		}
		return
	}
	seed := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	certificate, tlsConfig := seed.Certificate(), seed.TLS.Clone()
	seed.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	secured := tls.NewListener(listener, tlsConfig)
	defer secured.Close()
	var authenticated, tunnels atomic.Int32
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for {
			connection, err := secured.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				connection.SetDeadline(time.Now().Add(10 * time.Second))
				fmt.Fprint(connection, "* OK [CAPABILITY IMAP4rev1 AUTH=XOAUTH2 X-GM-EXT-1] fixture\r\n")
				reader := bufio.NewReader(connection)
				sequence := int32(0)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					parts := strings.Fields(line)
					if len(parts) < 2 {
						return
					}
					tag := parts[0]
					switch parts[1] {
					case "AUTHENTICATE":
						if len(parts) != 3 || parts[2] != "XOAUTH2" {
							t.Error("unexpected authentication mechanism")
							return
						}
						fmt.Fprint(connection, "+ \r\n")
						encoded, _ := reader.ReadString('\n')
						frame, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
						sequence = authenticated.Add(1)
						expected := fmt.Sprintf("user=fixture@example.test\x01auth=Bearer fixture-access-%d\x01\x01", sequence)
						if err != nil || string(frame) != expected {
							t.Error("reconnect did not acquire the next credential")
							return
						}
						fmt.Fprintf(connection, "%s OK authenticated\r\n", tag)
					case "CAPABILITY":
						fmt.Fprintf(connection, "* CAPABILITY IMAP4rev1 X-GM-EXT-1\r\n%s OK capability\r\n", tag)
					case "LIST":
						if sequence == 1 {
							return
						}
						fmt.Fprintf(connection, "* LIST () \"/\" \"INBOX\"\r\n%s OK list\r\n", tag)
					default:
						t.Error("central OAuth path issued an unexpected command")
						return
					}
				}
			}()
		}
	}()
	defer func() { secured.Close(); <-serverDone }()
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != "CONNECT" || request.Host != net.JoinHostPort(certificate.DNSNames[0], "993") {
			writer.WriteHeader(400)
			return
		}
		target, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
		if err != nil {
			writer.WriteHeader(502)
			return
		}
		defer target.Close()
		connection, buffered, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer connection.Close()
		tunnels.Add(1)
		buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		buffered.Flush()
		go io.Copy(target, connection)
		io.Copy(connection, target)
	}))
	defer proxy.Close()
	certPath := filepath.Join(t.TempDir(), "oauth-ca.pem")
	if err = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestOAuthDialAndDiagnosticRefreshOnReconnect$", "-test.timeout=30s")
	command.Env = append(os.Environ(), "SSL_CERT_FILE="+certPath, "MAIL_AUTO_TEST_OAUTH_PROXY="+strings.TrimPrefix(proxy.URL, "http://"), "MAIL_AUTO_TEST_OAUTH_HOST="+certificate.DNSNames[0])
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	if tunnels.Load() != 5 || authenticated.Load() != 5 {
		t.Fatal("OAuth route or authentication paths missing", tunnels.Load(), authenticated.Load())
	}
}

func TestOAuthRejectsWrongMechanismBeforeNetwork(t *testing.T) {
	account := mail.Account{Provider: "microsoft", Host: "127.0.0.1", Port: 1, AuthSource: wrongMechanismCredential{}}
	if _, err := dialRoute(context.Background(), account, "", network.Route{Key: "direct"}); mailoauth.Code(err) != "oauth_imap_denied" {
		t.Fatal("OAUTHBEARER accepted in XOAUTH2 path", err)
	}
}

type wrongMechanismCredential struct{}

func (wrongMechanismCredential) Credential(context.Context, string, string) (mailoauth.Credential, error) {
	return mailoauth.Credential{Mechanism: "OAUTHBEARER", Username: "fixture@example.test", Token: "fixture-access"}, nil
}
