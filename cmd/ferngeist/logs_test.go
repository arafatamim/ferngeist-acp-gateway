package main

import (
	"strings"
	"testing"
)

func TestPrettyLogLineFormatsSlogJSONAndPassesThroughOtherText(t *testing.T) {
	cases := []struct {
		name, line, wantSuffix string
	}{
		{"slog", `{"time":"2026-11-16T10:00:00Z","level":"WARN","msg":"agent exited","runtime":"rt-1","code":2,"err":"exit status 2","meta":{"a": 1}}`,
			` WARN  agent exited code=2 err="exit status 2" meta={"a":1} runtime=rt-1`},
		{"plain", "panic: boom", "panic: boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			writePrettyLogLine(&out, tc.line)
			if got := strings.TrimSuffix(out.String(), "\n"); !strings.HasSuffix(got, tc.wantSuffix) {
				t.Fatalf("got %q, want suffix %q", got, tc.wantSuffix)
			}
		})
	}
}
