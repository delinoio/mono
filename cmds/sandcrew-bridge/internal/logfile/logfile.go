// SPDX-License-Identifier: Apache-2.0

// Package logfile is the size-rotated bridge.log: when a write would make
// the file larger than MaxSize, the file becomes `<name>.1` (replacing the
// previous one) and a new file starts.
package logfile

import (
	"os"
	"sync"
)

// MaxSize is the rotation size of bridge.log.
const MaxSize = 10 << 20

// File is a rotating log file. It is safe for concurrent use.
type File struct {
	path    string
	maxSize int64

	mu   sync.Mutex
	file *os.File
	size int64
}

// Open opens or creates the log at path.
func Open(path string, maxSize int64) (*File, error) {
	if maxSize <= 0 {
		maxSize = MaxSize
	}
	f := &File{path: path, maxSize: maxSize}
	if err := f.open(); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *File) open() error {
	file, err := os.OpenFile(f.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	f.file = file
	f.size = info.Size()
	return nil
}

// Write appends p, rotating first when p does not fit.
func (f *File) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.size > 0 && f.size+int64(len(p)) > f.maxSize {
		if err := f.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := f.file.Write(p)
	f.size += int64(n)
	return n, err
}

func (f *File) rotate() error {
	if err := f.file.Close(); err != nil {
		return err
	}
	// On Windows, Rename does not replace an open file; the previous file
	// is closed here and nothing else holds it.
	_ = os.Remove(f.path + ".1")
	if err := os.Rename(f.path, f.path+".1"); err != nil {
		return err
	}
	return f.open()
}

// Close closes the file.
func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.file.Close()
}
