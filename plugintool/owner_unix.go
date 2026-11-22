//go:build unix

package plugintool

import (
	"errors"
	"os"
	"syscall"
)

// noFollowFlags opens a path without following a final symlink.
const noFollowFlags = os.O_RDONLY | syscall.O_NOFOLLOW

// checkOwner refuses a file the current user does not own.
func checkOwner(fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("its owner cannot be read")
	}
	if int(st.Uid) != os.Geteuid() {
		return errors.New("it is owned by another user; it must be yours")
	}
	return nil
}
