//go:build linux

package store

import (
	"fmt"
	"syscall"
)

const beegfsMagic = 0x19830326
const overlayfsMagic = 0x794C7630

// localFilesystems are known local, durable filesystems acceptable for the
// application database.
var localFilesystems = map[uint32]bool{
	0xEF53:     true, // ext2/ext3/ext4
	0x58465342: true, // XFS
	0x9123683E: true, // Btrfs
	0xF2F52010: true, // F2FS
	0x3153464A: true, // JFS
	0x52654973: true, // ReiserFS
	0x2FC12FC1: true, // ZFS
	0xCA451A4E: true, // bcachefs
	0x5346544E: true, // NTFS
}

// rejectedFilesystems are network, cluster or non-durable filesystems that can
// never hold the application database.
var rejectedFilesystems = map[uint32]string{
	0x6969:     "NFS",
	0x517B:     "SMB",
	0xFF534D42: "CIFS",
	0xFE534D42: "SMB2",
	0x73757245: "CODA",
	0x5346414F: "AFS",
	0x01021997: "9P",
	0x65735546: "FUSE",
	0x01021994: "tmpfs",
	0x858458f6: "ramfs",
	0x0BD00BD0: "Lustre",
	0x00C36400: "CephFS",
	0x47504653: "GPFS",
	0x7461636F: "OCFS2",
}

// databaseFilesystem enforces the database filesystem policy. It returns
// unverified=true when the storage cannot be proven local, so callers can warn.
func databaseFilesystem(path, journal string, allowUnverified bool) (bool, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return false, err
	}
	return checkFilesystemType(uint32(stat.Type), journal, allowUnverified)
}

func checkFilesystemType(fs uint32, journal string, allowUnverified bool) (bool, error) {
	if name, rejected := rejectedFilesystems[fs]; rejected {
		return false, fmt.Errorf("%s is not supported for the application database", name)
	}
	if fs == beegfsMagic {
		// WAL's shared-memory index is unsupported, and cross-node ownership
		// depends on effective client locking and durability the app cannot observe.
		if journal != "delete" {
			return true, fmt.Errorf("BeeGFS requires database.journalMode: delete")
		}
		if !allowUnverified {
			return true, fmt.Errorf("BeeGFS requires database.allowUnverifiedFilesystem: true after the storage operator verifies global file locks and remote fsync")
		}
		return true, nil
	}
	if fs == overlayfsMagic {
		// Containerized development and CI roots are commonly overlayfs. It is
		// accepted so those tests run, but its durability and cross-node locking
		// are not proven, so report it unverified and warn rather than pretending
		// it is verified local storage.
		return true, nil
	}
	if localFilesystems[fs] {
		return false, nil
	}
	if !allowUnverified {
		return true, fmt.Errorf("unrecognized filesystem type %#x: set database.allowUnverifiedFilesystem: true only after verifying durable local storage", fs)
	}
	return true, nil
}
