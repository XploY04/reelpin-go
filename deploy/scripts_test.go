package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const testRevision = "1111111111111111111111111111111111111111"
const testImage = "ghcr.io/xploy04/reelpin-go@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const idleQueues = `[{"name":"reelpin.processing.media","messages_ready":0,"messages_unacknowledged":0,"consumers":0},{"name":"reelpin.processing.light","messages_ready":0,"messages_unacknowledged":0,"consumers":0},{"name":"reelpin.notifications","messages_ready":0,"messages_unacknowledged":0,"consumers":0},{"name":"reelpin.scheduled","messages_ready":0,"messages_unacknowledged":0,"consumers":0}]`

func scriptHarness(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	commands := map[string]string{
		"docker": `printf '%s\n' "$*" >> "$TEST_CALLS"
case "$1" in
  image) printf '%s\n' "$TEST_REVISION" ;;
  ps) printf '%s' "${TEST_CONTAINER:-}" ;;
  exec) printf '%s\n' "$TEST_QUEUES" ;;
  run) exit "${TEST_MIGRATE_EXIT:-0}" ;;
  *) exit 99 ;;
esac`,
		"systemctl": `printf '%s\n' "$*" >> "$TEST_CALLS"
case "$1" in
  show)
    if [ "$2" = reelpin-production.slice ]; then
      case "$3" in
        --property=ActiveState) printf '%s\n' "$TEST_SLICE_STATE" ;;
        --property=MemorySwapMax) printf '%s\n' "$TEST_SLICE_SWAP" ;;
        *) exit 97 ;;
      esac
      exit "$TEST_SLICE_SHOW_EXIT"
    fi
    printf '%s\n' "$TEST_UNIT_STATE" ;;
  list-units)
    if [ "$TEST_UNIT_PRESENT" = 1 ]; then echo reelpin.service; fi
    exit "${TEST_LIST_UNITS_EXIT:-0}" ;;
  list-unit-files)
    if [ "$TEST_UNIT_PRESENT" = 1 ]; then echo reelpin.service; fi
    printf '%s' "$TEST_UNIT_FILES_OUTPUT"
    printf '%s' "$TEST_UNIT_FILES_ERROR" >&2
    if [ "$TEST_UNIT_PRESENT" = 1 ]; then exit "${TEST_UNIT_FILES_EXIT:-0}"; fi
    exit "${TEST_UNIT_FILES_EXIT:-1}" ;;
  *) exit 98 ;;
esac`,
		"curl": `case "$*" in
  *:8000/*) printf '%s\n' '{"service":"Python API","version":"old","status":"ok","ready":true}' ;;
  */health/ready) printf '%s\n' "${TEST_READY:-$TEST_HEALTH}" ;;
  *) printf '%s\n' "$TEST_HEALTH" ;;
esac`,
		"sleep": "exit 0",
	}
	for name, body := range commands {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/usr/bin/env bash\nset -eu\n"+body+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_CALLS", calls)
	t.Setenv("TEST_REVISION", testRevision)
	t.Setenv("TEST_HEALTH", `{"service":"ReelPin API","version":"`+testRevision+`","status":"ok","ready":true}`)
	t.Setenv("TEST_READY", "")
	t.Setenv("TEST_UNIT_STATE", "inactive")
	t.Setenv("TEST_SLICE_STATE", "active")
	t.Setenv("TEST_SLICE_SWAP", "0")
	t.Setenv("TEST_SLICE_SHOW_EXIT", "0")
	t.Setenv("TEST_UNIT_PRESENT", "1")
	t.Setenv("TEST_LIST_UNITS_EXIT", "")
	t.Setenv("TEST_UNIT_FILES_EXIT", "")
	t.Setenv("TEST_UNIT_FILES_OUTPUT", "")
	t.Setenv("TEST_UNIT_FILES_ERROR", "")
	t.Setenv("TEST_CONTAINER", "")
	t.Setenv("TEST_QUEUES", idleQueues)
	t.Setenv("TEST_MIGRATE_EXIT", "0")
	envFile := filepath.Join(dir, "production.env")
	if err := os.WriteFile(envFile, []byte("ENVIRONMENT=production\nRABBITMQ_URL=amqp://localhost/reelpin-production\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return calls, envFile
}

func TestSmokeRequiresTheExpectedGoBuild(t *testing.T) {
	for _, tc := range []struct {
		name, base, health, ready, revision, image string
		wantOK                                     bool
	}{
		{name: "expected Go build", base: "http://127.0.0.1:8080", wantOK: true},
		{name: "Python on the old port", base: "http://127.0.0.1:8000"},
		{name: "another service", health: `{"service":"Python API","version":"` + testRevision + `","status":"ok","ready":true}`},
		{name: "stale build", health: `{"service":"ReelPin API","version":"old","status":"ok","ready":true}`},
		{name: "readiness from another build", ready: `{"service":"ReelPin API","version":"old","status":"ok","ready":true}`},
		{name: "not ready", ready: `{"service":"ReelPin API","version":"` + testRevision + `","status":"degraded","ready":false}`},
		{name: "unreadable body", health: `<html>not JSON</html>`},
		{name: "image missing revision", revision: "unknown"},
		{name: "mutable image tag", image: "ghcr.io/xploy04/reelpin-go:dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scriptHarness(t)
			if tc.health != "" {
				t.Setenv("TEST_HEALTH", tc.health)
			}
			if tc.ready != "" {
				t.Setenv("TEST_READY", tc.ready)
			}
			if tc.revision != "" {
				t.Setenv("TEST_REVISION", tc.revision)
			}
			base := tc.base
			if base == "" {
				base = "http://127.0.0.1:8080"
			}
			image := tc.image
			if image == "" {
				image = testImage
			}
			out, err := exec.Command("bash", "reelpin-smoke.sh", base, image).CombinedOutput()
			if (err == nil) != tc.wantOK {
				t.Fatalf("smoke = %v, want success %v: %s", err, tc.wantOK, out)
			}
		})
	}
}

func TestProductionMigrationRefusesUndrainedWork(t *testing.T) {
	for _, tc := range []struct {
		name, state, container, queues string
	}{
		{name: "running unit", state: "active"},
		{name: "unit stopping", state: "deactivating"},
		{name: "orphan container", container: "reelpin-worker-production"},
		{name: "ready work", queues: strings.Replace(idleQueues, `"messages_ready":0`, `"messages_ready":1`, 1)},
		{name: "unacked delivery", queues: strings.Replace(idleQueues, `"messages_unacknowledged":0`, `"messages_unacknowledged":1`, 1)},
		{name: "consumer", queues: strings.Replace(idleQueues, `"consumers":0`, `"consumers":1`, 1)},
		{name: "missing queues", queues: `[{"name":"reelpin.processing.media","messages_ready":0,"messages_unacknowledged":0,"consumers":0}]`},
		{name: "malformed report", queues: `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, envFile := scriptHarness(t)
			if tc.state != "" {
				t.Setenv("TEST_UNIT_STATE", tc.state)
			}
			t.Setenv("TEST_CONTAINER", tc.container)
			if tc.queues != "" {
				t.Setenv("TEST_QUEUES", tc.queues)
			}
			out, err := exec.Command("bash", "reelpin-migrate.sh", "production", testImage, envFile).CombinedOutput()
			if err == nil {
				t.Fatalf("undrained work allowed migration: %s", out)
			}
			commands, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(commands), "run --rm") || strings.Contains(string(commands), "stop ") {
				t.Fatalf("blocked gate ran migration or stopped work: %s", commands)
			}
		})
	}
}

func TestIdleMigrationIsBoundedAndPropagatesFailure(t *testing.T) {
	for _, exitCode := range []string{"0", "12"} {
		t.Run(exitCode, func(t *testing.T) {
			calls, envFile := scriptHarness(t)
			t.Setenv("TEST_MIGRATE_EXIT", exitCode)
			t.Setenv("TEST_QUEUES", strings.TrimSuffix(idleQueues, "]")+`,{"name":"reelpin.scheduled.delay","messages_ready":1,"messages_unacknowledged":0,"consumers":0},{"name":"reelpin.processing.media.dead","messages_ready":1,"messages_unacknowledged":0,"consumers":0}]`)
			out, err := exec.Command("bash", "reelpin-migrate.sh", "production", testImage, envFile).CombinedOutput()
			if (err == nil) != (exitCode == "0") {
				t.Fatalf("migration = %v, expected exit %s: %s", err, exitCode, out)
			}
			commands, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			for _, required := range []string{"--no-healthcheck", "--memory 128m --memory-swap 128m --cpus 0.25", "--cgroup-parent reelpin-production.slice", "migrate --require-idle=drained", "--vhost reelpin-production"} {
				if !strings.Contains(string(commands), required) {
					t.Errorf("migration omitted %q: %s", required, commands)
				}
			}
		})
	}
}

func TestProductionMigrationRequiresNoSwapSlice(t *testing.T) {
	for _, tc := range []struct {
		name, state, swap, exitCode string
	}{
		{name: "inactive", state: "inactive", swap: "0", exitCode: "0"},
		{name: "unlimited swap", state: "active", swap: "infinity", exitCode: "0"},
		{name: "extra swap", state: "active", swap: "4096", exitCode: "0"},
		{name: "missing limit", state: "active", swap: "", exitCode: "0"},
		{name: "inspection error", state: "active", swap: "0", exitCode: "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, envFile := scriptHarness(t)
			t.Setenv("TEST_SLICE_STATE", tc.state)
			t.Setenv("TEST_SLICE_SWAP", tc.swap)
			t.Setenv("TEST_SLICE_SHOW_EXIT", tc.exitCode)
			out, err := exec.Command("bash", "reelpin-migrate.sh", "production", testImage, envFile).CombinedOutput()
			if err == nil {
				t.Fatalf("unsafe slice allowed migration: %s", out)
			}
			commands, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(commands), "run --rm") || strings.Contains(string(commands), "stop ") {
				t.Fatalf("failed slice gate ran migration or stopped work: %s", commands)
			}
		})
	}
}

func TestDevMigrationDoesNotRequireProductionSlice(t *testing.T) {
	calls, envFile := scriptHarness(t)
	t.Setenv("TEST_SLICE_SHOW_EXIT", "1")
	out, err := exec.Command("bash", "reelpin-migrate.sh", "dev", testImage, envFile).CombinedOutput()
	if err != nil {
		t.Fatalf("dev migration required production slice: %v: %s", err, out)
	}
	commands, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(commands), "show ") || strings.Contains(string(commands), "--cgroup-parent") || strings.Contains(string(commands), "--require-idle") {
		t.Fatalf("production-only gate changed dev migration: %s", commands)
	}
}

func TestFirstMigrationChecksTheUninitializedDatabase(t *testing.T) {
	calls, envFile := scriptHarness(t)
	t.Setenv("TEST_UNIT_PRESENT", "0")
	t.Setenv("TEST_QUEUES", "[]")
	out, err := exec.Command("bash", "reelpin-migrate.sh", "production", testImage, envFile).CombinedOutput()
	if err != nil {
		t.Fatalf("first deployment: %v: %s", err, out)
	}
	commands, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(commands), "migrate --require-idle=bootstrap") {
		t.Fatalf("first deployment omitted the empty-database check: %s", commands)
	}
}

func TestProductionMigrationRefusesUnitInventoryErrors(t *testing.T) {
	for _, tc := range []struct {
		name, loadedExit, installedExit, output, diagnostic string
	}{
		{name: "unit-file inventory error", installedExit: "1", diagnostic: "Failed to connect to bus"},
		{name: "unexpected unit-file status", installedExit: "2"},
		{name: "failed inventory with output", installedExit: "1", output: "unexpected inventory output"},
		{name: "loaded-unit inventory error", loadedExit: "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, envFile := scriptHarness(t)
			t.Setenv("TEST_UNIT_PRESENT", "0")
			t.Setenv("TEST_QUEUES", "[]")
			t.Setenv("TEST_LIST_UNITS_EXIT", tc.loadedExit)
			t.Setenv("TEST_UNIT_FILES_EXIT", tc.installedExit)
			t.Setenv("TEST_UNIT_FILES_OUTPUT", tc.output)
			t.Setenv("TEST_UNIT_FILES_ERROR", tc.diagnostic)
			out, err := exec.Command("bash", "reelpin-migrate.sh", "production", testImage, envFile).CombinedOutput()
			if err == nil {
				t.Fatalf("failed unit inventory allowed migration: %s", out)
			}
			if tc.diagnostic != "" && !strings.Contains(string(out), tc.diagnostic) {
				t.Fatalf("inventory diagnostic was lost: %s", out)
			}
			commands, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(commands), "run --rm") || strings.Contains(string(commands), "stop ") {
				t.Fatalf("failed inventory ran migration or stopped work: %s", commands)
			}
		})
	}
}

func TestReleaseSmokesGoOn8080(t *testing.T) {
	workflow, err := os.ReadFile("../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(workflow), "/tmp/reelpin-smoke.sh http://127.0.0.1:8080") {
		t.Fatal("release smoke must reach Go on 8080, not Python on 8000")
	}
	if !strings.Contains(string(workflow), "/tmp/reelpin-migrate.sh") {
		t.Fatal("release must use the gated, bounded migration wrapper")
	}
}
