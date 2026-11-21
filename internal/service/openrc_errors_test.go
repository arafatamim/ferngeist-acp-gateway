package service

import (
	"testing"
)

// The exact strings below come from OpenRC's src/rc-update/rc-update.c, so a
// wording change in rc-update is a compile-time-visible break here, not a
// silent misclassification.
func TestIsRcUpdateNotInRunlevelMessage(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    bool
	}{
		{"del of a never-added service", "rc-update: service `ferngeist-gateway' is not in the runlevel `default'", true},
		{"del from every runlevel", "rc-update: service `ferngeist-gateway' not found in any of the specified runlevels", true},
		{"another service must not match", "rc-update: service `other-service' is not in the runlevel `default'", false},
		{"a real failure must not match", "rc-update: failed to delete service `ferngeist-gateway' from runlevel `default': No such file or directory", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRcUpdateNotInRunlevelMessage(openrcServiceName, tc.message); got != tc.want {
				t.Fatalf("isRcUpdateNotInRunlevelMessage(%q) = %v, want %v", tc.message, got, tc.want)
			}
		})
	}
}

func TestOpenRCStatusFromMessage(t *testing.T) {
	cases := []struct {
		message string
		active  string
		sub     string
	}{
		{" * status: started", "active", "running"},
		{"status: starting", "activating", ""},
		{"status: stopping", "deactivating", ""},
		{"status: stopped", "inactive", ""},
		{"status: inactive", "inactive", "inactive"},
		{" * status: crashed", "failed", "crashed"},
	}
	for _, tc := range cases {
		t.Run(tc.message, func(t *testing.T) {
			active, sub, ok := openrcStatusFromMessage(tc.message)
			if !ok {
				t.Fatalf("openrcStatusFromMessage(%q) not recognized", tc.message)
			}
			if active != tc.active || sub != tc.sub {
				t.Fatalf("openrcStatusFromMessage(%q) = (%q, %q), want (%q, %q)",
					tc.message, active, sub, tc.active, tc.sub)
			}
		})
	}
}

func TestOpenRCStatusFromMessageRejectsUnknownOutput(t *testing.T) {
	_, _, ok := openrcStatusFromMessage("openrc: service ferngeist-gateway does not exist")
	if ok {
		t.Fatal("unknown rc-service output classified as a status, want ok=false so the error surfaces")
	}
}
