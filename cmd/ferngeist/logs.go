package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/logging"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/service"
)

func runLogs(tail int, follow bool) error {
	// An explicit log dir covers foreground `daemon run`; otherwise read the installed service's.
	dir := os.Getenv("FERNGEIST_GATEWAY_LOG_DIR")
	if dir == "" {
		serviceDir, err := service.NewManager().LogDir()
		if err != nil {
			return fmt.Errorf("resolve daemon log directory (set FERNGEIST_GATEWAY_LOG_DIR): %w", err)
		}
		dir = serviceDir
	}

	lines, err := logging.ReadGatewayLog(dir, tail)
	if err != nil {
		return fmt.Errorf("read gateway log: %w", err)
	}
	if len(lines) == 0 && !follow {
		fmt.Printf("No gateway log in %s\n", dir)
		return nil
	}
	for _, line := range lines {
		writePrettyLogLine(os.Stdout, line)
	}
	if !follow {
		return nil
	}

	ctx, stop := waitForSignal()
	defer stop()
	// shortcut: lines written between the read above and the follow's seek are skipped; fine for interactive use.
	err = logging.FollowGatewayLog(ctx, dir, 500*time.Millisecond, func(line string) {
		writePrettyLogLine(os.Stdout, line)
	})
	if err != nil {
		return fmt.Errorf("follow gateway log: %w", err)
	}
	return nil
}

// writePrettyLogLine renders one slog JSON line as `time LEVEL msg key=value...`;
// lines that are not JSON objects are printed unchanged.
func writePrettyLogLine(w io.Writer, line string) {
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
