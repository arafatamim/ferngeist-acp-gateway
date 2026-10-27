package catalog

import (
	_ "embed"
	"encoding/base64"
	"strings"
)

//go:embed logos/command-code.svg
var commandCodeLogo string

//go:embed logos/oh-my-pi.svg
var ohMyPiLogo string

//go:embed logos/deepseek-harness.svg
var deepseekHarnessLogo string

//go:embed logos/codewhale.svg
var codewhaleLogo string

//go:embed logos/kiro.svg
var kiroLogo string

//go:embed logos/letta.svg
var lettaLogo string

//go:embed logos/mimo-code.svg
var mimoCodeLogo string

//go:embed logos/muse-code.svg
var museCodeLogo string

//go:embed logos/reasonix.svg
var reasonixLogo string

//go:embed logos/zcode.svg
var zcodeLogo string

//go:embed logos/aider.svg
var aiderLogo string

//go:embed logos/hermes.svg
var hermesLogo string

//go:embed logos/openclaw.svg
var openclawLogo string

//go:embed logos/openhands.svg
var openhandsLogo string

//go:embed logos/default.svg
var defaultLogo string

var defaultLogoURL = logoDataURL(defaultLogo)

var bundledLogos = map[string]string{
	"command-code":     logoDataURL(commandCodeLogo),
	"oh-my-pi":         logoDataURL(ohMyPiLogo),
	"deepseek-harness": logoDataURL(deepseekHarnessLogo),
	"codewhale":        logoDataURL(codewhaleLogo),
	"kiro":             logoDataURL(kiroLogo),
	"letta":            logoDataURL(lettaLogo),
	"mimo-code":        logoDataURL(mimoCodeLogo),
	"muse-code":        logoDataURL(museCodeLogo),
	"reasonix":         logoDataURL(reasonixLogo),
	"zcode":            logoDataURL(zcodeLogo),
	"aider":            logoDataURL(aiderLogo),
	"hermes":           logoDataURL(hermesLogo),
	"openclaw":         logoDataURL(openclawLogo),
	"openhands":        logoDataURL(openhandsLogo),
}

func logoDataURL(svg string) string {
	return "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(svg))
}

func bundledLogo(id string) string {
	id = strings.TrimPrefix(strings.ToLower(id), "custom-")
	switch id {
	case "commandcode":
		id = "command-code"
	case "omp":
		id = "oh-my-pi"
	case "dsh":
		id = "deepseek-harness"
	case "hermes-agent":
		id = "hermes"
	}
	if logo := bundledLogos[id]; logo != "" {
		return logo
	}
	return defaultLogoURL
}
