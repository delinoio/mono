//go:build linux

package firecracker

import (
	"bytes"
	"errors"
	"io"
	"os"
	"syscall"
)

// ficlone is the FICLONE ioctl request number.
const ficlone = 0x40049409

// cloneFile copies src to dst with a reflink where the file system supports
// it, and otherwise with a sparse copy that skips all-zero blocks, so a
// mostly empty disk image does not use its full size.
func cloneFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, out.Fd(), ficlone, in.Fd()); errno == 0 {
		return out.Close()
	}
	if err := sparseCopy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func sparseCopy(out, in *os.File) error {
	const block = 1 << 20
	buf := make([]byte, block)
	zero := make([]byte, block)
	var off int64
	for {
		n, err := io.ReadFull(in, buf)
		if n > 0 {
			if !bytes.Equal(buf[:n], zero[:n]) {
				if _, werr := out.WriteAt(buf[:n], off); werr != nil {
					return werr
				}
			}
			off += int64(n)
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	return out.Truncate(off)
}
