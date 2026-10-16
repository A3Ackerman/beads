//go:build cgo

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage/embeddeddolt"
)

// bdKV runs "bd kv" with the given args and returns stdout.
func bdKV(t *testing.T, bd, dir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"kv"}, args...)
	cmd := exec.Command(bd, fullArgs...)
	cmd.Dir = dir
	cmd.Env = bdEnv(dir)
	stdout, stderr, err := runCommandBuffers(t, cmd)
	if err != nil {
		t.Fatalf("bd kv %s failed: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// bdKVFail runs "bd kv" expecting failure.
func bdKVFail(t *testing.T, bd, dir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"kv"}, args...)
	cmd := exec.Command(bd, fullArgs...)
	cmd.Dir = dir
	cmd.Env = bdEnv(dir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected bd kv %s to fail, but succeeded:\n%s", strings.Join(args, " "), out)
	}
	return string(out)
}

// bdKVListJSON runs "bd kv list --json" plus any extra args (such as
// --prefix) and returns the parsed map. Output with no JSON object fails the
// test rather than reading as an empty listing, which would let a check for
// "no pairs" pass on output that is not the JSON shape at all.
func bdKVListJSON(t *testing.T, bd, dir string, extraArgs ...string) map[string]string {
	t.Helper()
	args := append([]string{"kv", "list", "--json"}, extraArgs...)
	cmd := exec.Command(bd, args...)
	cmd.Dir = dir
	cmd.Env = bdEnv(dir)
	stdout, stderr, err := runCommandBuffers(t, cmd)
	if err != nil {
		t.Fatalf("bd %s failed: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	s := strings.TrimSpace(stdout.String())
	start := strings.Index(s, "{")
	if start < 0 {
		t.Fatalf("no JSON object in bd %s output: %s", strings.Join(args, " "), s)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(s[start:]), &raw); err != nil {
		t.Fatalf("parse kv list JSON: %v\n%s", err, s)
	}
	m := make(map[string]string, len(raw))
	for k, v := range raw {
		if k == "schema_version" {
			continue
		}
		if sv, ok := v.(string); ok {
			m[k] = sv
		}
	}
	return m
}

// embeddedConfigDirty reports whether the config table has uncommitted
// changes in an embedded workspace's working set, read with SQL beneath every
// bd code path, so no bd command runs between a write and this observation.
func embeddedConfigDirty(t *testing.T, beadsDir string) bool {
	t.Helper()
	cfg, _ := configfile.Load(beadsDir)
	database := ""
	if cfg != nil {
		database = cfg.GetDoltDatabase()
	}
	db, cleanup, err := embeddeddolt.OpenSQL(t.Context(), filepath.Join(beadsDir, "embeddeddolt"), database, "main")
	if err != nil {
		t.Fatalf("OpenSQL: %v", err)
	}
	defer cleanup()
	var count int
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM dolt_status WHERE table_name = 'config'").Scan(&count); err != nil {
		t.Fatalf("query dolt_status: %v", err)
	}
	return count > 0
}

func TestEmbeddedKV(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "tk")

	// ===== Set and Get =====

	t.Run("kv_set_and_get", func(t *testing.T) {
		bdKV(t, bd, dir, "set", "mykey", "myvalue")
		out := bdKV(t, bd, dir, "get", "mykey")
		if !strings.Contains(out, "myvalue") {
			t.Errorf("expected 'myvalue' in get output: %s", out)
		}
	})

	t.Run("kv_set_overwrite", func(t *testing.T) {
		bdKV(t, bd, dir, "set", "overkey", "first")
		bdKV(t, bd, dir, "set", "overkey", "second")
		out := bdKV(t, bd, dir, "get", "overkey")
		if !strings.Contains(out, "second") {
			t.Errorf("expected 'second' after overwrite: %s", out)
		}
	})

	t.Run("kv_set_special_chars", func(t *testing.T) {
		bdKV(t, bd, dir, "set", "special", "hello world with spaces")
		out := bdKV(t, bd, dir, "get", "special")
		if !strings.Contains(out, "hello world with spaces") {
			t.Errorf("expected value with spaces: %s", out)
		}
	})

	// ===== List =====

	t.Run("kv_list", func(t *testing.T) {
		out := bdKV(t, bd, dir, "list")
		if !strings.Contains(out, "mykey") {
			t.Errorf("expected 'mykey' in list output: %s", out)
		}
	})

	t.Run("kv_list_json", func(t *testing.T) {
		m := bdKVListJSON(t, bd, dir)
		if v, ok := m["mykey"]; !ok || v != "myvalue" {
			t.Errorf("expected mykey=myvalue in JSON, got %q (exists=%v)", v, ok)
		}
	})

	t.Run("kv_list_prefix_json", func(t *testing.T) {
		// mail.doggerel.* shares mail.dog.'s leading characters, so a match
		// that dropped the prefix's trailing dot would leak it. The keys set
		// by the subtests above must not leak either.
		for _, kvp := range [][2]string{
			{"mail.dog.m1", "first"},
			{"mail.dog.m2", "second"},
			{"mail.doggerel.m3", "sibling"},
		} {
			bdKV(t, bd, dir, "set", kvp[0], kvp[1])
		}

		m := bdKVListJSON(t, bd, dir, "--prefix", "mail.dog.")
		if want := map[string]string{"mail.dog.m1": "first", "mail.dog.m2": "second"}; !reflect.DeepEqual(m, want) {
			t.Errorf("kv list --prefix mail.dog. --json = %v; want %v", m, want)
		}

		if m := bdKVListJSON(t, bd, dir, "--prefix", "mail.owl."); len(m) != 0 {
			t.Errorf("kv list --prefix mail.owl. --json = %v; want no pairs", m)
		}
	})

	// ===== Clear =====

	t.Run("kv_clear", func(t *testing.T) {
		bdKV(t, bd, dir, "set", "clearme", "temporary")
		out := bdKV(t, bd, dir, "get", "clearme")
		if !strings.Contains(out, "temporary") {
			t.Fatalf("expected 'temporary' before clear: %s", out)
		}
		bdKV(t, bd, dir, "clear", "clearme")
		// Verify it's gone from list
		m := bdKVListJSON(t, bd, dir)
		if _, ok := m["clearme"]; ok {
			t.Error("expected clearme to be absent from kv list after clear")
		}
	})

	// THE AUTO-COMMIT EPILOGUE (GH#7523). A direct-route kv write lands in the
	// Dolt working set, and in embedded mode the PersistentPostRun auto-commit
	// is what commits it, keyed on the invocation having marked itself as a
	// write. bd kv set and bd kv clear never did, so their rows sat in the
	// working set while bd config set and bd remember got their commits — and
	// every assertion above still passed, because a working-set row reads and
	// lists exactly like a committed one. dolt_log and dolt_status are the
	// only places the difference shows.
	//
	// Its own workspace, so the counts are deterministic (the pattern
	// TestEmbeddedMemory's write_verbs_flag_the_auto_commit_epilogue uses).
	t.Run("write_verbs_commit_their_config_writes", func(t *testing.T) {
		edir, ebeads, _ := bdInit(t, bd, "--prefix", "kc")

		// Warm-up write: a fresh workspace's first command may commit
		// bookkeeping of its own, which is not what this is measuring.
		bdKV(t, bd, edir, "set", "kv-commit-warmup", "warm the workspace up")

		before := countMemoryDoltCommits(t, ebeads)
		bdKV(t, bd, edir, "set", "kv-commit-subject", "committed, not just stored")
		afterSet := countMemoryDoltCommits(t, ebeads)
		if got := afterSet - before; got != 1 {
			t.Errorf("bd kv set: dolt_log %d -> %d (delta %d), want exactly one auto-commit", before, afterSet, got)
		}
		if embeddedConfigDirty(t, ebeads) {
			t.Errorf("bd kv set left the config table modified in the working set")
		}

		bdKV(t, bd, edir, "clear", "kv-commit-subject")
		afterClear := countMemoryDoltCommits(t, ebeads)
		if got := afterClear - afterSet; got != 1 {
			t.Errorf("bd kv clear: dolt_log %d -> %d (delta %d), want exactly one auto-commit", afterSet, afterClear, got)
		}
		if embeddedConfigDirty(t, ebeads) {
			t.Errorf("bd kv clear left the config table modified in the working set")
		}
	})

	// ===== Error Cases =====

	t.Run("kv_get_missing_key", func(t *testing.T) {
		// get on nonexistent key — check behavior
		cmd := exec.Command(bd, "kv", "get", "nonexistent_key_xyz")
		cmd.Dir = dir
		cmd.Env = bdEnv(dir)
		out, err := cmd.CombinedOutput()
		// May succeed with empty or fail — either way no crash
		_ = err
		_ = out
	})

	t.Run("kv_set_no_args", func(t *testing.T) {
		bdKVFail(t, bd, dir, "set")
	})

	t.Run("kv_get_no_args", func(t *testing.T) {
		bdKVFail(t, bd, dir, "get")
	})

	t.Run("kv_clear_no_args", func(t *testing.T) {
		bdKVFail(t, bd, dir, "clear")
	})
}

// TestEmbeddedKVConcurrent exercises kv operations concurrently.
func TestEmbeddedKVConcurrent(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "kx")

	const numWorkers = 8

	type workerResult struct {
		worker int
		err    error
	}

	results := make([]workerResult, numWorkers)
	var wg sync.WaitGroup
	wg.Add(numWorkers)

	for w := 0; w < numWorkers; w++ {
		go func(worker int) {
			defer wg.Done()
			r := workerResult{worker: worker}

			// Each worker sets its own keys
			for i := 0; i < 5; i++ {
				key := fmt.Sprintf("w%d-k%d", worker, i)
				value := fmt.Sprintf("v%d-%d", worker, i)

				cmd := exec.Command(bd, "kv", "set", key, value)
				cmd.Dir = dir
				cmd.Env = bdEnv(dir)
				out, err := cmd.CombinedOutput()
				if err != nil {
					r.err = fmt.Errorf("set %s: %v\n%s", key, err, out)
					results[worker] = r
					return
				}
			}

			// Read back
			for i := 0; i < 5; i++ {
				key := fmt.Sprintf("w%d-k%d", worker, i)
				expected := fmt.Sprintf("v%d-%d", worker, i)

				cmd := exec.Command(bd, "kv", "get", key)
				cmd.Dir = dir
				cmd.Env = bdEnv(dir)
				out, err := cmd.CombinedOutput()
				if err != nil {
					r.err = fmt.Errorf("get %s: %v\n%s", key, err, out)
					results[worker] = r
					return
				}
				if !strings.Contains(string(out), expected) {
					r.err = fmt.Errorf("key %s expected %q, got %q", key, expected, string(out))
					results[worker] = r
					return
				}
			}

			// Clear one key
			clearKey := fmt.Sprintf("w%d-k0", worker)
			cmd := exec.Command(bd, "kv", "clear", clearKey)
			cmd.Dir = dir
			cmd.Env = bdEnv(dir)
			out, err := cmd.CombinedOutput()
			if err != nil {
				r.err = fmt.Errorf("clear %s: %v\n%s", clearKey, err, out)
				results[worker] = r
				return
			}

			results[worker] = r
		}(w)
	}
	wg.Wait()

	for _, r := range results {
		if r.err != nil && !strings.Contains(r.err.Error(), "one writer at a time") {
			t.Errorf("worker %d failed: %v", r.worker, r.err)
		}
	}

	// Verify remaining keys only for workers that succeeded (err==nil).
	// With exclusive flock, some workers may fail with "one writer at a time".
	m := bdKVListJSON(t, bd, dir)
	var successCount int
	for _, r := range results {
		if r.err != nil {
			continue
		}
		successCount++
		w := r.worker
		clearedKey := fmt.Sprintf("w%d-k0", w)
		if _, ok := m[clearedKey]; ok {
			t.Errorf("expected %s to be cleared", clearedKey)
		}
		for i := 1; i < 5; i++ {
			key := fmt.Sprintf("w%d-k%d", w, i)
			expected := fmt.Sprintf("v%d-%d", w, i)
			if v, ok := m[key]; !ok || v != expected {
				t.Errorf("key %s expected %q, got %q (exists=%v)", key, expected, v, ok)
			}
		}
	}
	if successCount == 0 {
		t.Fatal("expected at least 1 worker to succeed")
	}
}
