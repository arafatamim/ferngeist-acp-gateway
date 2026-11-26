package logging

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const defaultLogFileName = "gateway.log"
const defaultLogFileMode = 0o600

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
	return newLogger(level, dir, maxSize, maxBackups, os.Stdout)
}

func newLogger(level, dir string, maxSize int64, maxBackups int, stdout io.Writer) (*slog.Logger, *Service, error) {
	service, err := NewService(dir, defaultLogFileName, maxSize, maxBackups)
	if err != nil {
		return nil, nil, err
	}

	// The rolling file writer goes FIRST so gateway.log is always written even
	// when stdout is unavailable (e.g. the daemon is spawned without a console
	// and its std handles are invalid, or the parent that owns the stdout pipe
	// died). Writing stdout first would drop the file log on any stdout error,
	// because io.MultiWriter stops at the first failing writer.
	return slog.New(slog.NewJSONHandler(io.MultiWriter(service, prettyWriter{stdout}), &slog.HandlerOptions{
		Level: parseLevel(level),
	})), service, nil
}

// prettyWriter renders each JSON record slog writes as the same readable
// `time LEVEL msg key=value` line `ferngeist-gateway logs` prints. The file
// keeps JSON so the log stays machine-readable.
// shortcut: one record per Write, which slog's JSON handler always does.
type prettyWriter struct{ w io.Writer }

func (p prettyWriter) Write(b []byte) (int, error) {
	WritePrettyLogLine(p.w, strings.TrimSuffix(string(b), "\n"))
	return len(b), nil
}

// NewService opens the rolling log with owner-only permissions on Unix.
func NewService(dir, fileName string, maxSize int64, maxBackups int) (*Service, error) {
	return NewServiceWithMode(dir, fileName, maxSize, maxBackups, defaultLogFileMode)
}

// NewServiceWithMode is like NewService but with a configurable file mode for
// the active log (and its rotated backups). The gateway and frame logs use
// 0600 because diagnostics can include sensitive data.
func NewServiceWithMode(dir, fileName string, maxSize int64, maxBackups int, mode os.FileMode) (*Service, error) {
	if maxSize <= 0 {
		maxSize = 1024 * 1024
	}
	if maxBackups <= 0 {
		maxBackups = 3
	}
	if mode == 0 {
		mode = defaultLogFileMode
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
	if err := service.restrictBackups(); err != nil {
		_ = service.Close() // Startup failed before writing any records.
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

// WritePrettyLogLine renders one slog JSON line as `time LEVEL msg key=value...`;
// lines that are not JSON objects are printed unchanged.
func WritePrettyLogLine(w io.Writer, line string) {
	var record map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &record) != nil {
		fmt.Fprintln(w, line)
		return
	}

	var stamp, level, msg string
	_ = json.Unmarshal(record["time"], &stamp) // missing or malformed fields render empty
	_ = json.Unmarshal(record["level"], &level)
	_ = json.Unmarshal(record["msg"], &msg)
	if parsed, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
		stamp = parsed.Local().Format("2006-01-02 15:04:05")
	}
	delete(record, "time")
	delete(record, "level")
	delete(record, "msg")

	var out strings.Builder
	fmt.Fprintf(&out, "%s %-5s %s", stamp, level, msg)
	keys := make([]string, 0, len(record))
	for key := range record {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&out, " %s=%s", key, attrValue(record[key]))
	}
	fmt.Fprintln(w, out.String())
}

// attrValue unquotes plain strings and keeps every other value as compact JSON.
func attrValue(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil && !strings.ContainsAny(text, " \t\n\"") {
		return text
	}
	if text != "" {
		return fmt.Sprintf("%q", text)
	}
	var compact bytes.Buffer
	if json.Compact(&compact, raw) != nil {
		return string(raw)
	}
	return compact.String()
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
	file, err := s.openActiveLog(path)
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
