package acquire

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

func TestEnsureDownloadsRawBinaryIntoManagedDir(t *testing.T) {
	sourceDir := t.TempDir()
	sourcePath := filepath.Join(sourceDir, namedBinary("mock-stdio-agent"))
	buildMockStdioAgent(t, sourcePath)

	server := httptest.NewServer(http.FileServer(http.Dir(sourceDir)))
	defer server.Close()

	managedDir := t.TempDir()
	store, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	defer store.Close()

	installer := New(slog.New(slog.NewTextHandler(os.Stdout, nil)), managedDir, store)

	agent, changed, err := installer.Ensure(context.Background(), catalog.Agent{
		ID: "codex-acp",
		Launch: catalog.LaunchConfig{
			Mode:    "external",
			Command: "codex-acp",
		},
		Registry: catalog.RegistryInfo{
			CurrentBinaryPath:       namedBinary("mock-stdio-agent"),
			CurrentBinaryArchiveURL: server.URL + "/" + namedBinary("mock-stdio-agent"),
		},
	})
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if !changed {
		t.Fatal("Ensure() should report changed=true on first install")
	}
	if !agent.Detected {
		t.Fatal("agent should be marked detected after install")
	}
	if _, err := os.Stat(agent.Launch.Command); err != nil {
		t.Fatalf("installed binary missing at %q: %v", agent.Launch.Command, err)
	}

	record, err := store.GetAcquiredBinary(context.Background(), "codex-acp")
	if err != nil {
		t.Fatalf("GetAcquiredBinary() error = %v", err)
	}
	if record.Path != agent.Launch.Command {
		t.Fatalf("record.Path = %q, want %q", record.Path, agent.Launch.Command)
	}
}

func buildMockStdioAgent(t *testing.T, outputPath string) {
	t.Helper()

	command := exec.Command("go", "build", "-o", outputPath, "./cmd/mock-stdio-agent")
	command.Dir = filepath.Join("..", "..")
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		t.Fatalf("go build mock stdio agent error = %v", err)
	}
}

func namedBinary(name string) string {
	if os.PathSeparator == '\\' {
		return name + ".exe"
	}
	return name
}

func TestEnsureExtractsArchives(t *testing.T) {
	payload := []byte("#!/bin/sh\necho agent\n")
	var tarGz bytes.Buffer
	gz := gzip.NewWriter(&tarGz)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "dist/agent", Mode: 0o755, Size: int64(len(payload))})
	_, _ = tw.Write(payload)
	_ = tw.WriteHeader(&tar.Header{Name: "dist/lib/data", Mode: 0o644, Size: 4})
	_, _ = tw.Write([]byte("data"))
	if os.PathSeparator != '\\' {
		_ = tw.WriteHeader(&tar.Header{Name: "dist/link", Typeflag: tar.TypeSymlink, Linkname: "lib/data"})
	}
	_ = tw.Close()
	_ = gz.Close()
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	entry, _ := zw.Create("dist/agent")
	_, _ = entry.Write(payload)
	entry, _ = zw.Create("dist/lib/data")
	_, _ = entry.Write([]byte("data"))
	if os.PathSeparator != '\\' {
		linkHeader := &zip.FileHeader{Name: "dist/link"}
		linkHeader.SetMode(os.ModeSymlink | 0o777)
		entry, _ = zw.CreateHeader(linkHeader)
		_, _ = entry.Write([]byte("lib/data"))
	}
	_ = zw.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agent.tar.gz":
			_, _ = w.Write(tarGz.Bytes())
		case "/agent.zip":
			_, _ = w.Write(zipBuf.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	for _, name := range []string{"agent.tar.gz", "agent.zip?sig=abc"} {
		t.Run(name, func(t *testing.T) {
			installer := New(slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(), nil)
			agent, _, err := installer.Ensure(context.Background(), catalog.Agent{
				ID:     "archived",
				Launch: catalog.LaunchConfig{Mode: "external"},
				Registry: catalog.RegistryInfo{
					CurrentBinaryPath:       "dist/agent",
					CurrentBinaryArchiveURL: server.URL + "/" + name,
				},
			})
			if err != nil {
				t.Fatalf("Ensure() error = %v", err)
			}
			got, err := os.ReadFile(agent.Launch.Command)
			if err != nil {
				t.Fatalf("read installed binary: %v", err)
			}
			if data, err := os.ReadFile(filepath.Join(filepath.Dir(agent.Launch.Command), "lib", "data")); err != nil || string(data) != "data" {
				t.Fatalf("sibling data = %q, error = %v", data, err)
			}
			if os.PathSeparator != '\\' {
				link, err := os.Readlink(filepath.Join(filepath.Dir(agent.Launch.Command), "link"))
				if err != nil || link != "lib/data" {
					t.Fatalf("symlink = %q, error = %v", link, err)
				}
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("installed %d bytes of archive data, want extracted entry", len(got))
			}
		})
	}
}

func TestEnsureUpdatesOnRegistryVersionChange(t *testing.T) {
	body, status := "v1", http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()

	store, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	defer store.Close()
	installer := New(slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(), store)
	ensure := func(version string) (string, bool) {
		t.Helper()
		agent, changed, err := installer.Ensure(context.Background(), catalog.Agent{
			ID:     "versioned",
			Launch: catalog.LaunchConfig{Mode: "external"},
			Registry: catalog.RegistryInfo{
				Version:                 version,
				CurrentBinaryPath:       "agent",
				CurrentBinaryArchiveURL: server.URL + "/agent-" + version,
			},
		})
		if err != nil {
			t.Fatalf("Ensure(%s) error = %v", version, err)
		}
		got, err := os.ReadFile(agent.Launch.Command)
		if err != nil {
			t.Fatalf("read installed binary: %v", err)
		}
		return string(got), changed
	}

	if got, changed := ensure("1.0.0"); got != "v1" || !changed {
		t.Fatalf("first install = %q changed=%v, want v1 installed", got, changed)
	}
	body = "v2"
	if got, changed := ensure("1.0.0"); got != "v1" || changed {
		t.Fatalf("same version = %q changed=%v, want cached v1", got, changed)
	}
	if got, changed := ensure("2.0.0"); got != "v2" || !changed {
		t.Fatalf("new version = %q changed=%v, want v2 downloaded", got, changed)
	}
	status = http.StatusInternalServerError
	if got, changed := ensure("3.0.0"); got != "v2" || changed {
		t.Fatalf("failed update = %q changed=%v, want installed v2 kept", got, changed)
	}
}

func TestEnsureRejectsUnsafeOrIncompleteArchives(t *testing.T) {
	for _, tc := range []struct {
		name, entry, link string
		truncated         bool
	}{
		{name: "traversal", entry: "../outside"},
		{name: "symlink escape", entry: "escape", link: "../../outside"},
		{name: "truncated", entry: "sibling", truncated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gz)
			if err := tw.WriteHeader(&tar.Header{Name: "agent", Mode: 0o755, Size: 1}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			header := &tar.Header{Name: tc.entry, Mode: 0o644, Size: 1}
			if tc.link != "" {
				header.Typeflag = tar.TypeSymlink
				header.Linkname = tc.link
				header.Size = 0
			}
			if err := tw.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if header.Size > 0 {
				if _, err := tw.Write([]byte("x")); err != nil {
					t.Fatal(err)
				}
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			data := buf.Bytes()
			if tc.truncated {
				data = data[:len(data)-5]
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) }))
			defer server.Close()
			managed := t.TempDir()
			installer := New(slog.New(slog.NewTextHandler(io.Discard, nil)), managed, nil)
			agent := catalog.Agent{ID: "agent", Launch: catalog.LaunchConfig{Mode: "external"}, Registry: catalog.RegistryInfo{CurrentBinaryPath: "agent", CurrentBinaryArchiveURL: server.URL + "/agent.tar.gz"}}
			if _, _, err := installer.Ensure(context.Background(), agent); err == nil {
				t.Fatal("Ensure succeeded for unsafe or incomplete archive")
			}
			entries, err := os.ReadDir(filepath.Join(managed, "agent"))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("failed extraction left install entries: %v", entries)
			}
		})
	}
}
