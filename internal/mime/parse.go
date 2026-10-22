package mime

import (
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"
	"golang.org/x/net/html"
)

const MaxRaw = 4 << 20
const MaxText = 64 << 10

func Truncate(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	s = s[:n]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s, true
}
func Parse(raw io.Reader) (string, bool, error) {
	e, err := message.ReadWithOptions(io.LimitReader(raw, MaxRaw+1), &message.ReadOptions{MaxHeaderBytes: 64 << 10})
	if err != nil {
		return "", false, errors.New("邮件 MIME 头或编码无法解析")
	}
	count := 0
	var plain, htmlParts []string
	plainRemaining, htmlRemaining := MaxText+1, MaxText+1
	plainTruncated, htmlTruncated := false, false
	var walk func(*message.Entity, int) error
	walk = func(e *message.Entity, depth int) error {
		count++
		if depth > 12 || count > 128 {
			return errors.New("邮件 MIME 嵌套或分段超过上限")
		}
		disposition, _, _ := e.Header.ContentDisposition()
		media, params, err := e.Header.ContentType()
		if err != nil {
			return errors.New("无效 MIME 类型")
		}
		if disposition == "attachment" || params["name"] != "" {
			return nil
		}
		if strings.HasPrefix(media, "multipart/") {
			mr := e.MultipartReader()
			if mr == nil {
				return errors.New("无效 multipart")
			}
			for {
				part, err := mr.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					return errors.New("邮件 MIME 分段损坏")
				}
				if err = walk(part, depth+1); err != nil {
					return err
				}
			}
			return nil
		}
		if media != "text/plain" && media != "text/html" {
			return nil
		}
		remaining := &plainRemaining
		truncated := &plainTruncated
		if media == "text/html" {
			remaining = &htmlRemaining
			truncated = &htmlTruncated
		}
		if *remaining <= 0 {
			*truncated = true
			return nil
		}
		b, err := io.ReadAll(io.LimitReader(e.Body, int64(*remaining)))
		if err != nil {
			return errors.New("正文解码失败")
		}
		*remaining -= len(b)
		if *remaining <= 0 {
			*truncated = true
		}
		text := strings.ToValidUTF8(string(b), "�")
		if media == "text/plain" {
			plain = append(plain, text)
		} else {
			htmlParts = append(htmlParts, HTMLText(text))
		}
		return nil
	}
	if err = walk(e, 0); err != nil {
		return "", false, err
	}
	text := strings.Join(plain, "\n\n")
	truncated := plainTruncated
	if strings.TrimSpace(text) == "" {
		text = strings.Join(htmlParts, "\n\n")
		truncated = htmlTruncated
	}
	text, cut := Truncate(text, MaxText)
	return text, truncated || cut, nil
}
func HTMLText(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var b strings.Builder
	skip := 0
	for {
		switch z.Next() {
		case html.ErrorToken:
			return strings.TrimSpace(b.String())
		case html.StartTagToken:
			tag, _ := z.TagName()
			t := string(tag)
			if t == "script" || t == "style" || t == "head" {
				skip++
			}
			if skip == 0 && (t == "br" || t == "p" || t == "div" || t == "li") {
				b.WriteByte('\n')
			}
		case html.EndTagToken:
			tag, _ := z.TagName()
			t := string(tag)
			if (t == "script" || t == "style" || t == "head") && skip > 0 {
				skip--
			}
			if skip == 0 && (t == "p" || t == "div" || t == "li") {
				b.WriteByte('\n')
			}
		case html.TextToken:
			if skip == 0 {
				b.Write(z.Text())
			}
		}
	}
}
