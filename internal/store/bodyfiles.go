package store

import (
	"os"
	"path/filepath"
	"strings"
)

// SetCaptureRoot 规范化已存在目录的符号链接，避免 macOS 临时目录的 /var 别名使新文件被误判为目录外。
func (s *Store) SetCaptureRoot(root string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	s.captureRoot = abs
	return nil
}

func (s *Store) safeCapturePath(path string) bool {
	if s.captureRoot == "" || path == "" {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	root := s.captureRoot
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	rel, err := filepath.Rel(root, abs)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func (s *Store) stashSnapshot(trace, source string, snapshot RequestSnapshot) RequestSnapshot {
	if s.captureRoot == "" {
		return snapshot
	}
	path := filepath.Join(s.captureRoot, "requests", trace+"-"+source+".body")
	if !s.safeCapturePath(path) {
		return snapshot
	}
	if os.MkdirAll(filepath.Dir(path), 0700) != nil {
		return snapshot
	}
	if os.WriteFile(path, []byte(snapshot.Body), 0600) != nil {
		return snapshot
	}
	snapshot.FilePath = path
	snapshot.Body = ""
	return snapshot
}

func (s *Store) loadSnapshot(snapshot RequestSnapshot) RequestSnapshot {
	snapshot = copySnapshot(snapshot)
	if snapshot.FilePath != "" {
		if !s.safeCapturePath(snapshot.FilePath) {
			snapshot.Unavailable = "请求正文路径不可读取"
			return snapshot
		}
		body, err := os.ReadFile(snapshot.FilePath)
		if err != nil {
			snapshot.Unavailable = "请求正文文件不可读取，可能已被清理"
		} else {
			snapshot.Body = string(body)
		}
	}
	return snapshot
}
