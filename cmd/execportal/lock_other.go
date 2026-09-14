//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly)

package main

import "log"

// lockStateDir has no advisory lock to take on this platform, and says so
// instead of pretending: two portals on one intent directory are not stopped.
func lockStateDir(dir string) (release func(), err error) {
	log.Printf("execportal: CẢNH BÁO — không khoá được %s trên hệ điều hành này; đừng chạy hai portal cùng lúc", dir)
	return func() {}, nil
}
