//go:build windows

package service

import (
	"errors"
	"strings"
	"testing"
)

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
	if _, _, err := parseTaskState("FG_ERR=-2147024891"); !errors.Is(err, ErrServicePermissionDenied) {
		t.Errorf("access denied: %v", err)
	}
	if _, _, err := parseTaskState("garbage"); err == nil {
		t.Error("expected error on unexpected output")
	}
}
