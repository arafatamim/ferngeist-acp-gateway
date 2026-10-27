package catalog

import (
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strings"
	"testing"

	acpregistry "github.com/arafatamim/ferngeist-acp-gateway/internal/registry"
)

func TestBundledLogoFallback(t *testing.T) {
	service := &Service{
		baseDir: t.TempDir(),
		registry: fakeRegistrySource{snapshot: acpregistry.Snapshot{Agents: map[string]acpregistry.AgentEntry{
			"command-code": {ID: "command-code", Icon: "https://example.com/registry.svg"},
		}}},
		customs: func() []CustomAgent {
			return []CustomAgent{
				{ID: "custom-oh-my-pi", DisplayName: "Oh My Pi", Command: "omp"},
				{ID: "custom-deepseek-harness", DisplayName: "DeepSeek Harness", Command: "dsh"},
				{ID: "custom-command-code", DisplayName: "Command Code", Command: "commandcode"},
				{ID: "custom-codewhale", DisplayName: "Codewhale", Command: "codewhale"},
				{ID: "custom-kiro", DisplayName: "Kiro", Command: "kiro-cli"},
				{ID: "custom-letta", DisplayName: "Letta", Command: "letta"},
				{ID: "custom-mimo-code", DisplayName: "MiMo Code", Command: "mimo"},
				{ID: "custom-muse-code", DisplayName: "Muse Code", Command: "muse"},
				{ID: "custom-reasonix", DisplayName: "Reasonix", Command: "reasonix"},
				{ID: "custom-zcode", DisplayName: "ZCode", Command: "zcode"},
				{ID: "custom-other", DisplayName: "Other", Command: "other"},
			}
		},
	}
	service.Refresh()
	for _, agent := range service.List() {
		data, err := json.Marshal(agent)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Icon     string `json:"icon"`
			Registry struct {
				Icon string `json:"icon"`
			} `json:"registry"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatal(err)
		}
		switch agent.ID {
		case "custom-oh-my-pi", "custom-deepseek-harness", "custom-command-code",
			"custom-codewhale", "custom-kiro", "custom-letta", "custom-mimo-code",
			"custom-muse-code", "custom-reasonix", "custom-zcode", "custom-other":
			const prefix = "data:image/svg+xml;base64,"
			if !strings.HasPrefix(payload.Icon, prefix) {
				t.Fatalf("%s: missing bundled icon", agent.ID)
			}
			svg, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(payload.Icon, prefix))
			if err != nil || !strings.Contains(string(svg), "<svg") {
				t.Fatalf("%s: invalid SVG: %v", agent.ID, err)
			}
			var image struct {
				ViewBox string `xml:"viewBox,attr"`
			}
			if err := xml.Unmarshal(svg, &image); err != nil {
				t.Fatalf("%s: decode SVG: %v", agent.ID, err)
			}
			var x, y, width, height float64
			if _, err := fmt.Sscan(image.ViewBox, &x, &y, &width, &height); err != nil || width <= 0 || width != height {
				t.Fatalf("%s: non-square SVG viewBox %q", agent.ID, image.ViewBox)
			}
		case "command-code":
			if payload.Registry.Icon != "https://example.com/registry.svg" || payload.Icon != "" {
				t.Fatalf("registry icon lost precedence: %s", data)
			}
		}
	}
}
