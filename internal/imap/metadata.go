package imap

import (
	"bufio"
	"io"
	"net/textproto"
	"strings"

	protocol "github.com/emersion/go-imap"
	mailbox "mail-auto/internal/mail"
	mailmime "mail-auto/internal/mime"
)

// metadataHeaderSection reads only the two headers that the envelope lacks.
// PEEK keeps the read state unchanged.
var metadataHeaderSection = &protocol.BodySectionName{
	BodyPartName: protocol.BodyPartName{Specifier: protocol.HeaderSpecifier, Fields: []string{"REFERENCES", "LIST-ID"}},
	Peek:         true,
}

const maxHeaderBytes = 16 << 10

func joinAddresses(list []*protocol.Address) string {
	out := []string{}
	for _, a := range list {
		if a == nil {
			continue
		}
		if address := a.Address(); address != "" && address != "@" {
			out = append(out, address)
		}
	}
	return strings.Join(out, ", ")
}

func cleanField(v string, limit int) string {
	v = strings.ToValidUTF8(strings.TrimSpace(v), "�")
	v = strings.NewReplacer("\r", " ", "\n", " ", "\x00", "").Replace(v)
	v, _ = mailmime.Truncate(v, limit)
	return v
}

// hasAttachment walks a bounded BODYSTRUCTURE tree. It mirrors the attachment
// listing in Read: an explicit attachment disposition or any named part.
func hasAttachment(b *protocol.BodyStructure) bool {
	parts := 0
	var walk func(*protocol.BodyStructure, int) bool
	walk = func(b *protocol.BodyStructure, depth int) bool {
		if b == nil || depth > 12 || parts > 128 {
			return false
		}
		parts++
		if strings.EqualFold(b.Disposition, "attachment") {
			return true
		}
		if name, _ := b.Filename(); name != "" {
			return true
		}
		for _, p := range b.Parts {
			if walk(p, depth+1) {
				return true
			}
		}
		return false
	}
	return walk(b, 0)
}

// headerFields returns the fetched header subset. Servers may echo the field
// list with different case or quoting, so any HEADER.FIELDS section is accepted.
func headerFields(m *protocol.Message) textproto.MIMEHeader {
	for section, literal := range m.Body {
		if section == nil || literal == nil || section.Specifier != protocol.HeaderSpecifier || len(section.Fields) == 0 {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(literal, maxHeaderBytes+1))
		if err != nil || len(raw) > maxHeaderBytes {
			return nil
		}
		h, err := textproto.NewReader(bufio.NewReader(strings.NewReader(string(raw) + "\r\n"))).ReadMIMEHeader()
		if err != nil {
			return nil
		}
		return h
	}
	return nil
}

func applyExtendedMetadata(out *mailbox.Message, m *protocol.Message) {
	if m.Envelope != nil {
		out.Recipients = cleanField(joinAddresses(m.Envelope.To), 2048)
		out.Cc = cleanField(joinAddresses(m.Envelope.Cc), 2048)
		out.MessageID = cleanField(m.Envelope.MessageId, 512)
		out.InReplyTo = cleanField(m.Envelope.InReplyTo, 512)
	}
	out.HasAttachment = hasAttachment(m.BodyStructure)
	if h := headerFields(m); h != nil && completeBodyStructure(m.BodyStructure) {
		out.References = cleanField(strings.Join(strings.Fields(h.Get("References")), " "), 2048)
		out.ListID = cleanField(normalizeListID(h.Get("List-Id")), 512)
		out.MetadataVersion = mailbox.CurrentMetadataVersion
	}
}

func completeBodyStructure(root *protocol.BodyStructure) bool {
	parts := 0
	var valid func(*protocol.BodyStructure, int) bool
	valid = func(part *protocol.BodyStructure, depth int) bool {
		if part == nil || depth > 12 {
			return false
		}
		parts++
		if parts > 128 {
			return false
		}
		for _, child := range part.Parts {
			if !valid(child, depth+1) {
				return false
			}
		}
		return true
	}
	return valid(root, 0)
}

// normalizeListID keeps the machine identifier from "Name <id.example.com>".
func normalizeListID(v string) string {
	v = strings.TrimSpace(v)
	if open := strings.LastIndex(v, "<"); open >= 0 {
		if end := strings.Index(v[open:], ">"); end > 0 {
			return strings.ToLower(strings.TrimSpace(v[open+1 : open+end]))
		}
	}
	return strings.ToLower(v)
}
