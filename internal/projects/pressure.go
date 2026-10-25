package projects

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type pressureSource struct {
	path  string
	total uint64
	ready bool
}

type memoryPressure []pressureSource

func memoryPressureSources() memoryPressure {
	sources := memoryPressure{{path: "/proc/pressure/memory"}, {path: "/sys/fs/cgroup/memory.pressure"}}
	// PSI is a Linux kernel signal. Elsewhere, or when disabled/inaccessible,
	// warmTTL still bounds idle process retention without guessing at pressure.
	cgroups, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return sources
	}
	for line := range strings.SplitSeq(string(cgroups), "\n") {
		path, ok := strings.CutPrefix(line, "0::/")
		if !ok || path == "" || strings.HasPrefix(filepath.Clean(path), "..") {
			continue
		}
		sources = append(sources, pressureSource{path: filepath.Join("/sys/fs/cgroup", path, "memory.pressure")})
	}
	return sources
}

func (sources memoryPressure) increased() bool {
	pressure := false
	for i := range sources {
		source := &sources[i]
		total, err := readPressureTotal(source.path)
		if err != nil {
			// Missing or unreadable PSI is an unavailable signal, not evidence of pressure.
			source.ready = false
			continue
		}
		pressure = pressure || (source.ready && total > source.total)
		source.total, source.ready = total, true
	}
	return pressure
}

func readPressureTotal(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read memory pressure: %w", err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, "some ") {
			return parsePressureTotal(line)
		}
	}
	return 0, fmt.Errorf("memory pressure has no some total")
}

func parsePressureTotal(line string) (uint64, error) {
	for _, field := range strings.Fields(line) {
		total, ok := strings.CutPrefix(field, "total=")
		if !ok {
			continue
		}
		value, err := strconv.ParseUint(total, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse memory pressure: %w", err)
		}
		return value, nil
	}
	return 0, fmt.Errorf("memory pressure has no some total")
}
