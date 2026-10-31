package quota

import (
	"embed"
	"encoding/base64"
)

//go:embed logos/*.svg
var providerLogos embed.FS

func providerIcon(id string) string {
	// Provider IDs follow CodexBar; artwork names follow the bundled sources.
	switch id {
	case "alibabatokenplan":
		id = "alibaba"
	case "azureopenai":
		id = "azure"
	case "claudeplatform":
		id = "claude"
	case "clinepass":
		id = "cline"
	case "copilot":
		id = "githubcopilot"
	case "kilo":
		id = "kilocode"
	case "mimo":
		id = "xiaomimimo"
	case "nous":
		id = "nousresearch"
	case "opencodego":
		id = "opencode"
	case "qwencloud":
		id = "qwen"
	case "traecn":
		id = "trae"
	case "typesafe":
		id = "typesafeai"
	case "zed":
		id = "zedindustries"
	}
	data, err := providerLogos.ReadFile("logos/" + id + ".svg")
	if err != nil {
		// Unknown providers use the client's fallback glyph.
		return ""
	}
	return "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(data)
}
