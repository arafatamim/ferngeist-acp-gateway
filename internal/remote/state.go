package remote

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

// embeddedStateDir gives each database its own node identity. Installed gateways
// used a sibling tsnet directory; migrate only when its database owner is unique.
func embeddedStateDir(ctx context.Context, dbPath string) (string, error) {
	dbPath, err := filepath.Abs(dbPath)
	if err != nil {
		return "", fmt.Errorf("resolve gateway database: %w", err)
	}
	dir := dbPath + ".tsnet"
	if _, err := os.Stat(dir); err == nil {
		return dir, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("stat embedded state: %w", err)
	}
	legacy := filepath.Join(filepath.Dir(dbPath), "tsnet")
	if _, err := os.Stat(legacy); errors.Is(err, os.ErrNotExist) {
		return dir, nil
	} else if err != nil {
		return "", fmt.Errorf("stat legacy embedded state: %w", err)
	}
	owners, err := storage.FindGatewayDatabases(ctx, filepath.Dir(dbPath))
	if err != nil {
		return "", fmt.Errorf("identify legacy embedded state owner: %w", err)
	}
	if len(owners) != 1 || owners[0] != dbPath {
		return "", fmt.Errorf("legacy Tailscale state at %s has multiple possible database owners; stop its gateway and move it to <owning-database-path>.tsnet before provisioning (this database uses %s)", legacy, dir)
	}
	if err := os.Rename(legacy, dir); err != nil {
		return "", fmt.Errorf("migrate embedded state: %w", err)
	}
	return dir, nil
}
