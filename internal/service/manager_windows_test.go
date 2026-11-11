//go:build windows

package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsWrapperUsesSavedPathBeforeOverrides(t *testing.T) {
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Fatal(err)
	}
	systemDir := filepath.Join(os.Getenv("SystemRoot"), "System32")
	// The wrapper passes "daemon run"; where.exe searches those names without
	// executing them, exposing whether the child received the saved PATH.
	tools := filepath.Join(t.TempDir(), "tools ' $env")
	if err := os.MkdirAll(tools, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"daemon.exe", "run.exe"} {
		if err := os.WriteFile(filepath.Join(tools, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, override := range []bool{false, true} {
		t.Run(fmt.Sprintf("override=%t", override), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("PATH", tools)
			t.Setenv("PATHEXT", ".EXE")
			if err := saveInstallOptions(dir, InstallOptions{}); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", systemDir)
			paths := windowsPaths{
				serviceConfigDir: dir, binaryPath: filepath.Join(systemDir, "where.exe"),
				wrapperScriptPath: filepath.Join(dir, "wrapper.ps1"), overrideScriptPath: filepath.Join(dir, "overrides.ps1"),
				dataLogsDir: dir, dataManagedBinDir: dir, daemonLogPath: filepath.Join(dir, "daemon.log"),
			}
			var overrides string
			if override {
				overrides = "$env:PATH = ''\n"
			}
			if err := os.WriteFile(paths.overrideScriptPath, []byte(overrides), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := writeWindowsWrapperScript(paths, InstallOptions{}); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(powershell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", paths.wrapperScriptPath).CombinedOutput()
			if override && err == nil {
				t.Fatal("user override did not replace saved PATH")
			}
			if !override && err != nil {
				t.Fatalf("saved PATH did not reach child: %v\n%s", err, out)
			}
		})
	}
}

func TestWindowsTaskXML(t *testing.T) {
	got := windowsTaskXML(`DOM\us&er`, `C:\a b\run.vbs`)
	for _, want := range []string{
		"<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>",
		"<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>",
		"<Interval>PT1M</Interval>",
		"<Count>999</Count>",
		"<LogonType>InteractiveToken</LogonType>",
		"<RunLevel>LeastPrivilege</RunLevel>",
		`<UserId>DOM\us&amp;er</UserId>`,
		"<Command>wscript.exe</Command>",
		`<Arguments>&#34;C:\a b\run.vbs&#34;</Arguments>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("task XML missing %q", want)
		}
	}
}

func TestEncodeTaskXMLHasUTF16BOM(t *testing.T) {
	b := encodeTaskXML("<a>é</a>")
	if b[0] != 0xFF || b[1] != 0xFE || len(b) != 2+2*len("<a>é</a>")-2 {
		t.Fatalf("unexpected encoding: % x", b)
	}
}

func TestFerngeistProcessListScript(t *testing.T) {
	got := ferngeistProcessListScript(`C:\Users\o'brien\bin\ferngeist-gateway.exe`, 42)
	for _, want := range []string{
		`$_.ExecutablePath -eq 'C:\Users\o''brien\bin\ferngeist-gateway.exe'`,
		"$_.ProcessId -ne 42",
		"Name='ferngeist-gateway.exe'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("script missing %q: %s", want, got)
		}
	}
}

func TestParseTaskState(t *testing.T) {
	if s, found, err := parseTaskState("FG_STATE=Running\r\n"); s != "running" || !found || err != nil {
		t.Errorf("running: %q %v %v", s, found, err)
	}
	if _, found, err := parseTaskState("FG_NOTFOUND\r\n"); found || err != nil {
		t.Errorf("notfound: %v %v", found, err)
	}
	if _, _, err := parseTaskState("FG_ERR=80070005"); !errors.Is(err, ErrServicePermissionDenied) {
		t.Errorf("access denied: %v", err)
	}
	if _, _, err := parseTaskState("garbage"); err == nil {
		t.Error("expected error on unexpected output")
	}
}
