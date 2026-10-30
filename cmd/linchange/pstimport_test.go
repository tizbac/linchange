package main

import (
	"testing"

	"github.com/linchange/linchange/internal/pst"
)

func TestCommonFolderPrefix(t *testing.T) {
	folders := []importFolder{
		{Path: "Top of Personal Folders/Inbox"},
		{Path: "Top of Personal Folders/Inbox/Receipts"},
	}
	if got := commonFolderPrefix(folders); got != "Top of Personal Folders/Inbox" {
		t.Errorf("commonFolderPrefix = %q", got)
	}
	if got := commonFolderPrefix([]importFolder{{Path: "A"}, {Path: "B"}}); got != "" {
		t.Errorf("disjoint prefix = %q", got)
	}
}

func TestCollectImportFolders(t *testing.T) {
	items := []pst.Item{
		{ID: 10, ParentID: 1, Kind: "message"},
		{ID: 11, ParentID: 1, Kind: "message"},
		{ID: 12, ParentID: 2, Kind: "appointment"},
		{ID: 13, ParentID: 2, Kind: "contact"},
		{ID: 14, ParentID: 2, Kind: "item"},
	}
	paths := map[uint32]string{1: "Archive/Inbox", 2: "Archive/Calendar"}
	folders := collectImportFolders(func(id uint32) string { return paths[id] }, items)
	if len(folders) != 2 {
		t.Fatalf("got %d folders, want 2: %+v", len(folders), folders)
	}
	if folders[0].Path != "Archive/Calendar" || folders[0].Calendars != 1 || folders[0].Contacts != 1 {
		t.Errorf("calendar folder = %+v", folders[0])
	}
	if folders[1].Path != "Archive/Inbox" || folders[1].Messages != 2 {
		t.Errorf("inbox folder = %+v", folders[1])
	}
}

func TestValidateMailboxName(t *testing.T) {
	for _, name := range []string{"", " /x", "a//b", "a/"} {
		if err := validateMailboxName(name); err == nil {
			t.Errorf("validateMailboxName(%q) accepted an invalid name", name)
		}
	}
	if err := validateMailboxName("PST Import/2024"); err != nil {
		t.Errorf("valid name rejected: %v", err)
	}
}
