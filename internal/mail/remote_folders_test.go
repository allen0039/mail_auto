package mail

import (
	"context"
	"errors"
	"testing"
)

type folderRemote struct {
	*organizerRemote
	created []string
	err     error
	offline bool
}

func (f *folderRemote) CreateFolder(_ context.Context, _ Account, _ string, name string) error {
	f.created = append(f.created, name)
	return f.err
}
func (f *folderRemote) Capabilities(ctx context.Context, a Account, pw string) (Capabilities, error) {
	if f.offline {
		return Capabilities{}, errors.New("offline")
	}
	caps, e := f.organizerRemote.Capabilities(ctx, a, pw)
	caps.Move = false
	for _, name := range f.created {
		caps.Folders = append(caps.Folders, FolderInfo{Name: name})
	}
	return caps, e
}
func TestOriginalFolderCreationAndUIDPlusOnlyConfiguration(t *testing.T) {
	s, remote, access, m := organizerFixture(t)
	f := &folderRemote{organizerRemote: remote}
	s.Remote = f
	// No archive mapping is necessary for classification or Trash.
	if e := s.SaveFolderSettings(context.Background(), FolderSettings{AccountID: m.AccountID, Trash: "Trash", WritesEnabled: true}); e != nil {
		t.Fatal(e)
	}
	caps, e := s.ProbeCapabilities(context.Background(), m.AccountID)
	if e != nil || caps.MoveMode != "uidplus_copy" {
		t.Fatal(caps, e)
	}
	name := "工作/账单"
	out, e := s.CreateRemoteFolder(context.Background(), access, m.AccountID, name)
	if e != nil || out.Name != name || len(f.created) != 1 {
		t.Fatal(out, e, f.created)
	}
	p, e := s.NewPlan(context.Background(), access, PlanInput{Refs: []string{m.Ref}, Action: "move", Destination: name})
	if e != nil || p.Items[0].Destination != name {
		t.Fatal(p, e)
	}
	o := executeFixture(t, s, access, PlanInput{Refs: []string{m.Ref}, Action: "move", Destination: name})
	if o.State != "completed" {
		t.Fatal(o)
	}
	moved, e := s.Message(m.Ref)
	if e != nil || moved.Folder != name || remote.moves != 1 {
		t.Fatal(moved, e)
	}
	if _, e = s.NewPlan(context.Background(), access, PlanInput{Refs: []string{m.Ref}, Action: "archive"}); e == nil {
		t.Fatal("missing mapping accepted")
	}
	// A failed remote creation must never publish a local category.
	f.err = ErrUncertain
	if _, e = s.CreateRemoteFolder(context.Background(), access, m.AccountID, "Unconfirmed"); !errors.Is(e, ErrUncertain) {
		t.Fatal(e)
	}
	boxes, _ := s.Mailboxes(m.AccountID)
	for _, b := range boxes {
		if b.Name == "Unconfirmed" {
			t.Fatal("fake local folder published")
		}
	}
	// Disabling writes remains possible when the mailbox is unreachable.
	f.offline = true
	if e = s.SaveFolderSettings(context.Background(), FolderSettings{AccountID: m.AccountID, WritesEnabled: false}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.CreateRemoteFolder(context.Background(), access, m.AccountID, "Blocked"); !errors.Is(e, ErrDenied) {
		t.Fatal(e)
	}
}
func TestFolderCreationPermissionsAndNames(t *testing.T) {
	s, remote, _, m := organizerFixture(t)
	f := &folderRemote{organizerRemote: remote}
	s.Remote = f
	_, secret, e := s.CreateToken(TokenInput{Name: "read only", Accounts: []string{m.AccountID}, Actions: []string{"read"}})
	if e != nil {
		t.Fatal(e)
	}
	access, e := s.AuthenticateToken(secret)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.CreateRemoteFolder(context.Background(), access, m.AccountID, "Bills"); !errors.Is(e, ErrDenied) {
		t.Fatal(e)
	}
	_, secret, e = s.CreateToken(TokenInput{Name: "scoped", Accounts: []string{m.AccountID}, Folders: map[string][]string{m.AccountID: {"Allowed"}}, Actions: []string{"read", "folder_manage"}})
	if e != nil {
		t.Fatal(e)
	}
	access, _ = s.AuthenticateToken(secret)
	if _, e = s.CreateRemoteFolder(context.Background(), access, m.AccountID, "Other"); !errors.Is(e, ErrDenied) {
		t.Fatal(e)
	}
	if _, e = s.CreateRemoteFolder(context.Background(), access, m.AccountID, "Allowed"); e != nil {
		t.Fatal(e)
	}
	s.RevokeToken(access.Owner)
	if _, e = s.CreateRemoteFolder(context.Background(), access, m.AccountID, "Allowed"); !errors.Is(e, ErrDenied) {
		t.Fatal(e)
	}
	if len(f.created) != 1 {
		t.Fatal("unauthorized write", f.created)
	}
	for _, name := range []string{"", " ", " leading", "trailing ", "INBOX", "inbox", "bad\nname", "bad\x00name", string([]byte{0xff})} {
		if ValidFolderName(name) {
			t.Fatal(name)
		}
	}
	if !ValidFolderName("工作/账单") {
		t.Fatal("valid hierarchy rejected")
	}
}
