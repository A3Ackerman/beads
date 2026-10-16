//go:build cgo && integration

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// doltCommitHashLine matches a Dolt commit hash alone on a line of
// `bd sql --csv` output.
var doltCommitHashLine = regexp.MustCompile(`^[0-9a-v]{32}$`)

// TestCLI_ServerModeKVWritesCommit_E2E is the user-level pin for GH#7523 on
// the direct SQL-server route: `bd kv set` and `bd kv clear` must each leave
// the config table clean and move HEAD, the commit `bd dolt push` publishes.
// Before the fix both committed the SQL transaction only, so with
// @@dolt_transaction_commit off (the server default) the row sat in the
// working set until something else staged config, such as `bd dolt commit`.
//
// The last step pins the kv.* screen these writes share with bd remember: a
// dirty internal config key the kv write did not make (the issue_prefix of an
// unwired `bd rename-prefix`) is named and left alone, and `bd kv set` still
// succeeds with a warning, because its own write has landed.
func TestCLI_ServerModeKVWritesCommit_E2E(t *testing.T) {
	skipIfNoDolt(t)

	tmpDir := t.TempDir()
	env := os.Environ()
	database := uniqueTestDBName(t)
	t.Cleanup(func() {
		dropTestDatabase(database, testDoltServerPort)
	})

	mustBD := func(args ...string) string {
		t.Helper()
		out, err := runBDExecAllowErrorWithEnv(t, tmpDir, env, args...)
		if err != nil {
			t.Fatalf("bd %v failed: %v\n%s", args, err, out)
		}
		return out
	}
	sqlLines := func(query string) []string {
		t.Helper()
		var lines []string
		for _, line := range strings.Split(mustBD("sql", "--csv", query), "\n") {
			lines = append(lines, strings.TrimSpace(line))
		}
		return lines
	}
	configDirty := func() bool {
		t.Helper()
		return slices.Contains(sqlLines("SELECT table_name FROM dolt_status WHERE table_name = 'config'"), "config")
	}
	// head returns HEAD's commit hash and message.
	head := func() (string, string) {
		t.Helper()
		hash := ""
		for _, line := range sqlLines("SELECT commit_hash FROM dolt_log LIMIT 1") {
			if doltCommitHashLine.MatchString(line) {
				hash = line
			}
		}
		if hash == "" {
			t.Fatal("no commit hash in dolt_log's first row")
		}
		message := ""
		for _, line := range sqlLines(fmt.Sprintf("SELECT message FROM dolt_log WHERE commit_hash = '%s'", hash)) {
			if strings.HasPrefix(line, "bd: ") {
				message = line
			}
		}
		return hash, message
	}

	mustBD("init", "--backend", "dolt", "--server", "--external",
		"--server-host", "127.0.0.1",
		"--server-port", fmt.Sprintf("%d", testDoltServerPort),
		"--database", database,
		"--prefix", "kvc", "--quiet")
	if !slices.Contains(sqlLines("SELECT @@dolt_transaction_commit"), "0") {
		t.Fatal("precondition: @@dolt_transaction_commit is not 0, so every SQL COMMIT is a Dolt commit and this test proves nothing")
	}
	if configDirty() {
		t.Fatal("precondition: config dirty right after bd init")
	}

	for _, step := range []struct {
		args    []string
		message string
	}{
		{args: []string{"kv", "set", "gh7523", "server-mode kv value"}, message: "bd: kv set (auto-commit) by "},
		{args: []string{"kv", "clear", "gh7523"}, message: "bd: kv clear (auto-commit) by "},
	} {
		before, _ := head()
		mustBD(step.args...)
		if configDirty() {
			t.Fatalf("after bd %s: config left uncommitted in the working set (GH#7523)", strings.Join(step.args, " "))
		}
		after, message := head()
		if after == before {
			t.Fatalf("after bd %s: HEAD still %s, want a new commit carrying the write", strings.Join(step.args, " "), before)
		}
		if !strings.HasPrefix(message, step.message) {
			t.Fatalf("after bd %s: HEAD message = %q, want prefix %q", strings.Join(step.args, " "), message, step.message)
		}
	}
	// A missing key exits non-zero by contract, so only the payload is read.
	if out, _ := runBDExecAllowErrorWithEnv(t, tmpDir, env, "kv", "get", "gh7523", "--json"); !strings.Contains(out, `"found": false`) {
		t.Fatalf("bd kv clear did not remove gh7523:\n%s", out)
	}

	mustBD("rename-prefix", "kvr-")
	if !configDirty() {
		t.Fatal("precondition: config clean after bd rename-prefix, want its unwired issue_prefix write dirty")
	}
	before, _ := head()
	stdout, stderr, err := runBDExecSeparated(t, testBD, tmpDir, env, "kv", "set", "gh7523-screened", "screened value", "--json")
	if err != nil {
		t.Fatalf("bd kv set failed after its write landed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	var set struct {
		Key             string   `json:"key"`
		Value           string   `json:"value"`
		Committed       *bool    `json:"committed"`
		DirtyConfigKeys []string `json:"dirty_config_keys"`
	}
	if err := json.Unmarshal([]byte(stdout), &set); err != nil {
		t.Fatalf("bd kv set --json stdout is not one JSON object: %v\n%s", err, stdout)
	}
	if set.Key != "gh7523-screened" || set.Value != "screened value" || set.Committed == nil || *set.Committed ||
		!slices.Equal(set.DirtyConfigKeys, []string{"issue_prefix"}) {
		t.Fatalf("bd kv set --json = %s, want key gh7523-screened, committed false, dirty_config_keys [issue_prefix]", stdout)
	}
	if !strings.Contains(stderr, "Warning:") || !strings.Contains(stderr, "issue_prefix") {
		t.Fatalf("bd kv set stderr does not warn about the refused commit naming issue_prefix:\n%s", stderr)
	}
	if after, _ := head(); after != before {
		t.Fatalf("the refused commit moved HEAD %s -> %s, want nothing committed over the dirty internal key", before, after)
	}
	if out := mustBD("kv", "get", "gh7523-screened"); !strings.Contains(out, "screened value") {
		t.Fatalf("the screened kv value was not stored:\n%s", out)
	}
	mustBD("dolt", "commit", "-m", "gh7523: commit the screened config")
	if configDirty() {
		t.Fatal("after bd dolt commit: config still dirty")
	}
}
