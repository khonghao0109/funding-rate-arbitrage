//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockStateDir takes an exclusive advisory lock on the intent directory for the
// life of the process.
//
// writeMu serializes writes inside ONE portal. Two portals on two ports share
// .paper/exec and would each hold their own writeMu, so both could act on the
// same intent — the same derived ClientOrderIDs sent twice. The lock makes the
// second portal refuse to start. cmd/execcheck does not take it; running it
// against an intent while the portal is acting on that intent is still the
// operator's to avoid.
func lockStateDir(dir string) (release func(), err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, ".execportal.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("một execportal khác đang giữ %s — hai portal trên cùng thư mục ý định có thể gửi cùng một lệnh hai lần", path)
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
