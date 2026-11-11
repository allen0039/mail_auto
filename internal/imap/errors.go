package imap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	protocol "github.com/emersion/go-imap"
)

const remoteAttempts = 3

var errResponseLimit = errors.New("IMAP response limit")

// Never retain server error text: it can include credentials or message data.
type remoteError struct {
	operation string
	reason    string
	retryable bool
	attempts  int
}

func (e *remoteError) Error() string {
	s := e.operation + "：" + e.reason
	if e.attempts > 1 {
		s += fmt.Sprintf("（已自动重试 %d 次）", e.attempts-1)
	}
	return s
}
func (e *remoteError) PublicMessage() string { return e.Error() }
func (e *remoteError) Unwrap() error         { return ErrRemote }
func (e *remoteError) OperationErrorCode() string {
	switch e.reason {
	case "等待邮箱服务器响应超时（45 秒）":
		return "provider_timeout"
	case "邮箱服务器连接中断", "邮箱网络连接失败":
		return "provider_connection_failed"
	default:
		return "provider_error"
	}
}

func remoteFailure(conn *connection, operation string, err error) error {
	if conn != nil && conn.budget != nil {
		if readErr := conn.budget.lastError(); readErr != nil {
			err = readErr
		}
	}
	e := &remoteError{operation: operation, reason: "服务器拒绝请求或返回无效响应"}
	var netErr net.Error
	switch {
	case errors.Is(err, errResponseLimit):
		e.reason = "服务器响应超过 32 MiB 限制"
	case errors.As(err, &netErr) && netErr.Timeout():
		e.reason, e.retryable = "等待邮箱服务器响应超时（45 秒）", true
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed):
		e.reason, e.retryable = "邮箱服务器连接中断", true
	case protocol.IsParseError(err):
		e.reason = "邮箱服务器返回无法解析的响应"
	case err != nil && strings.Contains(err.Error(), "imap: connection closed"):
		e.reason, e.retryable = "邮箱服务器连接中断", true
	default:
		var opErr *net.OpError
		if errors.As(err, &opErr) {
			e.reason, e.retryable = "邮箱网络连接失败", true
		}
	}
	return e
}

func remoteProblem(operation, reason string) error {
	return &remoteError{operation: operation, reason: reason}
}

// Only transport failures are retried. Callbacks and protocol validation errors
// must reach the caller without replaying already committed batches.
func retryRemote(ctx context.Context, operation func() error) error {
	for attempt := 1; ; attempt++ {
		if e := ctx.Err(); e != nil {
			return e
		}
		err := operation()
		if err == nil {
			return nil
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		var failure *remoteError
		if !errors.As(err, &failure) || !failure.retryable {
			return err
		}
		failure.attempts = attempt
		if attempt == remoteAttempts {
			return err
		}
		slog.Warn("IMAP 请求中断，准备重连", "operation", failure.operation, "reason", failure.reason, "attempt", attempt)
		timer := time.NewTimer(time.Duration(attempt) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
