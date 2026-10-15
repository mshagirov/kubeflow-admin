//go:build linux

package store

import "testing"

func TestFilesystemJournalPolicy(t *testing.T) {
	for _, tc := range []struct {
		name           string
		fs             uint32
		journal        string
		allow          bool
		wantErr        bool
		wantUnverified bool
	}{
		{"beegfs wal rejected", 0x19830326, "wal", true, true, true},
		{"beegfs delete requires opt-in", 0x19830326, "delete", false, true, true},
		{"beegfs delete allowed", 0x19830326, "delete", true, false, true},
		{"nfs never allowed", 0x6969, "delete", true, true, false},
		{"fuse never allowed", 0x65735546, "wal", true, true, false},
		{"tmpfs never allowed", 0x01021994, "wal", true, true, false},
		{"lustre rejected", 0x0BD00BD0, "delete", true, true, false},
		{"cephfs rejected", 0x00C36400, "delete", true, true, false},
		{"ext4 wal", 0xEF53, "wal", false, false, false},
		{"ext4 delete", 0xEF53, "delete", false, false, false},
		{"xfs wal", 0x58465342, "wal", false, false, false},
		{"btrfs delete", 0x9123683E, "delete", false, false, false},
		{"overlayfs accepted for containers", 0x794C7630, "wal", false, false, false},
		{"unknown requires opt-in", 0x12345678, "wal", false, true, true},
		{"unknown allowed with opt-in", 0x12345678, "wal", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unverified, err := checkFilesystemType(tc.fs, tc.journal, tc.allow)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if err == nil && unverified != tc.wantUnverified {
				t.Fatalf("unverified=%v want=%v", unverified, tc.wantUnverified)
			}
		})
	}
}
