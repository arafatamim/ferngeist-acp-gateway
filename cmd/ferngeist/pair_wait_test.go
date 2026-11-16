package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/adminclient"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/config"
)

func TestPairingWaitsForRemoteTarget(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == 1 {
			fmt.Fprint(w, `{"pairingTarget":{"pending":true,"error":"login required"}}`)
			return
		}
		fmt.Fprint(w, `{"pairingTarget":{"reachable":true}}`)
	}))
	defer server.Close()
	client := adminclient.New(config.Config{AdminListenAddr: strings.TrimPrefix(server.URL, "http://")})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := waitForPairingTarget(ctx, client); err != nil {
		t.Fatal(err)
	}
}

func TestPairingReportsUnavailableManualTarget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"pairingTarget":{"error":"invalid URL"}}`)
	}))
	defer server.Close()
	client := adminclient.New(config.Config{AdminListenAddr: strings.TrimPrefix(server.URL, "http://")})
	if err := waitForPairingTarget(context.Background(), client); err == nil || !strings.Contains(err.Error(), "invalid URL") {
		t.Fatalf("error = %v, want invalid URL", err)
	}
}
