package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// claudeHeadBytes is how much of a transcript Claude Code's /resume picker
// reads to find the chat's entrypoint.
const claudeHeadBytes = 64 << 10

// claudeEntrypointKey is the compact form Claude Code writes and the picker
// looks for first.
var claudeEntrypointKey = []byte(`"entrypoint":"`)

// claudeSDKEntrypoints are the labels the picker hides.
var claudeSDKEntrypoints = []string{"sdk-cli", "sdk-ts", "sdk-py"}

// claudeAt configures the Claude Agent adapter. Claude Code's /resume picker hides a chat
// whose transcript's first entrypoint is an Agent SDK one, and headless Claude
// Code always records one (it even turns CLAUDE_CODE_ENTRYPOINT=cli into
// sdk-cli). After each turn the profile relabels the chat as a CLI one so the
// native picker lists it.
// ponytail: drop this if Claude Code lists SDK chats or gains a setting for it.
func claudeAt(dir string) Profile {
	if dir == "" {
		return Profile{}
	}
	transcripts := claudeTranscripts{projects: filepath.Join(dir, "projects")}
	return Profile{TurnFinished: transcripts.unhide}
}

type claudeTranscripts struct {
	projects string
}

// unhide relabels sessionID's transcript, in whichever project folder Claude
// Code filed it. A chat with no transcript yet is left alone.
func (c claudeTranscripts) unhide(ctx context.Context, sessionID string) error {
	if c.projects == "" {
		return nil
	}
	// The id comes from the agent; it must name a file, not a path.
	if sessionID == "" || sessionID != filepath.Base(sessionID) || strings.ContainsAny(sessionID, `/\`) {
		return fmt.Errorf("claude transcript: invalid session id %q", sessionID)
	}
	entries, err := os.ReadDir(c.projects)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if entry.IsDir() {
			errs = append(errs, relabelSDKEntrypoint(filepath.Join(c.projects, entry.Name(), sessionID+".jsonl")))
		}
	}
	return errors.Join(errs...)
}

// relabelSDKEntrypoint changes the transcript's first entrypoint from an SDK
// label to "cli", padded with spaces after the closing quote to the same byte
// length. Claude Code only appends to a live transcript, so overwriting those
// bytes in place cannot lose or interleave with its writes, and JSON allows the
// padding. Anything else is left untouched, so it is safe to repeat.
func relabelSDKEntrypoint(path string) (err error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()

	head := make([]byte, claudeHeadBytes)
	n, err := f.ReadAt(head, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	head = head[:n]
	key := bytes.Index(head, claudeEntrypointKey)
	if key < 0 {
		return nil
	}
	start := key + len(claudeEntrypointKey)
	end := bytes.IndexByte(head[start:], '"')
	if end < 0 || !slices.Contains(claudeSDKEntrypoints, string(head[start:start+end])) {
		return nil
	}
	label := `cli"` + strings.Repeat(" ", end-len("cli"))
	_, err = f.WriteAt([]byte(label), int64(start))
	return err
}
