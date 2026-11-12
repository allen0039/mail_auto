package imap

import (
	"errors"
	"testing"

	"mail-auto/internal/mail"
)

func TestUncertainProblemKeepsStageAndUncertainty(t *testing.T) {
	err := uncertainProblem("移除已复制的源邮件", "服务器拒绝 UID EXPUNGE")
	if !errors.Is(err, mail.ErrUncertain) {
		t.Fatal("must stay uncertain")
	}
	var public interface{ PublicMessage() string }
	if !errors.As(err, &public) || public.PublicMessage() != "移除已复制的源邮件：服务器拒绝 UID EXPUNGE" {
		t.Fatal(err)
	}
}
