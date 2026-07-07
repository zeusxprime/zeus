package online

import (
	"testing"

	"primecel-gestor/gestor_bot/model"
)

func TestFinalizeDoesNotClampConnectionsToLimit(t *testing.T) {
	sum := finalize(map[string]*Item{
		"david1": {Username: "David1", Connections: 2, Limit: 1, Sources: []string{"ssh"}},
	})
	if sum.Count != 2 {
		t.Fatalf("count = %d, want 2", sum.Count)
	}
	if len(sum.Users) != 1 || sum.Users[0].Connections != 2 || sum.Users[0].Limit != 1 {
		t.Fatalf("users = %#v, want David1 2/1", sum.Users)
	}
}

func TestFinalizeFiltersRootNotty(t *testing.T) {
	sum := finalize(map[string]*Item{
		"root@notty": {Username: "root@notty", Connections: 2, Limit: 2, Sources: []string{"ssh"}},
		"david1":     {Username: "David1", Connections: 1, Limit: 1, Sources: []string{"ssh"}},
	})
	if len(sum.Users) != 1 || sum.Users[0].Username != "David1" {
		t.Fatalf("users = %#v, want only David1", sum.Users)
	}
}

func TestFinalizeCapsVisibleConnectionsOneAboveLimit(t *testing.T) {
	sum := finalize(map[string]*Item{
		"david1": {Username: "David1", Connections: 14, Limit: 1, Sources: []string{"ssh"}},
		"kaua01": {Username: "Kaua01", Connections: 9, Limit: 2, Sources: []string{"ssh"}},
	})
	if sum.Count != 5 {
		t.Fatalf("count = %d, want 5", sum.Count)
	}
	got := map[string]int{}
	for _, it := range sum.Users {
		got[it.Username] = it.Connections
	}
	if got["David1"] != 2 {
		t.Fatalf("David1 connections = %d, want 2", got["David1"])
	}
	if got["Kaua01"] != 3 {
		t.Fatalf("Kaua01 connections = %d, want 3", got["Kaua01"])
	}
}

func TestSSHUsernameExtractorsIgnoreRootNottyAndResolveAccount(t *testing.T) {
	byName := map[string]model.Account{
		"david1": {Username: "David1"},
	}
	if got := sshPrivUsername("sshd: David1 [priv]", byName); got != "david1" {
		t.Fatalf("priv username = %q, want david1", got)
	}
	if got := sshChildUsername("sshd: David1@notty", byName); got != "david1" {
		t.Fatalf("child username = %q, want david1", got)
	}
	if got := sshChildUsername("sshd: root@notty", byName); got != "" {
		t.Fatalf("root child username = %q, want empty", got)
	}
}

func TestSSHPreferredUsesMaxPrimaryWhenSocketUndercounts(t *testing.T) {
	if got := sshPreferredSessionCount(
		map[string]bool{"sock:priv:10": true},
		map[string]bool{"priv:10": true, "priv:20": true},
		map[string]bool{"child:priv:10": true, "child:priv:20": true},
		nil,
		nil,
	); got != 2 {
		t.Fatalf("preferred SSH count = %d, want 2", got)
	}
}

func TestSSHSessionIDUsesPrivAncestor(t *testing.T) {
	procs := map[int]sshProcInfo{
		10: {pid: 10, ppid: 1, user: "root", args: "sshd: David1 [priv]"},
		11: {pid: 11, ppid: 10, user: "david1", args: "sshd: David1@notty"},
	}
	if got := sshSessionIDForPID(11, procs, "100.64.0.10:51000"); got != "priv:10" {
		t.Fatalf("session id = %q, want priv:10", got)
	}
}

func TestSSHPreferredDoesNotInflateSingleSocketWithExtraChild(t *testing.T) {
	if got := sshPreferredSessionCount(
		map[string]bool{"sock:peer:1": true},
		map[string]bool{"priv:10": true},
		map[string]bool{"child:ppid:10": true, "child:ppid:11": true},
		nil,
		nil,
	); got != 1 {
		t.Fatalf("preferred SSH count = %d, want 1", got)
	}
}

func TestSummaryFromJSONSupportsUsernameArrayMap(t *testing.T) {
	sum := summaryFromJSON(map[string]any{
		"users": map[string]any{
			"David1": []any{"sess1", "sess2"},
			"Kaua01": []any{"sess1"},
		},
	})
	got := map[string]int{}
	for _, it := range sum.Users {
		got[it.Username] = it.Connections
	}
	if got["David1"] != 2 || got["Kaua01"] != 1 || sum.Count != 3 {
		t.Fatalf("summary = %#v, want David1=2 Kaua01=1 count=3", sum)
	}
}
