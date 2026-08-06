package api

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveWithinRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveWithinRoot(root, "sub/hello.txt")
	if err != nil {
		t.Fatalf("resolve valid: %v", err)
	}
	if !strings.HasPrefix(got, root) {
		t.Fatalf("result %q not under root %q", got, root)
	}

	if _, err := resolveWithinRoot(root, filepath.Join(root, "..")); err == nil {
		t.Fatal("absolute path should be rejected")
	}
	if _, err := resolveWithinRoot(root, "../outside"); err == nil {
		t.Fatal("traversal should be rejected")
	}
	if _, err := resolveWithinRoot(root, "sub/../../outside"); err == nil {
		t.Fatal("deep traversal should be rejected")
	}
	if _, err := resolveWithinRoot(root, ""); err == nil {
		t.Fatal("empty path should be rejected")
	}
}

func TestResolveWithinRootRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	// The target file must exist so EvalSymlinks resolves the escape; a dangling
	// symlink leaves the un-resolved (in-root) path and is handled by a 404.
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveWithinRoot(root, "escape/secret.txt"); err == nil {
		t.Fatal("symlink escape should be rejected")
	}
}

func TestParseGitStatus(t *testing.T) {
	out := "# branch.oid abc123\n# branch.head main\n# branch.upstream origin/main\n# branch.ab +2 -0\n" +
		"1 .M N... 100644 100644 100644 f00 000 file_a.go\n" +
		"1 M. N... 100644 100644 100644 f00 000 my file with spaces.go\n" +
		"2 R. N... 100644 100644 100644 f00 000 R100 README.md\tdocs/x.md\n" +
		"? my untracked file.txt\n"
	files := parseGitStatus(out)
	if len(files) != 4 {
		t.Fatalf("parseGitStatus returned %d files, want 4: %+v", len(files), files)
	}
	if files[0].Path != "file_a.go" || files[0].Status != "M" {
		t.Fatalf("files[0] = %+v, want {file_a.go M}", files[0])
	}
	if files[1].Path != "my file with spaces.go" || files[1].Status != "M" {
		t.Fatalf("files[1] = %+v, want {my file with spaces.go M}", files[1])
	}
	// Rename/copy: the destination path precedes the tab; the source follows.
	if files[2].Path != "README.md" || files[2].Status != "R" {
		t.Fatalf("files[2] = %+v, want {README.md R}", files[2])
	}
	if files[3].Path != "my untracked file.txt" || files[3].Status != "?" {
		t.Fatalf("files[3] = %+v, want {my untracked file.txt ?}", files[3])
	}
}

func TestRunGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	if _, err := runGit(context.Background(), dir, "status", "--porcelain=v2", "--branch"); err == nil {
		t.Fatal("git status in non-repo should error")
	}
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("config", "commit.gpgSign", "false")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-qm", "init")
	out, err := runGit(context.Background(), dir, "status", "--porcelain=v2", "--branch")
	if err != nil {
		t.Fatalf("runGit in repo: %v", err)
	}
	if !strings.Contains(out, "branch.head ") {
		t.Fatalf("expected branch.head in output, got %q", out)
	}
}

func TestParseGitNumstat(t *testing.T) {
	out := "3\t1\tREADME.md\n-\t-\tlogo.png\n10\t2\tarch/{i386 => x86}/Makefile\n"
	stats := parseGitNumstat(out)

	if s := stats["README.md"]; s.Added != 3 || s.Removed != 1 || s.Binary {
		t.Fatalf("README.md = %+v, want {added:3 removed:1}", s)
	}
	if s := stats["logo.png"]; !s.Binary || s.Added != 0 || s.Removed != 0 {
		t.Fatalf("logo.png = %+v, want binary with zero counts", s)
	}
	// Rename/copy: git renders the path with inline braces, and the parser keys
	// by that single path field (the same path status --porcelain=v2 reports).
	if s := stats["arch/{i386 => x86}/Makefile"]; s.Added != 10 || s.Removed != 2 {
		t.Fatalf("rename = %+v, want {added:10 removed:2}", s)
	}
}

func TestCountFileLines(t *testing.T) {
	dir := t.TempDir()

	textPath := filepath.Join(dir, "text.txt")
	if err := os.WriteFile(textPath, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, binary := countFileLines(textPath)
	if lines != 3 || binary {
		t.Fatalf("text.txt = (%d, %v), want (3, false)", lines, binary)
	}

	// No trailing newline still counts the line.
	noNL := filepath.Join(dir, "nonl.txt")
	if err := os.WriteFile(noNL, []byte("solo"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, binary = countFileLines(noNL)
	if lines != 1 || binary {
		t.Fatalf("nonl.txt = (%d, %v), want (1, false)", lines, binary)
	}

	binaryPath := filepath.Join(dir, "bin.dat")
	if err := os.WriteFile(binaryPath, []byte{0x00, 0x01, 0x02, 0x0a}, 0o644); err != nil {
		t.Fatal(err)
	}
	lines, binary = countFileLines(binaryPath)
	if !binary {
		t.Fatalf("bin.dat should be flagged binary, got lines=%d binary=%v", lines, binary)
	}

	if lines, binary := countFileLines(filepath.Join(dir, "missing.txt")); lines != 0 || binary {
		t.Fatalf("missing file = (%d, %v), want (0, false)", lines, binary)
	}
}

func TestFileHelpers(t *testing.T) {
	// fileURI produces a file:// URI with forward slashes.
	uri := fileURI(`C:\proj\a b.txt`)
	if !strings.HasPrefix(uri, "file://") || !strings.Contains(uri, "a%20b.txt") {
		t.Fatalf("fileURI(%q) = %q, want file:// with encoded space", `C:\proj\a b.txt`, uri)
	}

	// mimeTypeFor maps common extensions; unknown returns "".
	if got := mimeTypeFor("x.json"); got != "application/json" {
		t.Fatalf("mimeTypeFor(x.json) = %q, want application/json", got)
	}
	if got := mimeTypeFor("x.unknown_ext"); got != "" {
		t.Fatalf("mimeTypeFor(x.unknown_ext) = %q, want empty", got)
	}

	// isBinary detects a NUL byte.
	if !isBinary([]byte{0x00, 0x01}) {
		t.Fatal("isBinary should flag NUL byte")
	}
	if isBinary([]byte("hello\n")) {
		t.Fatal("isBinary should not flag plain text")
	}
}

func TestTruncateStringPtr(t *testing.T) {
	if p := truncateStringPtr("", 10); p != nil {
		t.Fatalf("empty string: got %v, want nil", *p)
	}
	if p := truncateStringPtr("abc", 10); p == nil || *p != "abc" {
		t.Fatalf("short string: got %v, want abc", p)
	}
	if p := truncateStringPtr("abcdef", 3); p == nil || *p != "abc" {
		t.Fatalf("long string: got %v, want abc", p)
	}
}

func TestGitDiffEntry(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("config", "commit.gpgSign", "false")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-qm", "init")

	// Untracked file: no HEAD entry -> OldText nil, NewText is content.
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("fresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	entry, err := s.gitDiffEntry(context.Background(), dir, "new.txt", filepath.Join(dir, "new.txt"))
	if err != nil {
		t.Fatalf("gitDiffEntry(untracked): %v", err)
	}
	if entry.Type != "diff" || entry.NewText != "fresh\n" || entry.OldText != nil {
		t.Fatalf("untracked entry = %+v, want {type:diff newText:fresh oldText:nil}", entry)
	}

	// Modified tracked file: OldText from HEAD, NewText from disk.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry, err = s.gitDiffEntry(context.Background(), dir, "a.txt", filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatalf("gitDiffEntry(modified): %v", err)
	}
	if entry.Type != "diff" || entry.NewText != "new\n" {
		t.Fatalf("modified entry = %+v, want type:diff newText:new", entry)
	}
	if entry.OldText == nil || *entry.OldText != "old\n" {
		t.Fatalf("modified OldText = %v, want pointer to old", entry.OldText)
	}

	// Deleted tracked file: working copy gone, present in HEAD -> OldText set,
	// NewText empty.
	if err := os.Remove(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	entry, err = s.gitDiffEntry(context.Background(), dir, "a.txt", filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatalf("gitDiffEntry(deleted): %v", err)
	}
	if entry.Type != "diff" || entry.NewText != "" {
		t.Fatalf("deleted entry = %+v, want type:diff newText:empty", entry)
	}
	if entry.OldText == nil || *entry.OldText != "old\n" {
		t.Fatalf("deleted OldText = %v, want pointer to old", entry.OldText)
	}
}
