// Package customagents owns the write policy for client-registered agents:
// bounds, id allocation, storage and catalog read-back. Transports (the public
// API and the admin API) only decode requests and map Error to a status.
package customagents

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

// MaxAgents caps client-registered agents.
// ponytail: fixed cap, raise it if real users hit the ceiling.
const MaxAgents = 50

// MaxNameLen is the display name bound in characters. Exported because the API
// documents the same limit and its tests assert it.
const MaxNameLen = 80

// Bounds on client-supplied custom agent fields. Content rules (absolute-or-bare
// command, no control characters, ...) come from catalog validation: the service
// saves and reads the record back through the catalog instead of duplicating it.
const (
	maxSlugTries = 100
	maxCmdLen    = 1024
	maxArgs      = 20
	maxArgLen    = 1024
)

// Create is the client-supplied shape of a new custom agent. Everything else
// (id, protocol, launch policy) is derived server-side.
type Create struct {
	DisplayName string
	Command     string
	Args        []string
	Hint        string
}

// Update is a partial edit of an existing custom agent: nil pointers and an
// unset Args are unchanged, an empty Args slice clears the arguments.
type Update struct {
	DisplayName *string
	Command     *string
	Args        []string
	ArgsSet     bool
	Hint        *string
}

// Error carries the status a transport must return for a rejected operation.
// Errors that are not Error are infrastructure failures the transport maps to
// 500.
type Error struct {
	status  int
	message string
}

func (e Error) Error() string { return e.message }

// StatusCode is the HTTP status the transport must answer with.
func (e Error) StatusCode() int { return e.status }

// Service is the shared write path for custom agents.
type Service struct {
	store        *storage.SQLiteStore
	catalog      *catalog.Service
	listRuntimes func() []runtime.Runtime
	logger       *slog.Logger
	now          func() time.Time
}

func New(store *storage.SQLiteStore, catalogSvc *catalog.Service, listRuntimes func() []runtime.Runtime, logger *slog.Logger) *Service {
	return &Service{
		store:        store,
		catalog:      catalogSvc,
		listRuntimes: listRuntimes,
		logger:       logger,
		now:          func() time.Time { return time.Now().UTC() },
	}
}

// List returns the catalog, custom agents included.
func (s *Service) List() []catalog.Agent { return s.catalog.List() }

// Create allocates an id, persists the record and reads it back through the
// catalog for validation.
func (s *Service) Create(ctx context.Context, in Create) (catalog.Agent, error) {
	if err := s.ready(); err != nil {
		return catalog.Agent{}, err
	}
	displayName := strings.TrimSpace(in.DisplayName)
	command := strings.TrimSpace(in.Command)
	if err := validateCustomAgentFields(displayName, command, in.Args); err != nil {
		return catalog.Agent{}, err
	}
	count, err := s.store.CountCustomAgents(ctx)
	if err != nil {
		return catalog.Agent{}, err
	}
	if count >= MaxAgents {
		return catalog.Agent{}, Error{
			status:  http.StatusConflict,
			message: fmt.Sprintf("custom agent limit reached (%d)", MaxAgents),
		}
	}
	id, err := s.allocateCustomAgentID(ctx, displayName)
	if err != nil {
		return catalog.Agent{}, err
	}
	record := storage.CustomAgentRecord{
		ID:          id,
		DisplayName: displayName,
		Command:     command,
		Args:        in.Args,
		Hint:        in.Hint,
		CreatedAt:   s.now(),
	}
	// ponytail: id allocation and the cap check are not transactional, so two
	// concurrent creates of the same display name can race to one id (last write
	// wins). A single-user gateway never hits this; add a store-level lock if it
	// ever serves multiple writers.
	if err := s.store.SaveCustomAgent(ctx, record); err != nil {
		return catalog.Agent{}, err
	}
	agent, err := s.customAgentFromCatalog(ctx, id)
	if err != nil {
		// A rejected agent must never be served: drop the row again.
		if deleteErr := s.store.DeleteCustomAgent(ctx, id); deleteErr != nil && !errors.Is(deleteErr, storage.ErrNotFound) {
			s.logger.Warn("failed to roll back invalid custom agent", "agentId", id, "error", deleteErr)
		}
		return catalog.Agent{}, err
	}
	return agent, nil
}

// Update applies a partial edit in place. The id is immutable: it was derived
// from the original display name and clients key stored sessions/runtimes by it.
func (s *Service) Update(ctx context.Context, id string, in Update) (catalog.Agent, error) {
	if err := s.ready(); err != nil {
		return catalog.Agent{}, err
	}
	existing, err := s.store.GetCustomAgent(ctx, id)
	if err != nil {
		return catalog.Agent{}, err
	}
	updated := existing
	if in.DisplayName != nil {
		updated.DisplayName = *in.DisplayName
	}
	if in.Command != nil {
		updated.Command = *in.Command
	}
	if in.ArgsSet {
		updated.Args = in.Args
	}
	if in.Hint != nil {
		updated.Hint = *in.Hint
	}

	if err := validateCustomAgentFields(updated.DisplayName, updated.Command, updated.Args); err != nil {
		return catalog.Agent{}, err
	}
	if err := s.store.SaveCustomAgent(ctx, updated); err != nil {
		return catalog.Agent{}, err
	}
	agent, err := s.customAgentFromCatalog(ctx, id)
	if err != nil {
		// Roll back to the last accepted record so an invalid edit never sticks.
		if rollbackErr := s.store.SaveCustomAgent(ctx, existing); rollbackErr != nil {
			s.logger.Warn("failed to roll back custom agent update", "agentId", id, "error", rollbackErr)
		}
		return catalog.Agent{}, err
	}
	return agent, nil
}

// Delete removes a custom agent. Deleting one with a live gateway-tracked
// runtime would strand the process, so that is a 409; recently stopped runtimes
// are ignored (the supervisor keeps them listed for 10 minutes after they exit).
func (s *Service) Delete(ctx context.Context, id string) error {
	if err := s.ready(); err != nil {
		return err
	}
	if !s.catalog.IsCustomID(id) {
		return Error{status: http.StatusNotFound, message: "unknown custom agent"}
	}
	for _, runtimeInfo := range s.listRuntimes() {
		if runtimeInfo.AgentID != id {
			continue
		}
		if runtimeInfo.Status != runtime.StatusStopped && runtimeInfo.Status != runtime.StatusFailed {
			return Error{status: http.StatusConflict, message: "agent has running runtimes"}
		}
	}
	return s.store.DeleteCustomAgent(ctx, id)
}

// ready guards the nil store of test servers and admin-only builds, mirroring
// the session service guard.
func (s *Service) ready() error {
	if s.store == nil {
		return Error{status: http.StatusServiceUnavailable, message: "custom agent storage not available"}
	}
	return nil
}

// allocateCustomAgentID derives the immutable custom-<slug> id and suffixes it
// until it collides with neither a stored custom nor a catalog agent, so a new
// custom never shadows a registry or embedded entry.
func (s *Service) allocateCustomAgentID(ctx context.Context, displayName string) (string, error) {
	base := catalog.SlugCustomID(displayName)
	for attempt := 1; attempt <= maxSlugTries; attempt++ {
		id := base
		if attempt > 1 {
			id = fmt.Sprintf("%s-%d", base, attempt)
		}
		_, err := s.store.GetCustomAgent(ctx, id)
		if err == nil {
			continue
		}
		if !errors.Is(err, storage.ErrNotFound) {
			return "", err
		}
		if _, err := s.catalog.Get(id); err == nil {
			continue
		}
		return id, nil
	}
	return "", Error{status: http.StatusConflict, message: "no free custom agent id"}
}

// customAgentFromCatalog reads the stored record back through the catalog,
// which is the only validator custom agents have (customToAgent + validateAgent).
func (s *Service) customAgentFromCatalog(ctx context.Context, id string) (catalog.Agent, error) {
	agent, err := s.catalog.Get(id)
	if err != nil {
		return catalog.Agent{}, err
	}
	if !agent.ManifestValid {
		message := strings.TrimSpace(agent.ValidationError)
		if message == "" {
			message = "invalid custom agent"
		}
		return catalog.Agent{}, Error{status: http.StatusBadRequest, message: message}
	}
	return agent, nil
}

// validateCustomAgentFields enforces the byte-independent bounds the API
// documents: lengths are counted in runes, so a multi-byte display name is not
// rejected below the advertised limit.
func validateCustomAgentFields(displayName, command string, args []string) error {
	switch {
	case displayName == "":
		return Error{status: http.StatusBadRequest, message: "displayName is required"}
	case utf8.RuneCountInString(displayName) > MaxNameLen:
		return Error{status: http.StatusBadRequest, message: fmt.Sprintf("displayName must be at most %d characters", MaxNameLen)}
	case command == "":
		return Error{status: http.StatusBadRequest, message: "command is required"}
	case utf8.RuneCountInString(command) > maxCmdLen:
		return Error{status: http.StatusBadRequest, message: fmt.Sprintf("command must be at most %d characters", maxCmdLen)}
	case len(args) > maxArgs:
		return Error{status: http.StatusBadRequest, message: fmt.Sprintf("args must have at most %d entries", maxArgs)}
	}
	for _, arg := range args {
		if utf8.RuneCountInString(arg) > maxArgLen {
			return Error{status: http.StatusBadRequest, message: fmt.Sprintf("each arg must be at most %d characters", maxArgLen)}
		}
	}
	return nil
}
