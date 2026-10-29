package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// FindGatewayDatabases finds sibling gateway databases without applying
// migrations. Legacy embedded-node state predates a database ownership marker.
func FindGatewayDatabases(ctx context.Context, dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list database directory: %w", err)
	}
	var paths []string
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		gateway, err := isGatewayDatabase(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", path, err)
		}
		if gateway {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

func isGatewayDatabase(ctx context.Context, path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	var header [16]byte
	_, err = io.ReadFull(file, header[:])
	_ = file.Close() // Read-only inspection has no buffered writes to flush.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if string(header[:]) != "SQLite format 3\x00" {
		return false, nil
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "mode=ro"}
	if filepath.VolumeName(path) != "" && !strings.HasPrefix(uri.Path, "/") {
		uri.Path = "/" + uri.Path
	}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return false, err
	}
	defer func() { _ = db.Close() }() // The inspection connection only reads schema metadata.
	// These tables also identify gateways predating stable gateway IDs.
	var tables int
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name IN ('gateway_settings', 'paired_devices')`).Scan(&tables)
	if err != nil {
		return false, err
	}
	return tables == 2, nil
}
