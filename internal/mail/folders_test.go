package mail

import "testing"

func TestFolderLabelsFollowMailbox(t *testing.T) {
	for _, folder := range []string{"INBOX", "Inbox", "inbox"} {
		if FolderLabel(folder) != "收件箱" {
			t.Fatal("reserved inbox label", folder)
		}
	}
	for _, folder := range []string{"收件箱", "已发送", "草稿箱", "垃圾邮件", "已删除", "回收站", "Deleted Messages", "Deleted Items", "Trash", "INBOX.Trash", "INBOX/Deleted Messages", "废纸篓", "工作/已删除记录", "Archive", "归档/项目 A", " 自定义标签 "} {
		if FolderLabel(folder) != folder {
			t.Fatal("mailbox label renamed", folder)
		}
	}
	s, r, a := newAutomaticFixture(t)
	r.boxes = map[string]Snapshot{"已删除": {UIDValidity: 1, Messages: []Message{{UID: 1}}}}
	if e := s.incrementalFolder(t.Context(), &fixtureSession{r}, a, "已删除", 0); e != nil {
		t.Fatal(e)
	}
	found, n, e := s.Search(Search{Folder: "已删除"})
	if e != nil || n != 1 || found[0].Folder != "已删除" || found[0].FolderLabel != "已删除" {
		t.Fatal("display changed remote identity", n, e)
	}
}
