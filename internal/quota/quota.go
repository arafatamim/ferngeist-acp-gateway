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
	DisplayName      string         `json:"displayName,omitempty"`
	Plan             string         `json:"plan,omitempty"`
	Account          string         `json:"account,omitempty"`
	Costs            []cost         `json:"costs,omitempty"`
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
	DisplayName string `json:"displayName"`
	Provider    string `json:"provider"`
	Enabled     bool   `json:"enabled"`
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
			providers = append(providers, provider{Provider: p.Provider, DisplayName: p.DisplayName})
		}
	}
	if len(providers) > 100 {
		return nil, errors.New("too many providers")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	var costs []costSnapshot
	go func() {
		defer wg.Done()
		costs = collectCosts(ctx)
	}()
	collectProviders(ctx, providers)
	wg.Wait()
	attachCosts(providers, costs)
	if parent.Err() != nil {
		return nil, parent.Err()
	}
	if providers == nil {
		providers = []provider{}
	}
	return json.Marshal(response{Providers: providers})
}

func collectProviders(ctx context.Context, providers []provider) {
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
			p := collectProvider(ctx, providers[i].Provider)
			p.DisplayName = providers[i].DisplayName
			providers[i] = p
		}(i)
	}
	wg.Wait()
}

type usageSnapshot struct {
	usage
	Identity *struct {
		LoginMethod  string `json:"loginMethod"`
		AccountEmail string `json:"accountEmail"`
	} `json:"identity"`
}

type providerSnapshot struct {
	Provider         string         `json:"provider"`
	Source           string         `json:"source"`
	RateWindowLabels *labels        `json:"rateWindowLabels"`
	Usage            *usageSnapshot `json:"usage"`
	Error            *providerError `json:"error"`
}

func collectProvider(ctx context.Context, id string) provider {
	unavailable := provider{Provider: id, Error: &providerError{Kind: "provider"}}
	data, err := run(ctx, "usage", "--provider", id, "--format", "json")
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return unavailable
	}
	data = bytes.TrimSpace(data)
	var snapshot providerSnapshot
	if len(data) > 0 && data[0] == '[' {
		var list []providerSnapshot
		if json.Unmarshal(data, &list) != nil || len(list) != 1 {
			return unavailable
		}
		snapshot = list[0]
	} else if json.Unmarshal(data, &snapshot) != nil {
		return unavailable
	}
	p := provider{Provider: snapshot.Provider, Source: snapshot.Source, RateWindowLabels: snapshot.RateWindowLabels, Error: snapshot.Error}
	if snapshot.Usage != nil {
		p.Usage = &snapshot.Usage.usage
		if identity := snapshot.Usage.Identity; identity != nil {
			p.Plan, p.Account = identity.LoginMethod, identity.AccountEmail
		}
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
	cmd.WaitDelay = time.Second
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

type cost struct {
	Label     string  `json:"label"`
	Amount    float64 `json:"amount"`
	Tokens    *int64  `json:"tokens,omitempty"`
	Currency  string  `json:"currency"`
	Estimated bool    `json:"estimated"`
}

type costSnapshot struct {
	Provider          string          `json:"provider"`
	SessionCostUSD    *float64        `json:"sessionCostUSD"`
	SessionTokens     *int64          `json:"sessionTokens"`
	Last30DaysCostUSD *float64        `json:"last30DaysCostUSD"`
	Last30DaysTokens  *int64          `json:"last30DaysTokens"`
	CurrencyCode      string          `json:"currencyCode"`
	Error             json.RawMessage `json:"error"`
}

func collectCosts(ctx context.Context) []costSnapshot {
	data, err := run(ctx, "cost", "--format", "json")
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return nil
	}
	var snapshots []costSnapshot
	if json.Unmarshal(data, &snapshots) != nil {
		return nil
	}
	return snapshots
}

func attachCosts(providers []provider, snapshots []costSnapshot) {
	byProvider := make(map[string][]cost)
	for _, snapshot := range snapshots {
		if snapshot.CurrencyCode != "USD" || (len(snapshot.Error) > 0 && string(snapshot.Error) != "null") {
			continue
		}
		rows := costRows(snapshot)
		byProvider[snapshot.Provider] = rows
	}
	for i := range providers {
		providers[i].Costs = byProvider[providers[i].Provider]
	}
}

func costRows(snapshot costSnapshot) []cost {
	var rows []cost
	for _, period := range []struct {
		label  string
		amount *float64
		tokens *int64
	}{
		{"Today", snapshot.SessionCostUSD, snapshot.SessionTokens},
		{"Last 30 days", snapshot.Last30DaysCostUSD, snapshot.Last30DaysTokens},
	} {
		if period.amount == nil || *period.amount < 0 {
			continue
		}
		tokens := period.tokens
		if tokens != nil && *tokens < 0 {
			tokens = nil
		}
		rows = append(rows, cost{Label: period.label, Amount: *period.amount, Tokens: tokens, Currency: "USD", Estimated: true})
	}
	return rows
}
