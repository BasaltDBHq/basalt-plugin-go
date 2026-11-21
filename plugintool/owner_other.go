//go:build !unix

package plugintool

import "os"

const noFollowFlags = os.O_RDONLY

func checkOwner(os.FileInfo) error { return nil }
