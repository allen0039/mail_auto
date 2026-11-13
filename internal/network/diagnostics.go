package network

import (
	"context"
	"crypto/tls"
	"net"
	"time"
)

type Check struct {
	Stage     string `json:"stage"`
	OK        bool   `json:"ok"`
	Code      string `json:"code"`
	ElapsedMS int64  `json:"elapsed_ms"`
}

func ProbeTLS(ctx context.Context, route Route, address, hostname string) []Check {
	return ProbeTLSWithDialer(ctx, route.DialContext, address, hostname)
}

func ProbeTLSWithDialer(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error), address, hostname string) []Check {
	start := time.Now()
	conn, err := dial(ctx, "tcp", address)
	if err != nil {
		stage, code := Failure(err)
		return []Check{{Stage: stage, Code: code, ElapsedMS: time.Since(start).Milliseconds()}}
	}
	defer conn.Close()
	checks := []Check{{Stage: "tcp_tunnel", OK: true, Code: "ok", ElapsedMS: time.Since(start).Milliseconds()}}
	start = time.Now()
	secured := tls.Client(conn, &tls.Config{ServerName: hostname, MinVersion: tls.VersionTLS12})
	err = secured.HandshakeContext(ctx)
	check := Check{Stage: "target_tls", OK: err == nil, Code: "ok", ElapsedMS: time.Since(start).Milliseconds()}
	if err != nil {
		check.Code = "target_tls_failed"
	}
	return append(checks, check)
}
