// Package rotlog is a log file writer with size-based rotation, so logs stay
// bounded without depending on logrotate.
package rotlog

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// File writes to path, renaming it to path.1 (and path.1 to path.2, and so
// on, up to Backups files) when it would exceed MaxBytes. Total disk use is
// at most (Backups+1) * MaxBytes.
type File struct {
	path     string
	maxBytes int64
	backups  int

	mu   sync.Mutex
	f    *os.File
	size int64
}

// Open opens or creates the log file, creating its directory if needed.
func Open(path string, maxBytes int64, backups int) (*File, error) {
	if maxBytes <= 0 || backups < 0 {
		return nil, fmt.Errorf("invalid log limits: max %d bytes, %d backups", maxBytes, backups)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	r := &File{path: path, maxBytes: maxBytes, backups: backups}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *File) open() error {
	f, err := os.OpenFile(r.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, fi.Size()
	return nil
}

// Write appends p, rotating first if p would take the file over the limit.
func (r *File) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return 0, os.ErrClosed
	}
	if r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *File) rotate() error {
	if err := r.f.Close(); err != nil {
		return err
	}
	r.f = nil
	if r.backups == 0 {
		os.Remove(r.path)
	} else {
		os.Remove(fmt.Sprintf("%s.%d", r.path, r.backups))
		for i := r.backups - 1; i >= 1; i-- {
			os.Rename(fmt.Sprintf("%s.%d", r.path, i), fmt.Sprintf("%s.%d", r.path, i+1))
		}
		if err := os.Rename(r.path, r.path+".1"); err != nil {
			return err
		}
	}
	return r.open()
}

// Close closes the file.
func (r *File) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}
