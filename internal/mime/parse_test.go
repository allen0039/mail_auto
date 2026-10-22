package mime

import (
	"strings"
	"testing"
)

func TestMIMETextAndLimits(t *testing.T) {
	cases := []struct{ name, raw, want string }{{"gbk", "Content-Type: text/plain; charset=gbk\r\n\r\n\xc4\xe3\xba\xc3", "你好"}, {"html", "Content-Type: text/html; charset=utf-8\r\n\r\n<html><head><title>secret title</title></head><body><script>fetch('/bad')</script><p>正常文本</p><img src='https://tracker/x'></body></html>", "正常文本"}, {"alternative", "Content-Type: multipart/alternative; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain\r\n\r\nplain text\r\n--x\r\nContent-Type: text/html\r\n\r\n<b>html text</b>\r\n--x--\r\n", "plain text"}, {"attachment", "Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain\r\n\r\nbody\r\n--x\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=private.txt\r\n\r\nattachment secret\r\n--x--\r\n", "body"}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text, _, e := Parse(strings.NewReader(c.raw))
			if e != nil || strings.TrimSpace(text) != c.want {
				t.Fatalf("got %q, %v", text, e)
			}
		})
	}
	text, cut, e := Parse(strings.NewReader("Content-Type: text/plain\r\n\r\n" + strings.Repeat("中", MaxText)))
	if e != nil || !cut || len(text) > MaxText {
		t.Fatal("text cap failed", len(text), cut, e)
	}
	raw := ""
	for range 14 {
		raw += "Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\n"
	}
	raw += "Content-Type: text/plain\r\n\r\ndeep"
	if _, _, e = Parse(strings.NewReader(raw)); e == nil {
		t.Fatal("deep MIME accepted")
	}
	if _, _, e = Parse(strings.NewReader("X-Long: " + strings.Repeat("a", 70<<10) + "\r\n\r\n")); e == nil {
		t.Fatal("large headers accepted")
	}
}
func FuzzParse(f *testing.F) {
	for _, s := range []string{"Subject: test\r\n\r\nhello", "Content-Type: text/html\r\n\r\n<script>bad</script>ok", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > MaxRaw {
			return
		}
		text, _, _ := Parse(strings.NewReader(s))
		if len(text) > MaxText {
			t.Fatal("text bound exceeded")
		}
	})
}

func TestPlaintextPreferredAfterLargeHTML(t *testing.T) {
	raw := "Content-Type: multipart/alternative; boundary=x\r\n\r\n--x\r\nContent-Type: text/html\r\n\r\n" + strings.Repeat("x", MaxText+10) + "\r\n--x\r\nContent-Type: text/plain\r\n\r\nreadable plain\r\n--x--\r\n"
	text, cut, e := Parse(strings.NewReader(raw))
	if e != nil || text != "readable plain" || cut {
		t.Fatal("HTML consumed plaintext budget", len(text), cut, e)
	}
}
