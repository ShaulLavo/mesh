//go:build linux && amd64

package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

func (o *observer) bindImage(pid int) string {
	file, err := os.Open(fmt.Sprintf("/proc/%d/exe", pid)) // #nosec G304 G703 -- PID is a kernel-returned integer for an owned traced task, never path text; retain its mapped inode at the exec stop.
	if err != nil {
		o.err = fmt.Errorf("retain owned mapped image: %w", err)
		return unavailable
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(int(file.Fd()), &st); err != nil {
		_ = file.Close()
		o.err = fmt.Errorf("identify owned mapped image: %w", err)
		return unavailable
	}
	identity := fmt.Sprintf("%d:%d:%d", st.Dev, st.Ino, st.Size)
	if o.images[identity] != nil {
		_ = file.Close()
		return identity
	}
	if len(o.images) >= 256 || st.Size > 256<<20 {
		_ = file.Close()
		o.err = errors.New("owned mapped-image retention bound reached")
		return unavailable
	}
	o.images[identity] = file
	return identity
}

func (o *observer) mappedImage(identity string) map[string]any {
	if result := o.digests[identity]; result != nil {
		return result
	}
	file := o.images[identity]
	if file == nil {
		o.err = errors.New("failed syscall has no retained mapped image")
		return nil
	}
	result, err := digestImage(file)
	if err != nil {
		o.err = err
		return nil
	}
	o.digests[identity] = result
	return result
}

func digestImage(file *os.File) (map[string]any, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(file.Fd()), &st); err != nil {
		return nil, fmt.Errorf("stat held mapped image: %w", err)
	}
	if st.Size < 1 || st.Size > 256<<20 {
		return nil, errors.New("held mapped image exceeds byte bound")
	}
	digest := sha256.New()
	n, err := io.Copy(digest, io.NewSectionReader(file, 0, st.Size+1))
	if err != nil {
		return nil, fmt.Errorf("hash held mapped image: %w", err)
	}
	if n != st.Size {
		return nil, errors.New("held mapped image changed size")
	}
	return map[string]any{"sha256": fmt.Sprintf("%x", digest.Sum(nil)), "bytes": n, "device": st.Dev,
		"inode": st.Ino, "uid": st.Uid, "gid": st.Gid, "mode": st.Mode}, nil
}

func (o *observer) closeImages() {
	for _, file := range o.images {
		_ = file.Close()
	}
}
