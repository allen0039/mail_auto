package mail

import "testing"

func TestValidConditionOperators(t *testing.T) {
	valid := []RuleCondition{
		{"subject", "not_contains", "广告"},
		{"subject", "regex", `^\[(invoice|bill)\]`},
		{"sender", "equals", "a@example.com"},
		{"sender", "ends_with", "@example.com"},
		{"domain", "contains", "example"},
		{"to", "contains", "me@"},
		{"recipient", "equals", "team@example.com"},
		{"list_id", "equals", "news.example.com"},
		{"has_attachment", "equals", "true"},
		{"is_reply", "equals", "false"},
		{"received_days", "gte", "30"},
	}
	for _, c := range valid {
		if !validCondition(c) {
			t.Fatal("rejected", c)
		}
	}
	invalid := []RuleCondition{
		{"subject", "regex", "(unclosed"},
		{"subject", "gte", "1"},
		{"sender", "equals", "Name <a@example.com>"},
		{"domain", "equals", "a@example.com"},
		{"to", "contains", ""},
		{"has_attachment", "contains", "true"},
		{"is_reply", "equals", "yes"},
		{"folder", "contains", "INBOX"},
		{"header", "equals", "x"},
		{"subject", "contains", "a\nb"},
	}
	for _, c := range invalid {
		if validCondition(c) {
			t.Fatal("accepted", c)
		}
	}
}

func TestConditionMatchExtendedFields(t *testing.T) {
	m := Message{
		Subject: "[Invoice] October", Sender: "Billing <billing@shop.example.com>", Folder: "INBOX",
		Recipients: "me@example.com, team@example.com", Cc: "boss@example.org", ListID: "deals.shop.example.com",
		InReplyTo: "<root@x>", HasAttachment: true, FlagsKnown: true, MetadataVersion: CurrentMetadataVersion,
	}
	cases := []struct {
		c    RuleCondition
		want int
	}{
		{RuleCondition{"subject", "regex", `^\[invoice\]`}, 1},
		{RuleCondition{"subject", "starts_with", "[inv"}, 1},
		{RuleCondition{"subject", "ends_with", "october"}, 1},
		{RuleCondition{"subject", "not_contains", "invoice"}, 0},
		{RuleCondition{"subject", "not_contains", "newsletter"}, 1},
		{RuleCondition{"sender", "ends_with", "@shop.example.com"}, 1},
		{RuleCondition{"domain", "equals", "shop.example.com"}, 1},
		{RuleCondition{"to", "equals", "team@example.com"}, 1},
		{RuleCondition{"to", "equals", "boss@example.org"}, 0},
		{RuleCondition{"cc", "equals", "boss@example.org"}, 1},
		{RuleCondition{"recipient", "equals", "boss@example.org"}, 1},
		{RuleCondition{"recipient", "not_contains", "example.org"}, 0},
		{RuleCondition{"list_id", "contains", "shop.example"}, 1},
		{RuleCondition{"has_attachment", "equals", "true"}, 1},
		{RuleCondition{"is_reply", "equals", "false"}, 0},
	}
	for _, tc := range cases {
		if got, why := conditionMatch(tc.c, m, 0); got != tc.want {
			t.Fatal(tc.c, got, why)
		}
	}
	// Missing To is a real value: not_contains matches, positive operators do not.
	bare := m
	bare.Recipients, bare.Cc, bare.ListID = "", "", ""
	if got, _ := conditionMatch(RuleCondition{"to", "not_contains", "x"}, bare, 0); got != 1 {
		t.Fatal("empty to not_contains", got)
	}
	if got, _ := conditionMatch(RuleCondition{"list_id", "contains", "x"}, bare, 0); got != 0 {
		t.Fatal("empty list_id contains", got)
	}
	// Legacy rows must not be judged on fields they do not have yet.
	legacy := m
	legacy.MetadataVersion = 1
	for _, field := range extendedFields {
		if got, _ := conditionMatch(RuleCondition{field, "equals", "true"}, legacy, 0); got != -1 {
			t.Fatal("legacy row decided", field, got)
		}
	}
	if got, _ := conditionMatch(RuleCondition{"subject", "contains", "invoice"}, legacy, 0); got != 1 {
		t.Fatal("legacy subject", got)
	}
	// An empty sender stays unknown rather than silently matching negations.
	noSender := m
	noSender.Sender = ""
	if got, _ := conditionMatch(RuleCondition{"sender", "not_contains", "x"}, noSender, 0); got != -1 {
		t.Fatal("empty sender", got)
	}
}
