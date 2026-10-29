package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
)

// pairStartResponse is returned when a new pairing challenge is created.
type pairStartResponse struct {
	ChallengeID string    `json:"challengeId"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// pairCompleteRequest submits a pairing code and optional proof public key
// to complete the device pairing handshake.
type pairCompleteRequest struct {
	ChallengeID    string `json:"challengeId"`
	Code           string `json:"code"`
	DeviceName     string `json:"deviceName"`
	ProofPublicKey string `json:"proofPublicKey,omitempty"` // ECDSA public key for proof-of-possession
}

// pairCompleteResponse contains the credential issued after successful pairing.
// GatewayID is this gateway's stable, user-independent instance id; the client
// stores it as the server identity it later matches incoming pushes against to
// deep-link into the right chat.
type pairCompleteResponse struct {
	DeviceID   string    `json:"deviceId"`
	DeviceName string    `json:"deviceName"`
	Token      string    `json:"token"`
	ExpiresAt  time.Time `json:"expiresAt"`
	Scopes     []string  `json:"scopes,omitempty"`
	GatewayID  string    `json:"gatewayId,omitempty"`
}

// pairStatusResponse exposes the state of a pairing challenge to public clients.
type pairStatusResponse struct {
	ChallengeID     string    `json:"challengeId"`
	ExpiresAt       time.Time `json:"expiresAt"`
	State           string    `json:"state"`
	CompletedDevice string    `json:"completedDevice,omitempty"`
	CompletedID     string    `json:"completedDeviceId,omitempty"`
	CompletedExpiry time.Time `json:"completedDeviceExpiresAt,omitempty"`
}

// adminPairingResponse is the admin-facing representation of a pairing challenge,
// including the deep-link payload for mobile clients.
type adminPairingResponse struct {
	ChallengeID     string    `json:"challengeId"`
	Code            string    `json:"code"`
	ExpiresAt       time.Time `json:"expiresAt"`
	State           string    `json:"state"`
	Scheme          string    `json:"scheme,omitempty"`
	Host            string    `json:"host,omitempty"`
	Payload         string    `json:"payload,omitempty"` // ferngeist-gateway://pair?... deep link
	CompletedDevice string    `json:"completedDevice,omitempty"`
	CompletedID     string    `json:"completedDeviceId,omitempty"`
	CompletedExpiry time.Time `json:"completedDeviceExpiresAt,omitempty"`
}

// adminDevicesResponse wraps the list of paired devices for the admin endpoint.
type adminDevicesResponse struct {
	Devices []adminDeviceResponse `json:"devices"`
}

// adminDeviceResponse represents a single paired device in admin listings.
type adminDeviceResponse struct {
	DeviceID   string    `json:"deviceId"`
	DeviceName string    `json:"deviceName"`
	ExpiresAt  time.Time `json:"expiresAt"`
	Scopes     []string  `json:"scopes,omitempty"`
}

// pairingTarget holds the reachability information for mobile device pairing.
type pairingTarget struct {
	Scheme string
	Host   string
}

func (s *Server) handlePairStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.allowPairingRequest(r, true) {
		writeError(w, http.StatusTooManyRequests, "pairing temporarily rate-limited")
		return
	}

	challenge, err := s.pairing.StartPairing()
	if err != nil {
		if errors.Is(err, pairing.ErrPairingNotArmed) {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to start pairing")
		return
	}

	writeJSON(w, http.StatusOK, pairStartResponse{
		ChallengeID: challenge.ID,
		ExpiresAt:   challenge.ExpiresAt,
	})
}

// handlePairComplete validates the pairing code and optional proof-of-possession
// to issue a credential. It enforces rate limiting, attempt-based lockout
// (per-IP and per-challenge), and proof public key validation before calling
// into the pairing service. On success, the attempt counters are reset.
func (s *Server) handlePairComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.allowPairingRequest(r, false) {
		writeError(w, http.StatusTooManyRequests, "pairing temporarily rate-limited")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, jsonBodyLimit)

	var request pairCompleteRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	challengeKey := pairingAttemptKey(request.ChallengeID)
	if source := requestSourceIP(r); s.attempts.isLocked(source, challengeKey, s.now()) {
		writeError(w, http.StatusTooManyRequests, "pairing temporarily locked")
		return
	}
	if s.cfg.RequireProofOfPossession && strings.TrimSpace(request.ProofPublicKey) == "" {
		writeError(w, http.StatusBadRequest, "proof public key required")
		return
	}
	if proofKey := strings.TrimSpace(request.ProofPublicKey); proofKey != "" {
		if _, err := parseProofPublicKey(proofKey); err != nil {
			writeError(w, http.StatusBadRequest, "invalid proof public key")
			return
		}
	}

	credential, err := s.pairing.CompletePairingWithProofKey(request.ChallengeID, request.Code, request.DeviceName, strings.TrimSpace(request.ProofPublicKey))
	if err != nil {
		switch {
		case errors.Is(err, pairing.ErrInvalidDeviceName):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, pairing.ErrCodeMismatch):
			s.attempts.recordFailure(requestSourceIP(r), challengeKey, s.now())
			writeError(w, http.StatusUnauthorized, "pairing failed")
		case errors.Is(err, pairing.ErrChallengeNotFound), errors.Is(err, pairing.ErrChallengeExpired):
			writeError(w, http.StatusUnauthorized, "pairing failed")
		default:
			writeError(w, http.StatusInternalServerError, "failed to complete pairing")
		}
		return
	}
	s.attempts.reset(requestSourceIP(r), challengeKey)

	writeJSON(w, http.StatusOK, pairCompleteResponse{
		DeviceID:   credential.DeviceID,
		DeviceName: credential.DeviceName,
		Token:      credential.Token,
		ExpiresAt:  credential.ExpiresAt,
		Scopes:     credential.Scopes,
		GatewayID:  s.cfg.GatewayID,
	})
}

func (s *Server) handlePairStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.pairing.GetChallengeStatus(r.PathValue("challengeId"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, publicPairStatusResponse(status))
}

func (s *Server) handleAdminPairingStart(w http.ResponseWriter, r *http.Request) {
	challenge, err := s.pairing.StartPairingWithLocalApproval()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start pairing")
		return
	}

	target, _ := s.pairingTarget()

	writeJSON(w, http.StatusOK, s.adminPairingResponse(pairing.ChallengeStatus{
		ID:        challenge.ID,
		Code:      challenge.Code,
		ExpiresAt: challenge.ExpiresAt,
		State:     pairing.ChallengeStateActive,
	}, target))
}

func (s *Server) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	status := s.statusSnapshot(true)
	response := adminStatusResponse{
		statusResponse:  status,
		AdminListenAddr: s.cfg.AdminListenAddr,
	}
	if target, err := s.pairingTarget(); err != nil {
		response.PairingTarget = adminPairingTargetInfo{Reachable: false, Error: err.Error()}
	} else {
		response.PairingTarget = adminPairingTargetInfo{Reachable: true, Scheme: target.Scheme, Host: target.Host}
	}
	if challenge, ok := s.pairing.ActiveChallenge(); ok {
		activePairing := s.adminPairingResponse(challenge, pairingTarget{})
		response.ActivePairing = &activePairing
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleAdminPairingStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.pairing.GetChallengeStatus(r.PathValue("challengeId"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	target, _ := s.pairingTarget()
	writeJSON(w, http.StatusOK, s.adminPairingResponse(status, target))
}

func (s *Server) handleAdminPairingCancel(w http.ResponseWriter, r *http.Request) {
	status, err := s.pairing.CancelChallenge(r.PathValue("challengeId"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	target, _ := s.pairingTarget()
	writeJSON(w, http.StatusOK, s.adminPairingResponse(status, target))
}

func (s *Server) handleAdminDevices(w http.ResponseWriter, _ *http.Request) {
	devices := s.pairing.ListDevices()
	response := make([]adminDeviceResponse, 0, len(devices))
	for _, device := range devices {
		response = append(response, adminDeviceResponse{
			DeviceID:   device.DeviceID,
			DeviceName: device.DeviceName,
			ExpiresAt:  device.ExpiresAt,
			Scopes:     append([]string(nil), device.Scopes...),
		})
	}
	writeJSON(w, http.StatusOK, adminDevicesResponse{Devices: response})
}

func (s *Server) handleAdminDeviceRevoke(w http.ResponseWriter, r *http.Request) {
	device, err := s.pairing.RevokeDevice(r.PathValue("deviceId"))
	if err != nil {
		if errors.Is(err, pairing.ErrDeviceNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to revoke paired device")
		return
	}
	writeJSON(w, http.StatusOK, adminDeviceResponse{
		DeviceID:   device.DeviceID,
		DeviceName: device.DeviceName,
		ExpiresAt:  device.ExpiresAt,
		Scopes:     append([]string(nil), device.Scopes...),
	})
}

func (s *Server) adminPairingResponse(status pairing.ChallengeStatus, target pairingTarget) adminPairingResponse {
	response := adminPairingResponse{
		ChallengeID: status.ID,
		Code:        status.Code,
		ExpiresAt:   status.ExpiresAt,
		State:       string(status.State),
	}
	if target.Scheme != "" && target.Host != "" {
		response.Scheme = target.Scheme
		response.Host = target.Host
		response.Payload = buildPairingPayload(target, status.ID, status.Code)
	}
	if status.CompletedDevice != nil {
		response.CompletedID = status.CompletedDevice.DeviceID
		response.CompletedDevice = status.CompletedDevice.DeviceName
		response.CompletedExpiry = status.CompletedDevice.ExpiresAt
	}
	return response
}

func buildPairingPayload(target pairingTarget, challengeID, code string) string {
	values := url.Values{}
	values.Set("scheme", target.Scheme)
	values.Set("host", target.Host)
	values.Set("challengeId", challengeID)
	values.Set("code", code)
	return "ferngeist-gateway://pair?" + values.Encode()
}

// pairingTarget determines how a mobile client can reach this gateway for pairing.
// The resolution order is:
//  1. PublicBaseURL if configured (for remote/reverse proxy setups)
//  2. Listen address host if it's routable (non-loopback, non-unspecified)
//  3. Error if LAN is disabled (user must enable LAN or set public URL)
//  4. Error if listen address is loopback (user must use --lan flag)
//  5. First available LAN interface IPv4/IPv6 address
func (s *Server) pairingTarget() (pairingTarget, error) {
	publicBaseURL := s.publicBaseURL()
	if publicBaseURL != "" {
		parsed, err := url.Parse(publicBaseURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return pairingTarget{}, fmt.Errorf("configured public base URL is invalid")
		}
		return pairingTarget{Scheme: parsed.Scheme, Host: parsed.Host}, nil
	}

	listenHost, port, err := net.SplitHostPort(s.cfg.ListenAddr)
	if err != nil || strings.TrimSpace(port) == "" {
		return pairingTarget{}, fmt.Errorf("gateway listen address is invalid")
	}
	host := strings.Trim(strings.TrimSpace(listenHost), "[]")
	if isRoutableHost(host) {
		return pairingTarget{Scheme: "http", Host: net.JoinHostPort(host, port)}, nil
	}
	if !s.cfg.EnableLAN {
		return pairingTarget{}, fmt.Errorf("gateway is running in local-only mode; pairing requires a phone-reachable address. Set FERNGEIST_GATEWAY_PUBLIC_BASE_URL or run `ferngeist-gateway daemon run --lan`")
	}
	if host != "" && (strings.EqualFold(host, "localhost") || isLoopbackHost(host)) {
		return pairingTarget{}, fmt.Errorf("gateway LAN pairing requires a non-loopback listen address; run `ferngeist-gateway daemon run --lan` or set FERNGEIST_GATEWAY_LISTEN_ADDR=0.0.0.0:5788")
	}

	lanHost, err := firstLANHost()
	if err != nil {
		return pairingTarget{}, err
	}
	return pairingTarget{Scheme: "http", Host: net.JoinHostPort(lanHost, port)}, nil
}

// isRoutableHost returns true if the host is reachable from other devices on
// the network. Loopback addresses, localhost, and unspecified (0.0.0.0) are
// considered non-routable.
func isRoutableHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsLoopback() && !ip.IsUnspecified()
	}
	return true
}

// isLoopbackHost returns true if the host resolves to a loopback IP address.
func isLoopbackHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// firstLANHost scans local network interfaces to find the first non-loopback,
// non-unspecified IPv4 address. If no IPv4 address is found, it falls back to
// the first global unicast IPv6 address. This is used for LAN pairing when the
// listen address is loopback.
func firstLANHost() (string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("failed to inspect local network interfaces")
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch value := addr.(type) {
			case *net.IPNet:
				ip = value.IP
			case *net.IPAddr:
				ip = value.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
				continue
			}
			if v4 := ip.To4(); v4 != nil {
				return v4.String(), nil
			}
			if ip.IsGlobalUnicast() {
				return ip.String(), nil
			}
		}
	}
	return "", fmt.Errorf("no LAN address is available for pairing")
}

func publicPairStatusResponse(status pairing.ChallengeStatus) pairStatusResponse {
	response := pairStatusResponse{
		ChallengeID: status.ID,
		ExpiresAt:   status.ExpiresAt,
		State:       string(status.State),
	}
	if status.CompletedDevice != nil {
		response.CompletedDevice = status.CompletedDevice.DeviceName
		response.CompletedID = status.CompletedDevice.DeviceID
		response.CompletedExpiry = status.CompletedDevice.ExpiresAt
	}
	return response
}

// allowPairingRequest is the public entry point for rate limiting checks.
func (s *Server) allowPairingRequest(r *http.Request, isStart bool) bool {
	if s == nil || s.rateLimiter == nil {
		return true
	}
	return s.rateLimiter.allow(requestSourceIP(r), isStart, s.now())
}
