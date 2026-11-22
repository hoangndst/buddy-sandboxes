package workspace

import (
	"crypto/sha256"
	"io/fs"
	"time"
)

// Limits bounds the number and size of files transferred for a project.
type Limits struct {
	MaxEntries      int
	MaxTotalBytes   int64
	MaxFileBytes    int64
	MaxPathBytes    int
	TransferTimeout time.Duration
}

// DefaultLimits applies the initial project snapshot and merge limits.
func DefaultLimits() Limits {
	return Limits{
		MaxEntries:      10_000,
		MaxTotalBytes:   100 << 20,
		MaxFileBytes:    20 << 20,
		MaxPathBytes:    4 << 10,
		TransferTimeout: 5 * time.Minute,
	}
}

type EntryType uint8

const (
	EntryDirectory EntryType = iota + 1
	EntryRegular
)

// Entry records the transfer-relevant state of a path in the snapshot.
type Entry struct {
	Type   EntryType
	Mode   fs.FileMode
	Size   int64
	Digest [sha256.Size]byte
}

// Baseline is the state of the local project when its snapshot was created.
type Baseline map[string]Entry

// Stats describes the files included in a packed snapshot.
type Stats struct {
	Entries      int
	Files        int
	TotalBytes   int64
	ExcludedPath []string
}

// Conflict identifies a local path that changed after the snapshot was made.
type Conflict struct {
	Path   string
	Reason string
}

// MergeResult describes changes applied to the local project.
type MergeResult struct {
	Added     []string
	Modified  []string
	Deleted   []string
	Conflicts []Conflict
}
