// Package logging 保留旧日志挂载并限制磁盘占用。
package logging

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

type Writer struct {
	mu   sync.Mutex
	path string
	file *os.File
	size int64
}

func Open(path string) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	w := &Writer{path: path}
	return w, w.open()
}
func (w *Writer) open() error {
	f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.file, w.size = f, info.Size()
	return nil
}
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	if w.size+int64(len(p)) > 10<<20 {
		if err := w.file.Close(); err != nil {
			return 0, err
		}
		w.file = nil
		for i := 3; i >= 1; i-- {
			source := w.path
			if i > 1 {
				source = fmt.Sprintf("%s.%d", w.path, i-1)
			}
			target := fmt.Sprintf("%s.%d", w.path, i)
			if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
				return 0, err
			}
			if err := os.Rename(source, target); err != nil && !errors.Is(err, os.ErrNotExist) {
				return 0, err
			}
		}
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	if n < len(p) && err == nil {
		err = io.ErrShortWrite
	}
	return n, err
}
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
