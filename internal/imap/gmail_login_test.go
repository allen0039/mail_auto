package imap

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
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
	"testing"
	"time"

	"mail-auto/internal/mail"
	"mail-auto/internal/network"
)

func TestGmailPasswordLoginDistinguishesCredentialsTLSAndPoolKey(t *testing.T) {
	seed := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	certificate, tlsConfig := seed.Certificate(), seed.TLS.Clone()
	seed.Close()
	certPath := filepath.Join(t.TempDir(), "gmail-ca.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("MAIL_AUTO_TEST_GMAIL_HOST") == "" {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		secured := tls.NewListener(listener, tlsConfig)
		defer secured.Close()
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				conn, err := secured.Accept()
				if err != nil {
					return
				}
				go func(c net.Conn) {
					defer c.Close()
					_ = c.SetDeadline(time.Now().Add(5 * time.Second))
					_, _ = fmt.Fprint(c, "* OK [CAPABILITY IMAP4rev1 X-GM-EXT-1] gmail-fixture\r\n")
					reader := bufio.NewReader(c)
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
						switch strings.ToUpper(parts[1]) {
						case "LOGIN":
							if strings.Contains(line, "valid-app-password") {
								_, _ = fmt.Fprintf(c, "%s OK authenticated\r\n", tag)
							} else {
								_, _ = fmt.Fprintf(c, "%s NO [AUTHENTICATIONFAILED] Invalid credentials\r\n", tag)
							}
						case "CAPABILITY":
							_, _ = fmt.Fprintf(c, "* CAPABILITY IMAP4rev1 X-GM-EXT-1\r\n%s OK capability\r\n", tag)
						case "LIST":
							_, _ = fmt.Fprintf(c, "* LIST (\\HasNoChildren) \"/\" \"INBOX\"\r\n%s OK list\r\n", tag)
						default:
							_, _ = fmt.Fprintf(c, "%s BAD unsupported\r\n", tag)
						}
					}
				}(conn)
			}
		}()
		defer func() { secured.Close(); <-done }()

		// 1. Untrusted TLS server (without SSL_CERT_FILE) reports target_tls_failed, NOT credentials_rejected.
		host, portStr, _ := net.SplitHostPort(listener.Addr().String())
		port, _ := strconv.Atoi(portStr)
		badTLSAccount := mail.Account{ID: "gm", Provider: "google", Username: "user@gmail.com", Host: host, Port: port}
		if _, err := dialRoute(context.Background(), badTLSAccount, "valid-app-password", network.Route{Key: "direct"}); err == nil {
			t.Fatal("untrusted TLS cert accepted")
		} else {
			var coded interface{ OperationErrorCode() string }
			if !errors.As(err, &coded) || coded.OperationErrorCode() != "target_tls_failed" {
				t.Fatalf("expected target_tls_failed, got %v", err)
			}
		}

		// 2. Re-exec with SSL_CERT_FILE to test trusted TLS + LOGIN + wrong password distinction.
		cmd := exec.Command(os.Args[0], "-test.run=^TestGmailPasswordLoginDistinguishesCredentialsTLSAndPoolKey$", "-test.timeout=20s")
		cmd.Env = append(os.Environ(), "SSL_CERT_FILE="+certPath, "MAIL_AUTO_TEST_GMAIL_HOST="+host, "MAIL_AUTO_TEST_GMAIL_PORT="+portStr, "MAIL_AUTO_TEST_GMAIL_TLS_NAME="+certificate.DNSNames[0])
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("subprocess failed: %v\n%s", err, out)
		}
		return
	}

	// Verify accountAuthKey changes when password secret rotates (pool invalidation).
	a1 := mail.Account{ID: "gm", Provider: "google", Username: "u@gmail.com", Host: "imap.gmail.com", Port: 993, Secret: []byte("cipher-1")}
	a2 := mail.Account{ID: "gm", Provider: "google", Username: "u@gmail.com", Host: "imap.gmail.com", Port: 993, Secret: []byte("cipher-2")}
	if accountAuthKey(a1) == accountAuthKey(a2) {
		t.Fatal("accountAuthKey did not change on password rotation")
	}

	port, _ := strconv.Atoi(os.Getenv("MAIL_AUTO_TEST_GMAIL_PORT"))
	host := os.Getenv("MAIL_AUTO_TEST_GMAIL_HOST")
	// Connect over loopback via proxy/host matching certificate DNS name.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), time.Second)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer target.Close()
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buf.Flush()
		go io.Copy(target, conn)
		io.Copy(conn, target)
	}))
	defer proxy.Close()
	pHost, pPortStr, _ := net.SplitHostPort(strings.TrimPrefix(proxy.URL, "http://"))
	pPort, _ := strconv.Atoi(pPortStr)
	route := network.Route{Key: "test-proxy", Profile: &network.Profile{Protocol: "http", Host: pHost, Port: pPort}}
	account := mail.Account{ID: "gm", Provider: "google", Username: "user@gmail.com", Host: os.Getenv("MAIL_AUTO_TEST_GMAIL_TLS_NAME"), Port: 993}

	// Wrong password -> credentials_rejected, no secret leaked.
	if _, err := dialRoute(context.Background(), account, "secret-wrong-pass-never-echo", route); err == nil {
		t.Fatal("wrong password succeeded")
	} else {
		var coded interface{ OperationErrorCode() string }
		if !errors.As(err, &coded) || coded.OperationErrorCode() != "credentials_rejected" || strings.Contains(err.Error(), "secret-wrong-pass-never-echo") {
			t.Fatalf("unexpected wrong password error: %v", err)
		}
	}

	// Valid password -> succeeds and detects X-GM-EXT-1.
	conn, err := dialRoute(context.Background(), account, "valid-app-password", route)
	if err != nil {
		t.Fatalf("valid Gmail password login failed: %v", err)
	}
	if !conn.gmail {
		conn.close()
		t.Fatal("Gmail X-GM-EXT-1 capability not enabled for password auth")
	}
	conn.close()
}
