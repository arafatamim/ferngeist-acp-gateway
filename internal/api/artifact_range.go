package api

import (
	"net/http"
	"strconv"
	"strings"
)

// artifactRange selects one current-file read. If-Range never promises identity
// across mutable versions, even when a supplied date or tag appears to match.
func artifactRange(r *http.Request, size int64) (offset, length int64, status int) {
	values := r.Header.Values("Range")
	if r.Method != http.MethodGet || len(values) == 0 || len(r.Header.Values("If-Range")) > 0 {
		return 0, size, http.StatusOK
	}
	if size == 0 || len(values) != 1 || !strings.HasPrefix(values[0], "bytes=") || strings.Contains(values[0], ",") {
		return 0, 0, http.StatusRequestedRangeNotSatisfiable
	}
	first, last, ok := strings.Cut(strings.TrimPrefix(values[0], "bytes="), "-")
	if !ok {
		return 0, 0, http.StatusRequestedRangeNotSatisfiable
	}
	first, last = strings.TrimSpace(first), strings.TrimSpace(last)
	if first == "" {
		suffix, err := strconv.ParseUint(last, 10, 63)
		if err != nil || suffix == 0 {
			return 0, 0, http.StatusRequestedRangeNotSatisfiable
		}
		length = min(int64(suffix), size)
		return size - length, length, http.StatusPartialContent
	}
	start, err := strconv.ParseUint(first, 10, 63)
	if err != nil || int64(start) >= size {
		return 0, 0, http.StatusRequestedRangeNotSatisfiable
	}
	end := size - 1
	if last != "" {
		requested, err := strconv.ParseUint(last, 10, 63)
		if err != nil || requested < start {
			return 0, 0, http.StatusRequestedRangeNotSatisfiable
		}
		end = min(int64(requested), end)
	}
	return int64(start), end - int64(start) + 1, http.StatusPartialContent
}
