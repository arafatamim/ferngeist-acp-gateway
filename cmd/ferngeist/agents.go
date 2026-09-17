package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/adminclient"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/config"
)

// agentCommand renders the launch command with its arguments appended, so one
// column shows everything the daemon will exec.
func agentCommand(agent adminclient.Agent) string {
	if len(agent.Launch.Args) == 0 {
		return agent.Launch.Command
	}
	return agent.Launch.Command + " " + strings.Join(agent.Launch.Args, " ")
}

func runAgentsList() error {
	client := adminclient.New(config.Load())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	agents, err := client.ListAgents(ctx)
	if err != nil {
		return fmt.Errorf("list agents: %w", err)
	}
	if len(agents) == 0 {
		fmt.Println("No agents.")
		return nil
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\tNAME\tCOMMAND\tDETECTED\tSOURCE")
	for _, agent := range agents {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%t\t%s\n", agent.ID, agent.DisplayName, agentCommand(agent), agent.Detected, agent.Source)
	}
	return writer.Flush()
}

func runAgentsAdd(name, command string, args []string, hint string) error {
	client := adminclient.New(config.Load())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	agent, err := client.AddCustomAgent(ctx, adminclient.AgentInput{DisplayName: name, Command: command, Args: args, Hint: hint})
	if err != nil {
		return fmt.Errorf("add agent: %w", err)
	}
	fmt.Printf("Added custom agent: %s (%s)\n", agent.DisplayName, agent.ID)
	if !agent.Detected {
		fmt.Printf("Warning: %q is not detected on this host (command not on PATH or absolute path missing).\n", agent.Launch.Command)
	}
	return nil
}

func runAgentsUpdate(id string, name, command *string, args []string, argsSet, clearArgs bool, hint *string) error {
	client := adminclient.New(config.Load())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	input := adminclient.AgentInput{}
	if name != nil {
		input.DisplayName = *name
	}
	if command != nil {
		input.Command = *command
	}
	switch {
	case clearArgs:
		// The admin API clears args on an empty (non-nil) list and leaves them
		// alone on an omitted one.
		input.Args = []string{}
	case argsSet:
		input.Args = args
	}
	if hint != nil {
		input.Hint = *hint
	} else {
		// The admin API always overwrites hint, so an update that omits --hint
		// would silently clear it. Carry the stored hint forward instead.
		agents, err := client.ListAgents(ctx)
		if err != nil {
			return fmt.Errorf("update agent: %w", err)
		}
		for _, agent := range agents {
			if agent.ID == id {
				input.Hint = agent.Hint
				break
			}
		}
	}

	agent, err := client.UpdateCustomAgent(ctx, id, input)
	if err != nil {
		return fmt.Errorf("update agent: %w", err)
	}
	fmt.Printf("Updated custom agent: %s\n", agent.ID)
	return nil
}

func runAgentsRemove(id string) error {
	client := adminclient.New(config.Load())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := client.RemoveCustomAgent(ctx, id); err != nil {
		return fmt.Errorf("remove agent: %w", err)
	}
	fmt.Printf("Removed custom agent: %s\n", id)
	return nil
}
