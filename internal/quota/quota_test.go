package quota

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCollectRelaysDisplayIdentityWithoutOtherAccountDetails(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "codexbar")
	data := `#!/bin/sh
if [ "$1" = config ]; then printf '%s' '[{"provider":"codex","enabled":true,"displayName":"Codex"}]'; exit; fi
printf '%s' '[{"provider":"codex","source":"oauth","usage":{"primary":{"usedPercent":28,"windowMinutes":300,"resetsAt":"2026-10-31T14:15:00Z"},"updatedAt":"2026-10-31T10:00:00Z","accountEmail":"private@example.com","identity":{"loginMethod":"plus","accountEmail":"private@example.com","accountID":"hidden-account-id"},"accessToken":"hidden-token"}}]'
`
	if err := os.WriteFile(script, []byte(data), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	result, err := Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result), `"usedPercent":28`) {
		t.Fatalf("unexpected response: %s", result)
	}
	for _, want := range []string{`"displayName":"Codex"`, `"plan":"plus"`, `"account":"private@example.com"`} {
		if !strings.Contains(string(result), want) {
			t.Fatalf("missing %s in %s", want, result)
		}
	}
	for _, secret := range []string{"hidden-account-id", "hidden-token", "identity", "accountEmail"} {
		if strings.Contains(string(result), secret) {
			t.Fatalf("unexpected %s in %s", secret, result)
		}
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

func TestRunStopsWaitingForInheritedStdout(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "codexbar")
	data := "#!/bin/sh\n/bin/sleep 3 &\nexit 0\n"
	if err := os.WriteFile(script, []byte(data), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	started := time.Now()
	_, err := run(context.Background(), "usage")
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("error = %v, want ErrWaitDelay", err)
	}
	if elapsed := time.Since(started); elapsed > 2500*time.Millisecond {
		t.Fatalf("waited %v for inherited stdout", elapsed)
	}
}

func TestCollectRelaysOnlyCostSummariesForEnabledProviders(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
config) printf '%s' '[{"provider":"codex","enabled":true},{"provider":"disabled","enabled":false}]';;
usage) printf '%s' '{"provider":"codex","usage":{"primary":{"usedPercent":28},"providerCost":{"currencyCode":"Credits","used":100}}}';;
cost) printf '%s' '[{"provider":"codex","sessionCostUSD":0,"sessionTokens":0,"last30DaysCostUSD":12.5,"last30DaysTokens":4200,"currencyCode":"USD","daily":[{"path":"hidden-project-path"}],"projects":[{"path":"hidden-project-path"}]},{"provider":"disabled","sessionCostUSD":99,"currencyCode":"USD"}]';;
*) exit 1;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "codexbar"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	result, err := Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"label":"Today","amount":0,"tokens":0,"currency":"USD","estimated":true`, `"label":"Last 30 days","amount":12.5,"tokens":4200,"currency":"USD","estimated":true`} {
		if !strings.Contains(string(result), want) {
			t.Fatalf("missing %s in %s", want, result)
		}
	}
	for _, secret := range []string{"hidden-project-path", "disabled", "Credits"} {
		if strings.Contains(string(result), secret) {
			t.Fatalf("unexpected %s in %s", secret, result)
		}
	}
}

func TestCollectKeepsQuotasWhenCostsAreUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		exit    string
	}{
		{"command failure", "", "1"},
		{"malformed", "not json", "0"},
		{"missing amounts", `[{"provider":"codex","currencyCode":"USD","sessionTokens":10}]`, "0"},
		{"noncurrency credits", `[{"provider":"codex","currencyCode":"Credits","sessionCostUSD":5}]`, "0"},
		{"negative amount", `[{"provider":"codex","currencyCode":"USD","sessionCostUSD":-1}]`, "0"},
		{"provider error", `[{"provider":"codex","currencyCode":"USD","sessionCostUSD":5,"error":{"message":"hidden-error"}}]`, "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			script := `#!/bin/sh
case "$1" in
config) printf '%s' '[{"provider":"codex","enabled":true}]';;
usage) printf '%s' '{"provider":"codex","usage":{"primary":{"usedPercent":28}}}';;
cost) printf '%s' '` + tc.payload + `'; exit ` + tc.exit + `;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "codexbar"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			result, err := Collect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(result), `"usedPercent":28`) || strings.Contains(string(result), `"costs"`) || strings.Contains(string(result), "hidden-error") {
				t.Fatalf("unexpected response: %s", result)
			}
		})
	}
}
