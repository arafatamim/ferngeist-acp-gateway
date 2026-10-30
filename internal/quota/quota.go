// Package quota collects provider quota windows from CodexBar.
package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"sync"
	"time"
)

type window struct {
	UsageKnown       *bool    `json:"usageKnown,omitempty"`
	UsedPercent      *float64 `json:"usedPercent,omitempty"`
	WindowMinutes    *int     `json:"windowMinutes,omitempty"`
	ResetsAt         *string  `json:"resetsAt,omitempty"`
	ResetDescription *string  `json:"resetDescription,omitempty"`
}
type namedWindow struct {
	ID     string  `json:"id"`
	Title  string  `json:"title"`
	Window *window `json:"window"`
}
type labels struct {
	Primary   string `json:"primary,omitempty"`
	Secondary string `json:"secondary,omitempty"`
	Tertiary  string `json:"tertiary,omitempty"`
}
type usage struct {
	ExtraRateWindows []namedWindow `json:"extraRateWindows,omitempty"`
	Primary          *window       `json:"primary"`
	Secondary        *window       `json:"secondary"`
	Tertiary         *window       `json:"tertiary"`
	UpdatedAt        *string       `json:"updatedAt"`
}
type provider struct {
	RateWindowLabels *labels        `json:"rateWindowLabels,omitempty"`
	Provider         string         `json:"provider"`
	Source           string         `json:"source,omitempty"`
	Usage            *usage         `json:"usage"`
	Error            *providerError `json:"error,omitempty"`
}
type providerError struct {
	Kind string `json:"kind"`
}

type response struct {
	Providers []provider `json:"providers"`
}

type configuredProvider struct {
	Provider string `json:"provider"`
	Enabled  bool   `json:"enabled"`
}

// Collect returns a sanitized JSON snapshot of the configured providers.
func Collect(ctx context.Context) (json.RawMessage, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	data, err := run(ctx, "config", "providers", "--format", "json")
	if err != nil {
		return nil, err
	}
	var configured []configuredProvider
	if err := json.Unmarshal(data, &configured); err != nil || configured == nil {
		return nil, errors.New("invalid provider list")
	}
	var providers []provider
	for _, p := range configured {
		if p.Enabled && p.Provider != "" {
			providers = append(providers, provider{Provider: p.Provider})
		}
	}
	if len(providers) > 100 {
		return nil, errors.New("too many providers")
	}
	var wg sync.WaitGroup
	limit := make(chan struct{}, 8)
	for i := range providers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case limit <- struct{}{}:
				defer func() { <-limit }()
			case <-ctx.Done():
				providers[i].Error = &providerError{Kind: "provider"}
				return
			}
			providers[i] = collectProvider(ctx, providers[i].Provider)
		}(i)
	}
	wg.Wait()
	if parent.Err() != nil {
		return nil, parent.Err()
	}
	if providers == nil {
		providers = []provider{}
	}
	return json.Marshal(response{Providers: providers})
}

func collectProvider(ctx context.Context, id string) provider {
	unavailable := provider{Provider: id, Error: &providerError{Kind: "provider"}}
	data, err := run(ctx, "usage", "--provider", id, "--format", "json")
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return unavailable
	}
	data = bytes.TrimSpace(data)
	var p provider
	if len(data) > 0 && data[0] == '[' {
		var list []provider
		if json.Unmarshal(data, &list) != nil || len(list) != 1 {
			return unavailable
		}
		p = list[0]
	} else if json.Unmarshal(data, &p) != nil {
		return unavailable
	}
	if p.Provider != id || (p.Usage == nil && p.Error == nil) {
		return unavailable
	}
	if p.Error != nil {
		p.Error.Kind = "provider"
	} else if err != nil {
		return unavailable
	}
	return p
}

func run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "codexbar", args...)
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &out, remaining: 1 << 20}
	err := cmd.Run()
	return out.Bytes(), err
}

type limitedWriter struct {
	w         io.Writer
	remaining int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		return 0, errors.New("quota output too large")
	}
	n, err := w.w.Write(p)
	w.remaining -= n
	return n, err
}
