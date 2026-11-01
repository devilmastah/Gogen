//go:build !unix && !windows

package codeindex

import "os"

func openLockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
}

func acquireLock(f *os.File) (func(), error) {
	return func() {}, nil
}
