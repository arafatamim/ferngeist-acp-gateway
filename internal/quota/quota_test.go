package quota

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollectPreservesQuotaAndOmitsIdentity(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "codexbar")
	data := `#!/bin/sh
if [ "$1" = config ]; then printf '%s' '[{"provider":"codex","enabled":true}]'; exit; fi
printf '%s' '[{"provider":"codex","source":"oauth","usage":{"primary":{"usedPercent":28,"windowMinutes":300,"resetsAt":"2026-10-31T14:15:00Z"},"updatedAt":"2026-10-31T10:00:00Z","accountEmail":"private@example.com"}}]'
`
	if err := os.WriteFile(script, []byte(data), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	result, err := Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result), `"usedPercent":28`) || strings.Contains(string(result), "private@example.com") {
		t.Fatalf("unexpected response: %s", result)
	}
}

func TestCollectPreservesSuccessfulProvidersWhenOneFails(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "codexbar")
	data := `#!/bin/sh
if [ "$1" = config ]; then printf '%s' '[{"provider":"codex","enabled":true},{"provider":"commandcode","enabled":true}]'; exit; fi
if [ "$3" = codex ]; then printf '%s' '{"provider":"codex","usage":{"primary":{"usedPercent":28}}}'; exit; fi
printf '%s' '{"provider":"commandcode","error":{"message":"secret diagnostic","kind":"provider","code":1}}'
exit 1
`
	if err := os.WriteFile(script, []byte(data), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	result, err := Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Providers []struct {
			Provider string `json:"provider"`
			Usage    *struct {
				Primary *struct {
					UsedPercent *float64 `json:"usedPercent"`
				} `json:"primary"`
			} `json:"usage"`
			Error *struct {
				Kind string `json:"kind"`
			} `json:"error"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Providers) != 2 || response.Providers[0].Usage == nil || response.Providers[0].Usage.Primary == nil || *response.Providers[0].Usage.Primary.UsedPercent != 28 || response.Providers[1].Error == nil || response.Providers[1].Error.Kind != "provider" || strings.Contains(string(result), "secret diagnostic") {
		t.Fatalf("unexpected response: %s", result)
	}
}

func TestCollectKeepsLabelsAndExtraWindows(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "codexbar")
	data := `#!/bin/sh
if [ "$1" = config ]; then printf '%s' '[{"provider":"codex","enabled":true}]'; exit; fi
printf '%s' '{"provider":"codex","rateWindowLabels":{"primary":"Session"},"usage":{"extraRateWindows":[{"id":"spark","title":"Spark","window":{"usedPercent":0,"usageKnown":false}}]}}'
`
	if err := os.WriteFile(script, []byte(data), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	result, err := Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"rateWindowLabels":{"primary":"Session"}`, `"extraRateWindows"`, `"usageKnown":false`, `"title":"Spark"`} {
		if !strings.Contains(string(result), want) {
			t.Fatalf("missing %s in %s", want, result)
		}
	}
}

func TestCollectReturnsOtherProvidersWhenOneTimesOut(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "codexbar")
	data := `#!/bin/sh
if [ "$1" = config ]; then printf '%s' '[{"provider":"fast","enabled":true},{"provider":"slow","enabled":true}]'; exit; fi
if [ "$3" = slow ]; then exec /bin/sleep 30; fi
printf '%s' '{"provider":"fast","usage":{"primary":{"usedPercent":42}}}'
`
	if err := os.WriteFile(script, []byte(data), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	result, err := Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result), `"usedPercent":42`) || !strings.Contains(string(result), `"provider":"slow"`) || !strings.Contains(string(result), `"kind":"provider"`) {
		t.Fatalf("unexpected response: %s", result)
	}
}
