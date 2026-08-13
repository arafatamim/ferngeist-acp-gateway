package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/adminclient"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/config"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/service"
)

func runDaemonInstall(options service.InstallOptions) error {
	manager := service.NewManager()
	if err := manager.Install(options); err != nil {
		if errors.Is(err, service.ErrServicePermissionDenied) {
			return fmt.Errorf("install daemon service: %w\nHint: rerun with elevated privileges, for example: sudo ferngeist-gateway daemon install", err)
		}
		if errors.Is(err, service.ErrInvalidInstallOptions) {
			return fmt.Errorf("install daemon service: %w\nHint: use --host, --port, and optional --public-url", err)
		}
		return fmt.Errorf("install daemon service: %w", err)
	}
	fmt.Println("Daemon service installed and started.")
	if options.TailscaleMode != "off" {
		printRemoteSetupAfterInstall(context.Background())
	}
	return nil
}

// printRemoteSetupAfterInstall waits briefly for the freshly installed daemon
// to report its remote-access state and prints the actionable next step: the
// login link when Tailscale auth is pending, the public URL when ready, or a
// pointer to `daemon status` while provisioning is still in flight.
func printRemoteSetupAfterInstall(ctx context.Context) {
	status, err := waitForRemoteSetup(ctx, fetchDaemonStatus, 30*time.Second)
	if err != nil {
		fmt.Println("\nRemote access is provisioning in the background.")
		fmt.Println("Check progress with: ferngeist-gateway daemon status")
		return
	}
	switch {
	case status.Remote.AuthRequired:
		fmt.Println("\nTailscale login required — open this link once to finish setup:")
		fmt.Printf("  %s\n", status.Remote.AuthURL)
		fmt.Println("\nIt is also shown anytime by: ferngeist-gateway daemon status")
	case status.Remote.PublicURL != "":
		fmt.Printf("\nRemote access ready: %s\n", status.Remote.PublicURL)
	default:
		fmt.Println("\nRemote access is provisioning in the background.")
		fmt.Println("Check progress with: ferngeist-gateway daemon status")
	}
}

// waitForRemoteSetup polls daemon status until remote access is ready or
// blocked on interactive login, or the budget expires. Returns the last
// observed status (zero value if the daemon never answered).
func waitForRemoteSetup(ctx context.Context, fetch func(context.Context) (adminclient.DaemonStatus, error), budget time.Duration) (adminclient.DaemonStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var last adminclient.DaemonStatus
	for {
		st, err := fetch(ctx)
		if err == nil {
			last = st
			if st.Remote.PublicURL != "" || st.Remote.AuthRequired {
				return st, nil
			}
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func runDaemonUninstall(purge bool) error {
	manager := service.NewManager()
	if err := manager.Uninstall(purge); err != nil {
		return fmt.Errorf("uninstall daemon service: %w", err)
	}
	if purge {
		fmt.Println("Daemon service uninstalled and data purged.")
	} else {
		fmt.Println("Daemon service uninstalled.")
	}
	return nil
}

func runDaemonStart() error {
	manager := service.NewManager()
	if err := manager.Start(); err != nil {
		return fmt.Errorf("start daemon service: %w", err)
	}
	fmt.Println("Daemon service started.")
	return nil
}

func runDaemonStop() error {
	manager := service.NewManager()
	if err := manager.Stop(); err != nil {
		return fmt.Errorf("stop daemon service: %w", err)
	}
	fmt.Println("Daemon service stopped.")
	return nil
}

func runDaemonRestart() error {
	manager := service.NewManager()
	if err := manager.Restart(); err != nil {
		return fmt.Errorf("restart daemon service: %w", err)
	}
	fmt.Println("Daemon service restarted.")
	return nil
}

func runDaemonStatus() error {
	manager := service.NewManager()
	serviceStatus, err := manager.Status()
	if err != nil {
		return fmt.Errorf("read daemon service status: %w", err)
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "SERVICE")
	fmt.Fprintf(writer, "INSTALLED\t%t\n", serviceStatus.Installed)
	if serviceStatus.UnitPath != "" {
		fmt.Fprintf(writer, "UNIT PATH\t%s\n", serviceStatus.UnitPath)
	}
	if serviceStatus.LoadState != "" {
		fmt.Fprintf(writer, "LOAD STATE\t%s\n", serviceStatus.LoadState)
	}
	if serviceStatus.ActiveState != "" {
		fmt.Fprintf(writer, "ACTIVE STATE\t%s\n", serviceStatus.ActiveState)
	}
	if serviceStatus.SubState != "" {
		fmt.Fprintf(writer, "SUB STATE\t%s\n", serviceStatus.SubState)
	}
	if serviceStatus.UnitFileState != "" {
		fmt.Fprintf(writer, "DETAIL\t%s\n", serviceStatus.UnitFileState)
	}

	daemonStatus, err := fetchDaemonStatus(context.Background())
	if err != nil {
		fmt.Fprintf(writer, "DAEMON API\tunreachable (%s)\n", err)
		return writer.Flush()
	}

	fmt.Fprintln(writer, "")
	fmt.Fprintln(writer, "DAEMON")
	fmt.Fprintf(writer, "NAME\t%s\n", daemonStatus.Name)
	fmt.Fprintf(writer, "VERSION\t%s\n", daemonStatus.Version)
	fmt.Fprintf(writer, "LISTEN ADDR\t%s\n", daemonStatus.ListenAddr)
	fmt.Fprintf(writer, "ADMIN ADDR\t%s\n", daemonStatus.AdminListenAddr)
	fmt.Fprintf(writer, "LAN ENABLED\t%t\n", daemonStatus.LANEnabled)
	fmt.Fprintf(writer, "REMOTE MODE\t%s\n", valueOrFallback(daemonStatus.Remote.Mode, "unknown"))
	fmt.Fprintf(writer, "REMOTE SCOPE\t%s\n", valueOrFallback(daemonStatus.Remote.Scope, "unknown"))
	fmt.Fprintf(writer, "PAIRED DEVICES\t%d\n", daemonStatus.PairedDeviceCount)
	fmt.Fprintf(writer, "UPTIME\t%s\n", formatUptime(daemonStatus.UptimeSeconds))
	if daemonStatus.Remote.PublicURL != "" {
		fmt.Fprintf(writer, "PUBLIC URL\t%s\n", daemonStatus.Remote.PublicURL)
	}
	if daemonStatus.Remote.AuthRequired {
		fmt.Fprintf(writer, "AUTH REQUIRED\topen this link once to finish Tailscale login: %s\n", daemonStatus.Remote.AuthURL)
	}
	if daemonStatus.Remote.Warning != "" {
		fmt.Fprintf(writer, "REMOTE WARNING\t%s\n", daemonStatus.Remote.Warning)
	}
	if daemonStatus.PairingTarget.Reachable {
		fmt.Fprintf(writer, "PAIRING TARGET\t%s://%s\n", daemonStatus.PairingTarget.Scheme, daemonStatus.PairingTarget.Host)
	} else {
		fmt.Fprintf(writer, "PAIRING TARGET\tunavailable\n")
		fmt.Fprintf(writer, "PAIRING ERROR\t%s\n", valueOrFallback(daemonStatus.PairingTarget.Error, "unknown"))
	}
	if daemonStatus.ActivePairing != nil {
		fmt.Fprintf(writer, "ACTIVE PAIRING\t%s\n", daemonStatus.ActivePairing.State)
		fmt.Fprintf(writer, "PAIRING CODE\t%s\n", daemonStatus.ActivePairing.Code)
		if !daemonStatus.ActivePairing.ExpiresAt.IsZero() {
			fmt.Fprintf(writer, "PAIRING EXPIRES\t%s\n", daemonStatus.ActivePairing.ExpiresAt.Local().Format(time.RFC3339))
		}
	}

	return writer.Flush()
}

func fetchDaemonStatus(ctx context.Context) (adminclient.DaemonStatus, error) {
	client := adminclient.New(config.Load())
	status, err := client.Status(ctx)
	if err != nil {
		return adminclient.DaemonStatus{}, fmt.Errorf("read daemon status: %w", err)
	}

	return status, nil
}
