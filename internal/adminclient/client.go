package adminclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/config"
)

// ErrDaemonUnreachable marks a request that never reached an HTTP response, so
// the daemon is not running (or not on this address). Callers use IsDaemonUnreachable
// to report one recovery hint and one exit code everywhere instead of dumping a
// dial error per command.
var ErrDaemonUnreachable = errors.New("daemon not reachable")

// DaemonUnreachableHint is the recovery advice every surface prints, so the
// wording cannot drift between commands.
const DaemonUnreachableHint = "start the daemon with 'ferngeist-gateway daemon run', or install it as a service with 'ferngeist-gateway daemon install'"

// IsDaemonUnreachable reports whether err means the daemon did not answer.
func IsDaemonUnreachable(err error) bool { return errors.Is(err, ErrDaemonUnreachable) }

// Client talks to the daemon's loopback admin API.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

type PairingStatus struct {
	ChallengeID              string    `json:"challengeId"`
	Code                     string    `json:"code"`
	ExpiresAt                time.Time `json:"expiresAt"`
	State                    string    `json:"state"`
	Scheme                   string    `json:"scheme,omitempty"`
	Host                     string    `json:"host,omitempty"`
	Payload                  string    `json:"payload,omitempty"`
	CompletedDevice          string    `json:"completedDevice,omitempty"`
	CompletedDeviceID        string    `json:"completedDeviceId,omitempty"`
	CompletedDeviceExpiresAt time.Time `json:"completedDeviceExpiresAt,omitempty"`
}

type DaemonStatus struct {
	Name              string            `json:"name"`
	Version           string            `json:"version"`
	ListenAddr        string            `json:"listenAddr"`
	AdminListenAddr   string            `json:"adminListenAddr"`
	LANEnabled        bool              `json:"lanEnabled"`
	PairedDeviceCount int               `json:"pairedDeviceCount"`
	Remote            RemoteStatus      `json:"remote"`
	PairingTarget     PairingTargetInfo `json:"pairingTarget"`
	ActivePairing     *PairingStatus    `json:"activePairing,omitempty"`
	UptimeSeconds     int64             `json:"uptimeSeconds"`
}

type RemoteStatus struct {
	Configured   bool   `json:"configured"`
	Mode         string `json:"mode,omitempty"`
	Scope        string `json:"scope,omitempty"`
	Healthy      bool   `json:"healthy"`
	Warning      string `json:"warning,omitempty"`
	PublicURL    string `json:"publicUrl,omitempty"`
	AuthRequired bool   `json:"authRequired,omitempty"`
	AuthURL      string `json:"authUrl,omitempty"`
}

type PairingTargetInfo struct {
	Reachable bool   `json:"reachable"`
	Scheme    string `json:"scheme,omitempty"`
	Host      string `json:"host,omitempty"`
	Error     string `json:"error,omitempty"`
}

type Device struct {
	DeviceID   string    `json:"deviceId"`
	DeviceName string    `json:"deviceName"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

type devicesResponse struct {
	Devices []Device `json:"devices"`
}

// Agent is one gateway-visible agent. catalog.Agent serializes a superset of
// these fields; this is the subset the CLI acts on.
type Agent struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Source      string `json:"source"`
	Detected    bool   `json:"detected"`
	Hint        string `json:"hint"`
	Launch      struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	} `json:"launch"`
}

// AgentInput is the client-supplied shape of a custom agent. Empty fields are
// omitted: on update, omission means "unchanged" and only args are replaced
// wholesale.
type AgentInput struct {
	DisplayName string `json:"displayName,omitempty"`
	Command     string `json:"command,omitempty"`
	// Args has no omitempty: the admin API reads an omitted args as "unchanged"
	// and an empty list as "clear", so an empty list must reach the wire.
	Args []string `json:"args"`
	Hint string   `json:"hint,omitempty"`
}

type agentsResponse struct {
	Agents []Agent `json:"agents"`
}

type customAgentDeleteResponse struct {
	Deleted string `json:"deleted"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func New(cfg config.Config) *Client {
	return &Client{
		baseURL: strings.TrimRight("http://"+cfg.AdminListenAddr, "/"),
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (c *Client) StartPairing(ctx context.Context) (PairingStatus, error) {
	return doJSON[PairingStatus](c, ctx, http.MethodPost, "/admin/v1/pairings/start", nil)
}

func (c *Client) Status(ctx context.Context) (DaemonStatus, error) {
	return doJSON[DaemonStatus](c, ctx, http.MethodGet, "/admin/v1/status", nil)
}

func (c *Client) GetPairing(ctx context.Context, challengeID string) (PairingStatus, error) {
	return doJSON[PairingStatus](c, ctx, http.MethodGet, "/admin/v1/pairings/"+challengeID, nil)
}

func (c *Client) CancelPairing(ctx context.Context, challengeID string) (PairingStatus, error) {
	return doJSON[PairingStatus](c, ctx, http.MethodDelete, "/admin/v1/pairings/"+challengeID, nil)
}

func (c *Client) ListDevices(ctx context.Context) ([]Device, error) {
	response, err := doJSON[devicesResponse](c, ctx, http.MethodGet, "/admin/v1/devices", nil)
	if err != nil {
		return nil, err
	}
	return response.Devices, nil
}

func (c *Client) RevokeDevice(ctx context.Context, deviceID string) (Device, error) {
	return doJSON[Device](c, ctx, http.MethodDelete, "/admin/v1/devices/"+deviceID, nil)
}

// ListAgents returns the full catalog the gateway can launch, custom agents
// included.
func (c *Client) ListAgents(ctx context.Context) ([]Agent, error) {
	response, err := doJSON[agentsResponse](c, ctx, http.MethodGet, "/admin/v1/agents", nil)
	if err != nil {
		return nil, err
	}
	return response.Agents, nil
}

func (c *Client) AddCustomAgent(ctx context.Context, in AgentInput) (Agent, error) {
	return doJSON[Agent](c, ctx, http.MethodPost, "/admin/v1/agents/custom", in)
}

func (c *Client) UpdateCustomAgent(ctx context.Context, id string, in AgentInput) (Agent, error) {
	return doJSON[Agent](c, ctx, http.MethodPut, "/admin/v1/agents/custom/"+id, in)
}

// RemoveCustomAgent deletes a custom agent. A rejected delete (unknown id, cap,
// live runtime) comes back as the gateway's error message.
func (c *Client) RemoveCustomAgent(ctx context.Context, id string) error {
	_, err := doJSON[customAgentDeleteResponse](c, ctx, http.MethodDelete, "/admin/v1/agents/custom/"+id, nil)
	return err
}

func doJSON[T any](c *Client, ctx context.Context, method, path string, body any) (T, error) {
	var zero T

	var requestBody []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return zero, err
		}
		requestBody = encoded
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(requestBody))
	if err != nil {
		return zero, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	response, err := c.httpClient.Do(req)
	if err != nil {
		return zero, daemonUnreachable(c.baseURL, err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var apiError errorResponse
		if err := json.NewDecoder(response.Body).Decode(&apiError); err == nil && strings.TrimSpace(apiError.Error) != "" {
			return zero, errors.New(apiError.Error)
		}
		return zero, fmt.Errorf("admin api request failed: %s", response.Status)
	}

	if err := json.NewDecoder(response.Body).Decode(&zero); err != nil {
		return zero, err
	}
	return zero, nil
}

// daemonUnreachable wraps a transport-level failure from http.Client.Do. Such
// an error means no HTTP response was ever produced — the connection was
// refused, timed out, reset, or the host did not resolve — which for a
// loopback admin API can only mean the daemon is not answering. Matching on the
// error type rather than on message substrings covers every one of those cases
// with one code path and one message.
func daemonUnreachable(baseURL string, cause error) error {
	var urlErr *url.Error
	if !errors.As(cause, &urlErr) {
		return cause
	}
	return &unreachableError{baseURL: baseURL, cause: cause}
}

// unreachableError keeps the transport cause for logs while showing the
// operator one actionable line.
type unreachableError struct {
	baseURL string
	cause   error
}

func (e *unreachableError) Error() string {
	return fmt.Sprintf("%v at %s\nHint: %s", ErrDaemonUnreachable, e.baseURL, DaemonUnreachableHint)
}

// Unwrap exposes both the sentinel (for IsDaemonUnreachable) and the transport
// cause.
func (e *unreachableError) Unwrap() []error { return []error{ErrDaemonUnreachable, e.cause} }
