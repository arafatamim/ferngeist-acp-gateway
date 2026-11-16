package logging

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const defaultLogFileName = "gateway.log"

// Service is a tiny rolling log writer used by the gateway for local diagnostics
// export. It intentionally avoids introducing a heavier logging dependency.
type Service struct {
	mu         sync.Mutex
	dir        string
	fileName   string
	maxSize    int64
	maxBackups int
	fileMode   os.FileMode
	file       *os.File
	size       int64
}

// New returns both the structured gateway logger and the underlying file
// service so diagnostics export can tail recent log lines.
func New(level, dir string, maxSize int64, maxBackups int) (*slog.Logger, *Service, error) {
	service, err := NewService(dir, defaultLogFileName, maxSize, maxBackups)
	if err != nil {
		return nil, nil, err
	}

	// The rolling file writer goes FIRST so gateway.log is always written even
	// when stdout is unavailable (e.g. the daemon is spawned without a console
	// and its std handles are invalid, or the parent that owns the stdout pipe
	// died). Writing stdout first would drop the file log on any stdout error,
	// because io.MultiWriter stops at the first failing writer.
	return slog.New(slog.NewJSONHandler(io.MultiWriter(service, os.Stdout), &slog.HandlerOptions{
		Level: parseLevel(level),
	})), service, nil
}

func NewService(dir, fileName string, maxSize int64, maxBackups int) (*Service, error) {
	return NewServiceWithMode(dir, fileName, maxSize, maxBackups, 0o644)
}

// NewServiceWithMode is like NewService but with a configurable file mode for
// the active log (and its rotated backups). The gateway log uses the default
// 0644; the frame log opts into 0600 because it carries raw conversation data.
func NewServiceWithMode(dir, fileName string, maxSize int64, maxBackups int, mode os.FileMode) (*Service, error) {
	if maxSize <= 0 {
		maxSize = 1024 * 1024
	}
	if maxBackups <= 0 {
		maxBackups = 3
	}
	if mode == 0 {
		mode = 0o644
	}

	service := &Service{
		dir:        dir,
		fileName:   fileName,
		maxSize:    maxSize,
		maxBackups: maxBackups,
		fileMode:   mode,
	}
	if err := service.ensureFileLocked(); err != nil {
		return nil, err
	}
	return service, nil
}

func (s *Service) Close() error {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	s.size = 0
	return err
}

func (s *Service) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ensureFileLocked(); err != nil {
		return 0, err
	}
	if s.size+int64(len(p)) > s.maxSize {
		if err := s.rotateLocked(); err != nil {
			return 0, err
		}
	}

	written, err := s.file.Write(p)
	s.size += int64(written)
	return written, err
}

// TailLines reads rotated files from oldest to newest and returns the last
// requested lines. This is optimized for small diagnostic bundles, not for
// arbitrary log browsing.
func (s *Service) TailLines(limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}

	s.mu.Lock()
	paths := s.logPathsLocked()
	s.mu.Unlock()

	return tailFiles(paths, limit)
}

// ReadGatewayLog returns the last limit lines (all when limit <= 0) of the
// gateway log in dir, oldest rotation first, without opening it for writing.
func ReadGatewayLog(dir string, limit int) ([]string, error) {
	paths := []string{filepath.Join(dir, defaultLogFileName)}
	// Rotation keeps backups numbered contiguously from 1, so stop at the first gap.
	for index := 1; ; index++ {
		path := filepath.Join(dir, fmt.Sprintf("%s.%d", defaultLogFileName, index))
		if _, err := os.Stat(path); err != nil {
			break
		}
		paths = append([]string{path}, paths...)
	}
	return tailFiles(paths, limit)
}

// FollowGatewayLog emits each complete line appended to the gateway log in dir
// from now until ctx is done, following the active file across rotations.
// shortcut: a file rotated out and back past within one poll is skipped; fine at 1 MiB rotations, track backups if logs grow faster.
func FollowGatewayLog(ctx context.Context, dir string, poll time.Duration, emit func(string)) error {
	path := filepath.Join(dir, defaultLogFileName)
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }() // read-only; close errors lose nothing
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return err
	}

	reader := bufio.NewReader(file)
	var partial string
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		if partial, err = emitLines(reader, partial, emit); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		current, statErr := os.Stat(path)
		opened, err := file.Stat()
		if statErr != nil || err != nil || os.SameFile(current, opened) {
			continue // not rotated, or the new file is not created yet
		}
		// Rotated: drain what was written before the rename, then switch files.
		if partial, err = emitLines(reader, partial, emit); err != nil {
			return err
		}
		next, err := os.Open(path)
		if err != nil {
			return err
		}
		_ = file.Close()
		file = next
		reader.Reset(file)
	}
}

// emitLines emits every complete line available and returns the unterminated rest.
func emitLines(reader *bufio.Reader, partial string, emit func(string)) (string, error) {
	for {
		chunk, err := reader.ReadString('\n')
		if errors.Is(err, io.EOF) {
			return partial + chunk, nil
		}
		if err != nil {
			return partial, err
		}
		emit(strings.TrimSuffix(partial+chunk, "\n"))
		partial = ""
	}
}

// tailFiles concatenates the lines of paths in order, skipping missing files,
// and keeps the last limit lines (all when limit <= 0).
func tailFiles(paths []string, limit int) ([]string, error) {
	var lines []string
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}

		scanner := bufio.NewScanner(file)
		buffer := make([]byte, 0, 64*1024)
		scanner.Buffer(buffer, 1024*1024)
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		closeErr := file.Close()
		if err := scanner.Err(); err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}

	if limit > 0 && len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return lines, nil
}

func (s *Service) ensureFileLocked() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	if s.file != nil {
		return nil
	}

	path := filepath.Join(s.dir, s.fileName)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, s.fileMode)
	if err != nil {
		return err
	}

	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}

	s.file = file
	s.size = info.Size()
	return nil
}

// rotateLocked shifts numbered backups upward before reopening the active log
// file. The caller must already hold s.mu.
func (s *Service) rotateLocked() error {
	if s.file != nil {
		if err := s.file.Close(); err != nil {
			return err
		}
		s.file = nil
		s.size = 0
	}

	for index := s.maxBackups - 1; index >= 1; index-- {
		source := s.rotatedPath(index)
		target := s.rotatedPath(index + 1)
		if index == s.maxBackups-1 {
			_ = os.Remove(target)
		}
		if err := os.Rename(source, target); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	current := filepath.Join(s.dir, s.fileName)
	firstBackup := s.rotatedPath(1)
	if err := os.Rename(current, firstBackup); err != nil && !os.IsNotExist(err) {
		return err
	}

	return s.ensureFileLocked()
}

func (s *Service) rotatedPath(index int) string {
	return filepath.Join(s.dir, fmt.Sprintf("%s.%d", s.fileName, index))
}

func (s *Service) logPathsLocked() []string {
	paths := make([]string, 0, s.maxBackups+1)
	for index := s.maxBackups; index >= 1; index-- {
		paths = append(paths, s.rotatedPath(index))
	}
	paths = append(paths, filepath.Join(s.dir, s.fileName))
	return paths
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
