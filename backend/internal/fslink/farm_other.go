//go:build !windows

package fslink

import (
	"io/fs"
	"os"
	"syscall"
)

// hardLinkFile makes link a second name for the file existing.
func hardLinkFile(existing, link string) error {
	return os.Link(existing, link)
}

// onSameVolume compares the device numbers of two existing paths. A platform
// that reports none is assumed to be one volume; a cross-device link then fails
// and the farm is removed.
func onSameVolume(a, b string) (bool, error) {
	ia, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	ib, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	sa, okA := ia.Sys().(*syscall.Stat_t)
	sb, okB := ib.Sys().(*syscall.Stat_t)
	if !okA || !okB {
		return true, nil
	}
	return uint64(sa.Dev) == uint64(sb.Dev), nil //nolint:unconvert // Dev's type differs by platform
}

// makeFarmLink re-creates a symlink at to, pointing at target.
func makeFarmLink(_, to, target string, _ fs.FileInfo) (bool, error) {
	if err := os.Symlink(target, to); err != nil {
		return false, err
	}
	return true, nil
}
