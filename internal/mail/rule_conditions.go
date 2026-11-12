package mail

import (
	"net/mail"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Text fields accept the full operator set. Address fields compare each parsed
// address separately; positive operators match when any value matches and
// not_contains matches only when no value contains the text.
var textOperators = []string{"equals", "contains", "not_contains", "starts_with", "ends_with", "regex"}

// Fields that only exist in metadata version 2. Older rows are "unknown" until
// the automatic sync refreshes them, so rules never act on missing data.
var extendedFields = []string{"to", "cc", "recipient", "list_id", "has_attachment", "is_reply"}

var ruleFieldLabels = map[string]string{
	"subject": "主题", "folder": "原邮箱目录", "sender": "发件地址", "domain": "发件域名",
	"to": "收件人", "cc": "抄送", "recipient": "收件人或抄送", "list_id": "邮件列表 List-Id",
	"read_state": "已读状态", "flagged": "星标", "has_attachment": "附件", "is_reply": "回复邮件",
	"received_days": "收信天数", "size": "邮件大小",
}

const maxRegexLength = 256

var regexCache sync.Map // pattern -> *regexp.Regexp

// compileRuleRegex compiles case-insensitively. Go's RE2 engine runs in linear
// time, so user patterns cannot cause catastrophic backtracking.
func compileRuleRegex(pattern string) (*regexp.Regexp, error) {
	if cached, ok := regexCache.Load(pattern); ok {
		return cached.(*regexp.Regexp), nil
	}
	re, e := regexp.Compile("(?i)" + pattern)
	if e != nil {
		return nil, e
	}
	regexCache.Store(pattern, re)
	return re, nil
}

func validCondition(c RuleCondition) bool {
	if len(c.Value) > maxRegexLength || strings.ContainsAny(c.Value, "\x00\r\n") {
		return false
	}
	switch c.Field {
	case "subject", "to", "cc", "recipient", "list_id", "sender", "domain":
		if !slices.Contains(textOperators, c.Operator) || c.Value == "" {
			return false
		}
		if c.Operator == "regex" {
			_, e := compileRuleRegex(c.Value)
			return e == nil
		}
		if c.Field == "sender" && c.Operator == "equals" {
			addr, e := mail.ParseAddress(c.Value)
			return e == nil && addr.Address == c.Value
		}
		if c.Field == "domain" && c.Operator == "equals" {
			return !strings.ContainsAny(c.Value, "@, \t")
		}
		return true
	case "folder":
		return c.Value != "" && c.Operator == "equals"
	case "read_state":
		return c.Operator == "equals" && slices.Contains([]string{"read", "unread"}, c.Value)
	case "flagged", "has_attachment", "is_reply":
		return c.Operator == "equals" && slices.Contains([]string{"true", "false"}, c.Value)
	case "received_days", "size":
		n, e := strconv.ParseInt(c.Value, 10, 64)
		max := int64(1 << 40)
		if c.Field == "received_days" {
			max = 36500
		}
		return e == nil && n >= 0 && n <= max && slices.Contains([]string{"gte", "lte"}, c.Operator)
	}
	return false
}

func addressValues(raw string, domainOnly bool) ([]string, bool) {
	if strings.TrimSpace(raw) == "" {
		return []string{}, true
	}
	addrs, e := mail.ParseAddressList(raw)
	if e != nil {
		return nil, false
	}
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		v := addr.Address
		if domainOnly {
			_, v, _ = strings.Cut(v, "@")
		}
		out = append(out, v)
	}
	return out, true
}

func textMatches(operator, want string, actual []string) bool {
	want = strings.ToLower(want)
	var re *regexp.Regexp
	if operator == "regex" {
		var e error
		if re, e = compileRuleRegex(want); e != nil {
			return false
		}
	}
	if operator == "not_contains" {
		for _, v := range actual {
			if strings.Contains(strings.ToLower(v), want) {
				return false
			}
		}
		return true
	}
	for _, v := range actual {
		v = strings.ToLower(v)
		switch operator {
		case "equals":
			if v == want {
				return true
			}
		case "contains":
			if strings.Contains(v, want) {
				return true
			}
		case "starts_with":
			if strings.HasPrefix(v, want) {
				return true
			}
		case "ends_with":
			if strings.HasSuffix(v, want) {
				return true
			}
		case "regex":
			if re.MatchString(v) {
				return true
			}
		}
	}
	return false
}

// conditionMatch returns 1 (match), 0 (no match) or -1 (unknown) and a reason.
func conditionMatch(c RuleCondition, m Message, now int64) (int, string) {
	label := ruleFieldLabels[c.Field]
	if label == "" {
		label = c.Field
	}
	if slices.Contains(extendedFields, c.Field) && m.MetadataVersion < CurrentMetadataVersion {
		return -1, label + "等待元数据刷新"
	}
	verdict := func(ok bool) (int, string) {
		if ok {
			return 1, label + "符合条件"
		}
		return 0, label + "不符合条件"
	}
	var actual []string
	switch c.Field {
	case "subject":
		actual = []string{m.Subject}
	case "folder":
		actual = []string{m.Folder}
	case "list_id":
		actual = []string{}
		if m.ListID != "" {
			actual = []string{m.ListID}
		}
	case "sender", "domain", "to", "cc", "recipient":
		raw := m.Sender
		switch c.Field {
		case "to":
			raw = m.Recipients
		case "cc":
			raw = m.Cc
		case "recipient":
			raw = strings.Trim(m.Recipients+", "+m.Cc, ", ")
		}
		values, ok := addressValues(raw, c.Field == "domain")
		// Every message has a sender; an empty one means the envelope is unusable.
		if !ok || ((c.Field == "sender" || c.Field == "domain") && len(values) == 0) {
			return -1, label + "无法解析"
		}
		actual = values
	case "read_state":
		if !m.FlagsKnown {
			return -1, "已读状态尚未验证"
		}
		return verdict((c.Value == "unread") == m.Unread)
	case "flagged":
		if !m.FlagsKnown {
			return -1, "星标状态尚未验证"
		}
		return verdict(strconv.FormatBool(m.Flagged) == c.Value)
	case "has_attachment":
		return verdict(strconv.FormatBool(m.HasAttachment) == c.Value)
	case "is_reply":
		return verdict(strconv.FormatBool(m.InReplyTo != "") == c.Value)
	case "received_days", "size":
		value := int64(m.Size)
		if c.Field == "received_days" {
			if m.ReceivedAt <= 0 {
				return -1, "收信时间尚未同步"
			}
			value = (now - m.ReceivedAt) / 86400
		}
		n, _ := strconv.ParseInt(c.Value, 10, 64)
		return verdict((c.Operator == "gte" && value >= n) || (c.Operator == "lte" && value <= n))
	default:
		return -1, label + "不受支持"
	}
	return verdict(textMatches(c.Operator, c.Value, actual))
}
