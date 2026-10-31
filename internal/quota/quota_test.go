package quota

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
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

func TestCollectBundlesProviderIconsAndOmitsUnknownIcons(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
config) printf '%s' '[{"provider":"claude","enabled":true},{"provider":"copilot","enabled":true},{"provider":"unknown","enabled":true},{"provider":"warp","enabled":true}]';;
usage) printf '{"provider":"%s","error":{"kind":"provider"}}' "$3";;
cost) printf '[]';;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "codexbar"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	data, err := Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Providers []struct {
			Provider string  `json:"provider"`
			Icon     *string `json:"icon"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Providers) != 4 {
		t.Fatalf("providers = %s", data)
	}
	for _, p := range payload.Providers {
		if p.Provider == "unknown" {
			if p.Icon != nil {
				t.Fatalf("unknown provider has icon: %s", data)
			}
			continue
		}
		if p.Icon == nil || !strings.HasPrefix(*p.Icon, "data:image/svg+xml;base64,") {
			t.Fatalf("%s: missing bundled SVG", p.Provider)
		}
		svg, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(*p.Icon, "data:image/svg+xml;base64,"))
		var image struct {
			ViewBox string `xml:"viewBox,attr"`
		}
		if err != nil || xml.Unmarshal(svg, &image) != nil || image.ViewBox == "" || !strings.Contains(string(svg), "currentColor") {
			t.Fatalf("%s: invalid monochrome SVG: %s", p.Provider, svg)
		}
		if p.Provider == "copilot" && !strings.Contains(string(svg), "GithubCopilot") {
			t.Fatalf("copilot must use GitHub Copilot artwork: %s", svg)
		}
	}
}

func TestCollectRelaysOpenRouterCreditDetails(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
config) printf '%s' '[{"provider":"openrouter","enabled":true}]';;
usage) printf '%s' '[{"provider":"openrouter","source":"api","usage":{"primary":null,"secondary":null,"tertiary":null,"updatedAt":"2026-10-31T10:00:00Z","identity":{"loginMethod":"Balance: $35.05","accountID":"hidden-id"},"details":[{"title":"Credits","rows":[{"label":"Remaining","value":"$35.05"},{"label":"Used","value":"$12.50"},{"label":"Total added","value":"$47.55"},{"label":"Secret","value":"hidden-secret"}]},{"title":"API key","rows":[{"label":"Today","value":"$0.25"},{"label":"This week","value":"$1.00"},{"label":"This month","value":"$4.00"},{"label":"API key used","value":"hidden-token"}]}]}}]';;
cost) printf '[]';;
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
	for _, want := range []string{`"plan":"Balance: $35.05"`, `"label":"Remaining","value":"$35.05"`, `"label":"Used","value":"$12.50"`, `"label":"Total added","value":"$47.55"`, `"label":"Today","value":"$0.25"`, `"label":"This week","value":"$1.00"`, `"label":"This month","value":"$4.00"`} {
		if !strings.Contains(string(result), want) {
			t.Fatalf("missing %s in %s", want, result)
		}
	}
	for _, secret := range []string{"hidden-id", "hidden-secret", "hidden-token"} {
		if strings.Contains(string(result), secret) {
			t.Fatalf("unexpected %s in %s", secret, result)
		}
	}
}

func TestCollectRelaysProviderBalancesAcrossFormats(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
config) printf '%s' '[{"provider":"vercel","enabled":true},{"provider":"grok","enabled":true},{"provider":"poe","enabled":true},{"provider":"hyper","enabled":true}]';;
usage) case "$3" in
vercel) printf '%s' '{"provider":"vercel","usage":{"details":[{"title":"Team credits","rows":[{"label":"Available balance","value":"$1,234.56"},{"label":"Lifetime spend","value":"$12.50"},{"label":"Secret","value":"hidden-secret"}]}]}}';;
grok) printf '%s' '{"provider":"grok","usage":{"providerCost":{"balance":14.46,"currencyCode":"USD","secret":"hidden-token"}}}';;
poe) printf '%s' '{"provider":"poe","usage":{"details":[{"title":"Points","rows":[{"label":"Current balance","value":"1.2K points"}]}]}}';;
hyper) printf '%s' '{"provider":"hyper","usage":{"providerCost":{"balance":"invalid","currencyCode":"USD"},"details":[{"title":"Hypercredits","rows":[{"label":"Balance","value":"1,234.50 HC"}]}]}}';;
esac;;
cost) printf '[]';;
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
	for _, want := range []string{`"label":"Available balance","value":"$1,234.56"`, `"label":"Lifetime spend","value":"$12.50"`, `"balance":14.46,"currencyCode":"USD"`, `"label":"Current balance","value":"1.2K points"`, `"label":"Balance","value":"1,234.50 HC"`} {
		if !strings.Contains(string(result), want) {
			t.Fatalf("missing %s in %s", want, result)
		}
	}
	for _, secret := range []string{"hidden-secret", "hidden-token"} {
		if strings.Contains(string(result), secret) {
			t.Fatalf("unexpected %s in %s", secret, result)
		}
	}
}
