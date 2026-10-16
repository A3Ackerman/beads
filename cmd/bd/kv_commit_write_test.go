//go:build cgo

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/uow"
)

// kvCommitStore is a direct-route store on which `bd kv set` and `bd kv clear`
// land their config rows, recording the scoped commits that follow them.
type kvCommitStore struct {
	storage.DoltStorage
	rows              map[string]string
	userKVCommits     []string
	configOnlyCommits int
	commitErr         error
}

func (s *kvCommitStore) SetConfig(_ context.Context, key, value string) error {
	s.rows[key] = value
	return nil
}

func (s *kvCommitStore) DeleteConfig(_ context.Context, key string) error {
	delete(s.rows, key)
	return nil
}

func (s *kvCommitStore) CommitConfigUserKVOnly(_ context.Context, message string) error {
	s.userKVCommits = append(s.userKVCommits, message)
	return s.commitErr
}

func (s *kvCommitStore) CommitConfigOnly(context.Context, string) error {
	s.configOnlyCommits++
	return nil
}

// useKVCommitStore puts the kv verbs' direct route on a kvCommitStore whose
// scoped commit returns commitErr. The caller picks the route by setting
// serverMode / proxiedServerMode; both start false (embedded).
func useKVCommitStore(t *testing.T, commitErr error) *kvCommitStore {
	t.Helper()
	t.Setenv("BEADS_DIR", t.TempDir())
	saveAndRestoreGlobals(t)
	saveStorageMode(t)
	serverMode, proxiedServerMode = false, false
	fake := &kvCommitStore{rows: map[string]string{}, commitErr: commitErr}
	store = fake
	setStoreActive(true)
	oldJSON, oldWrote, oldCtx := jsonOutput, commandDidWrite.Load(), rootCtx
	t.Cleanup(func() {
		jsonOutput = oldJSON
		commandDidWrite.Store(oldWrote)
		rootCtx = oldCtx
	})
	jsonOutput = false
	commandDidWrite.Store(false)
	rootCtx = context.Background()
	return fake
}

// kvWriteStep is one kv write verb, with what it prints and the commit
// message its scoped commit carries.
type kvWriteStep struct {
	verb    string
	run     func() error
	text    string
	json    map[string]interface{}
	message string
}

func kvWriteSteps() []kvWriteStep {
	return []kvWriteStep{
		{
			verb:    "kv set",
			run:     func() error { return kvSetCmd.RunE(kvSetCmd, []string{"gh7523", "a value that lands"}) },
			text:    "Set gh7523 = a value that lands",
			json:    map[string]interface{}{"key": "gh7523", "value": "a value that lands"},
			message: "bd: kv set (auto-commit) by ",
		},
		{
			verb:    "kv clear",
			run:     func() error { return kvClearCmd.RunE(kvClearCmd, []string{"gh7523"}) },
			text:    "Cleared gh7523",
			json:    map[string]interface{}{"key": "gh7523", "deleted": "true"},
			message: "bd: kv clear (auto-commit) by ",
		},
	}
}

// decodeKVJSON parses a kv verb's --json stdout, failing on anything that is
// not one JSON object.
func decodeKVJSON(t *testing.T, verb, stdout string) map[string]interface{} {
	t.Helper()
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("bd %s --json stdout is not one JSON object: %v\n%s", verb, err, stdout)
	}
	return got
}

// GH#7523: on the direct SQL-server route nothing else commits a kv write —
// the PersistentPostRun auto-commit returns early off the embedded route and
// plain Commit excludes config — so each verb commits its own write, through
// the kv.*-screened commit memories use, and marks the invocation as having
// written. A committed write's output keeps its shipped shape: a mail client
// parses the --json.
func TestKVWriteVerbsCommitOnTheSQLServerRoute(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%v", asJSON), func(t *testing.T) {
			fake := useKVCommitStore(t, nil)
			serverMode = true
			jsonOutput = asJSON

			for _, step := range kvWriteSteps() {
				commandDidWrite.Store(false)
				before := len(fake.userKVCommits)
				var runErr error
				stdout, stderr := captureStdio(t, func() { runErr = step.run() })
				if runErr != nil {
					t.Fatalf("bd %s: %v", step.verb, runErr)
				}
				if !commandDidWrite.Load() {
					t.Errorf("bd %s did not mark the invocation as having written", step.verb)
				}
				if got := len(fake.userKVCommits) - before; got != 1 {
					t.Fatalf("bd %s: CommitConfigUserKVOnly calls = %d, want 1: the write is left in the working set", step.verb, got)
				}
				if msg := fake.userKVCommits[before]; !strings.HasPrefix(msg, step.message) {
					t.Errorf("bd %s commit message = %q, want prefix %q", step.verb, msg, step.message)
				}
				if strings.Contains(stderr, "Warning:") {
					t.Errorf("bd %s warned after a commit that succeeded:\n%s", step.verb, stderr)
				}
				if !asJSON {
					if !strings.Contains(stdout, step.text) {
						t.Errorf("bd %s stdout = %q, want %q", step.verb, stdout, step.text)
					}
					continue
				}
				want := step.json
				want["schema_version"] = float64(JSONSchemaVersion)
				if got := decodeKVJSON(t, step.verb, stdout); !reflect.DeepEqual(got, want) {
					t.Errorf("bd %s --json = %#v, want the shipped shape %#v", step.verb, got, want)
				}
			}
			if fake.configOnlyCommits != 0 {
				t.Errorf("CommitConfigOnly calls = %d, want 0: a kv write takes only kv.* rows with it", fake.configOnlyCommits)
			}
			if _, ok := fake.rows["kv.gh7523"]; ok {
				t.Errorf("kv.gh7523 still stored, want the clear to have landed")
			}
		})
	}
}

// A refused commit must not turn a write that landed into exit 1: like bd
// remember and bd forget, the kv verbs warn on stderr, print their normal
// result and, under --json, add committed:false, commit_error and the refused
// keys.
func TestKVWriteVerbsWarnAndSucceedWhenTheCommitIsRefused(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%v", asJSON), func(t *testing.T) {
			refusal := &storage.ConfigCommitRefusedError{Keys: []string{"issue_prefix"}}
			fake := useKVCommitStore(t, refusal)
			serverMode = true
			jsonOutput = asJSON

			for _, step := range kvWriteSteps() {
				var runErr error
				stdout, stderr := captureStdio(t, func() { runErr = step.run() })
				if runErr != nil {
					t.Fatalf("bd %s returned %v after its write landed, want success with a warning", step.verb, runErr)
				}
				if !strings.Contains(stderr, "Warning: committing kv write: refusing to commit") ||
					!strings.Contains(stderr, "issue_prefix") {
					t.Errorf("bd %s stderr does not warn about the refused commit naming issue_prefix:\n%s", step.verb, stderr)
				}
				if !asJSON {
					if !strings.Contains(stdout, step.text) {
						t.Errorf("bd %s stdout = %q, want the normal result %q", step.verb, stdout, step.text)
					}
					continue
				}
				want := step.json
				want["schema_version"] = float64(JSONSchemaVersion)
				want["committed"] = false
				want["commit_error"] = "committing kv write: " + refusal.Error()
				want["dirty_config_keys"] = []interface{}{"issue_prefix"}
				if got := decodeKVJSON(t, step.verb, stdout); !reflect.DeepEqual(got, want) {
					t.Errorf("bd %s --json = %#v, want %#v", step.verb, got, want)
				}
			}
			if len(fake.userKVCommits) != 2 {
				t.Errorf("CommitConfigUserKVOnly calls = %d, want one per command", len(fake.userKVCommits))
			}
			if _, ok := fake.rows["kv.gh7523"]; ok {
				t.Errorf("kv.gh7523 still stored, want the clear to have landed")
			}
		})
	}
}

// In embedded mode the PersistentPostRun auto-commit commits the working set,
// and it runs only for an invocation marked as having written (GH#7523: bd kv
// set/clear never were, so their rows stayed uncommitted). The verbs mark it
// and leave the commit to it, making none of their own.
func TestKVWriteVerbsLeaveTheEmbeddedCommitToTheEpilogue(t *testing.T) {
	fake := useKVCommitStore(t, nil)

	for _, step := range kvWriteSteps() {
		commandDidWrite.Store(false)
		var runErr error
		captureStdio(t, func() { runErr = step.run() })
		if runErr != nil {
			t.Fatalf("bd %s: %v", step.verb, runErr)
		}
		if !commandDidWrite.Load() {
			t.Errorf("bd %s did not mark the invocation as having written, so the embedded auto-commit never runs", step.verb)
		}
	}
	if len(fake.userKVCommits) != 0 || fake.configOnlyCommits != 0 {
		t.Errorf("scoped commits = %d user kv, %d config, want none in embedded mode", len(fake.userKVCommits), fake.configOnlyCommits)
	}
}

// batch and off defer server-mode version commits to `bd dolt commit`, so a
// kv write defers with them.
func TestKVWriteVerbsDeferTheirCommitUnderBatchAndOff(t *testing.T) {
	for _, mode := range []doltAutoCommitMode{doltAutoCommitBatch, doltAutoCommitOff} {
		t.Run(string(mode), func(t *testing.T) {
			fake := useKVCommitStore(t, nil)
			serverMode = true
			doltAutoCommit = string(mode)

			for _, step := range kvWriteSteps() {
				var runErr error
				captureStdio(t, func() { runErr = step.run() })
				if runErr != nil {
					t.Fatalf("bd %s: %v", step.verb, runErr)
				}
			}
			if len(fake.userKVCommits) != 0 || fake.configOnlyCommits != 0 {
				t.Errorf("scoped commits = %d user kv, %d config, want none in %s mode", len(fake.userKVCommits), fake.configOnlyCommits, mode)
			}
		})
	}
}

// kvUnitOfWork is a proxied-route unit of work whose config role takes the kv
// verbs' writes and whose commit records the message the role commits with.
type kvUnitOfWork struct {
	uow.UnitOfWork
	config  *kvConfigUseCase
	commits *[]string
}

func (u kvUnitOfWork) Close(context.Context) {}

func (u kvUnitOfWork) Commit(_ context.Context, message string) error {
	*u.commits = append(*u.commits, message)
	return nil
}

func (u kvUnitOfWork) ConfigUseCase() domain.ConfigUseCase { return u.config }

type kvConfigUseCase struct {
	domain.ConfigUseCase
	rows map[string]string
}

func (c *kvConfigUseCase) SetConfig(_ context.Context, key, value string) error {
	c.rows[key] = value
	return nil
}

func (c *kvConfigUseCase) DeleteConfig(_ context.Context, key string) error {
	delete(c.rows, key)
	return nil
}

type kvUnitOfWorkProvider struct {
	config  *kvConfigUseCase
	commits *[]string
}

func (p kvUnitOfWorkProvider) NewUOW(context.Context) (uow.UnitOfWork, error) {
	return kvUnitOfWork{config: p.config, commits: p.commits}, nil
}

func (p kvUnitOfWorkProvider) Close(context.Context) error { return nil }

// The proxied route commits each kv write inside the role's own unit of work,
// so the direct route's scoped commit must not run there as a second commit
// for the same write.
func TestKVWriteVerbsCommitOnlyInsideTheUnitOfWorkOnTheProxiedRoute(t *testing.T) {
	fake := useKVCommitStore(t, nil)
	proxiedServerMode = true
	var roleCommits []string
	config := &kvConfigUseCase{rows: map[string]string{}}
	oldProvider := uowProvider
	t.Cleanup(func() { uowProvider = oldProvider })
	uowProvider = kvUnitOfWorkProvider{config: config, commits: &roleCommits}

	for _, step := range kvWriteSteps() {
		var runErr error
		captureStdio(t, func() { runErr = step.run() })
		if runErr != nil {
			t.Fatalf("bd %s: %v", step.verb, runErr)
		}
	}
	if want := []string{"bd: kv set gh7523", "bd: kv clear gh7523"}; !reflect.DeepEqual(roleCommits, want) {
		t.Errorf("unit-of-work commits = %q, want %q", roleCommits, want)
	}
	if len(fake.userKVCommits) != 0 || fake.configOnlyCommits != 0 {
		t.Errorf("scoped commits = %d user kv, %d config, want none on the proxied route", len(fake.userKVCommits), fake.configOnlyCommits)
	}
}

// noteDirectConfigWrite is the marker the kv verbs' direct route calls. It is
// direct-route only: the proxied route's unit of work has already committed,
// so the marker must not ask the epilogue for a commit there.
func TestNoteDirectConfigWriteMarksOnlyTheDirectRoute(t *testing.T) {
	for _, proxied := range []bool{false, true} {
		t.Run(fmt.Sprintf("proxied=%v", proxied), func(t *testing.T) {
			useKVCommitStore(t, nil)
			proxiedServerMode = proxied
			noteDirectConfigWrite()
			if got := commandDidWrite.Load(); got == proxied {
				t.Errorf("commandDidWrite = %v on the proxied=%v route, want %v", got, proxied, !proxied)
			}
		})
	}
}
