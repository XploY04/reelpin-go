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
const previousImage = "ghcr.io/xploy04/reelpin-go@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const previousRevision = "2222222222222222222222222222222222222222"
const idleQueues = `[{"name":"reelpin.processing.media","messages_ready":0,"messages_unacknowledged":0,"consumers":0},{"name":"reelpin.processing.light","messages_ready":0,"messages_unacknowledged":0,"consumers":0},{"name":"reelpin.notifications","messages_ready":0,"messages_unacknowledged":0,"consumers":0},{"name":"reelpin.scheduled","messages_ready":0,"messages_unacknowledged":0,"consumers":0}]`

func scriptHarness(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	commands := map[string]string{
		"docker": `printf '%s\n' "$*" >> "$TEST_CALLS"
case "$1" in
  image)
    if [ "$3" = "${TEST_PREVIOUS_IMAGE:-}" ]; then
      printf '%s\n' "$TEST_PREVIOUS_REVISION"
    else
      printf '%s\n' "$TEST_REVISION"
    fi ;;
  pull) exit 0 ;;
  ps) printf '%s' "${TEST_CONTAINER:-}" ;;
  exec) printf '%s\n' "$TEST_QUEUES" ;;
  run) exit "${TEST_MIGRATE_EXIT:-0}" ;;
  *) exit 99 ;;
esac`,
		"systemctl": `printf '%s\n' "$*" >> "$TEST_CALLS"
case "$1" in
  restart)
    count=0
    if [ -r "$TEST_RESTARTS" ]; then count=$(cat "$TEST_RESTARTS"); fi
    count=$((count + 1))
    printf '%s\n' "$count" > "$TEST_RESTARTS"
    if [ "$count" = 1 ] && [ "$TEST_RESTART_FAIL" = 1 ]; then exit 1; fi
    if [ "$count" != 1 ] && [ "$TEST_ROLLBACK_RESTART_FAIL" = 1 ]; then exit 1; fi
    sed -n 's/^REELPIN_IMAGE=//p' "$TEST_DEPLOY_STATE" > "$TEST_RUNNING_IMAGE" ;;
  is-active)
    count=$(cat "$TEST_RESTARTS")
    if [ "$count" = 1 ]; then exit "$TEST_DEPLOY_ACTIVE_EXIT"; fi
    exit "$TEST_ROLLBACK_ACTIVE_EXIT" ;;
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
		"curl": `if [ -n "${TEST_RUNNING_IMAGE:-}" ]; then
  printf 'curl %s\n' "$*" >> "$TEST_CALLS"
  if [ "$(cat "$TEST_RUNNING_IMAGE")" = "$TEST_PREVIOUS_IMAGE" ]; then
    printf '%s\n' "$TEST_RESTORED_HEALTH"
    exit 0
  fi
  if [ "$TEST_CURL_EXIT" != 0 ]; then exit "$TEST_CURL_EXIT"; fi
fi
case "$*" in
  *:8000/*) printf '%s\n' '{"service":"Python API","version":"old","status":"ok","ready":true}' ;;
  */health/ready) printf '%s\n' "${TEST_READY:-$TEST_HEALTH}" ;;
  *) printf '%s\n' "$TEST_HEALTH" ;;
esac`,
		"sleep":      "exit 0",
		"journalctl": "printf '%s\\n' 'test service journal'",
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

func deploymentHarness(t *testing.T, service string) (string, string, string, string) {
	t.Helper()
	calls, envFile := scriptHarness(t)
	dir := filepath.Dir(envFile)
	state := filepath.Join(dir, "reelpin-"+service+"-production.image")
	if err := os.WriteFile(state, []byte("REELPIN_IMAGE="+previousImage+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"reelpin-deploy.sh", "reelpin-smoke.sh"} {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		body = []byte(strings.ReplaceAll(string(body), "/etc/reelpin/", dir+"/"))
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TEST_PREVIOUS_IMAGE", previousImage)
	t.Setenv("TEST_PREVIOUS_REVISION", previousRevision)
	t.Setenv("TEST_RESTORED_HEALTH", `{"service":"ReelPin API","version":"`+previousRevision+`","status":"ok","ready":true}`)
	t.Setenv("TEST_DEPLOY_STATE", state)
	t.Setenv("TEST_RUNNING_IMAGE", filepath.Join(dir, "running-image"))
	t.Setenv("TEST_RESTARTS", filepath.Join(dir, "restarts"))
	t.Setenv("TEST_RESTART_FAIL", "0")
	t.Setenv("TEST_DEPLOY_ACTIVE_EXIT", "0")
	t.Setenv("TEST_ROLLBACK_RESTART_FAIL", "0")
	t.Setenv("TEST_ROLLBACK_ACTIVE_EXIT", "0")
	t.Setenv("TEST_CURL_EXIT", "0")
	return filepath.Join(dir, "reelpin-deploy.sh"), calls, state, filepath.Join(dir, "restarts")
}

func TestDeployRestoresAndVerifiesPreviousAPIOnFailure(t *testing.T) {
	for _, failure := range []string{"restart", "inactive", "unreachable", "stale build", "not ready"} {
		t.Run(failure, func(t *testing.T) {
			script, calls, state, restarts := deploymentHarness(t, "api")
			switch failure {
			case "restart":
				t.Setenv("TEST_RESTART_FAIL", "1")
			case "inactive":
				t.Setenv("TEST_DEPLOY_ACTIVE_EXIT", "1")
			case "unreachable":
				t.Setenv("TEST_CURL_EXIT", "7")
			case "stale build":
				t.Setenv("TEST_HEALTH", `{"service":"ReelPin API","version":"old","status":"ok","ready":true}`)
			case "not ready":
				t.Setenv("TEST_READY", `{"service":"ReelPin API","version":"`+testRevision+`","status":"degraded","ready":false}`)
			}
			out, err := exec.Command("bash", script, "api", "production", testImage).CombinedOutput()
			if err == nil {
				t.Fatalf("failed API deployment succeeded: %s", out)
			}
			for path, want := range map[string]string{state: "REELPIN_IMAGE=" + previousImage + "\n", restarts: "2\n"} {
				body, err := os.ReadFile(path)
				if err != nil || string(body) != want {
					t.Fatalf("rollback state at %s = %q, %v, want %q: %s", path, body, err, want, out)
				}
			}
			commands, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(commands), "image inspect "+previousImage) || !strings.Contains(string(out), "rollback verified") {
				t.Fatalf("previous API revision/readiness was not verified: %s\n%s", commands, out)
			}
			if strings.Contains(string(commands), "worker") || strings.Contains(string(commands), "nginx") || strings.Contains(string(commands), "run --rm") {
				t.Fatalf("API rollback touched another service or migration: %s", commands)
			}
		})
	}
}

func TestDeployReportsFailedRollback(t *testing.T) {
	for _, failure := range []string{"restart", "inactive", "wrong revision"} {
		t.Run(failure, func(t *testing.T) {
			script, _, _, _ := deploymentHarness(t, "api")
			t.Setenv("TEST_CURL_EXIT", "7")
			switch failure {
			case "restart":
				t.Setenv("TEST_ROLLBACK_RESTART_FAIL", "1")
			case "inactive":
				t.Setenv("TEST_ROLLBACK_ACTIVE_EXIT", "1")
			case "wrong revision":
				t.Setenv("TEST_RESTORED_HEALTH", `{"service":"ReelPin API","version":"wrong","status":"ok","ready":true}`)
			}
			out, err := exec.Command("bash", script, "api", "production", testImage).CombinedOutput()
			if err == nil || !strings.Contains(string(out), "rollback failed") || strings.Contains(string(out), "rollback verified") {
				t.Fatalf("unhealthy previous API was reported as restored: %v: %s", err, out)
			}
		})
	}
}

func TestDeployReportsMissingRollbackImage(t *testing.T) {
	script, _, state, restarts := deploymentHarness(t, "api")
	if err := os.Remove(state); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_CURL_EXIT", "7")
	out, err := exec.Command("bash", script, "api", "production", testImage).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "no previous image") {
		t.Fatalf("first-deploy smoke failure was not reported: %v: %s", err, out)
	}
	count, err := os.ReadFile(restarts)
	if err != nil || string(count) != "1\n" {
		t.Fatalf("first deploy attempted an unavailable rollback: %q, %v", count, err)
	}
}

func TestDeployAcceptsHealthyAPIAndDoesNotSmokeWorker(t *testing.T) {
	for _, service := range []string{"api", "worker"} {
		t.Run(service, func(t *testing.T) {
			script, calls, state, restarts := deploymentHarness(t, service)
			out, err := exec.Command("bash", script, service, "production", testImage).CombinedOutput()
			if err != nil {
				t.Fatalf("healthy deployment failed: %v: %s", err, out)
			}
			for path, want := range map[string]string{state: "REELPIN_IMAGE=" + testImage + "\n", restarts: "1\n"} {
				body, err := os.ReadFile(path)
				if err != nil || string(body) != want {
					t.Fatalf("deployment state = %q, %v, want %q", body, err, want)
				}
			}
			commands, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(commands), "curl ") != (service == "api") {
				t.Fatalf("unexpected API smoke for %s: %s", service, commands)
			}
		})
	}
}

func TestDeployRejectsMalformedInputsBeforeMutation(t *testing.T) {
	for _, tc := range []struct {
		name, environment, image string
	}{
		{name: "environment path", environment: "./production", image: testImage},
		{name: "mutable tag", environment: "production", image: "ghcr.io/xploy04/reelpin-go:dev"},
		{name: "short digest", environment: "production", image: "ghcr.io/xploy04/reelpin-go@sha256:abc"},
		{name: "other repository", environment: "production", image: "example.com/api@sha256:" + strings.Repeat("a", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script, calls, state, _ := deploymentHarness(t, "api")
			out, err := exec.Command("bash", script, "api", tc.environment, tc.image).CombinedOutput()
			if err == nil {
				t.Fatalf("malformed deployment was accepted: %s", out)
			}
			commands, err := os.ReadFile(calls)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if len(commands) != 0 {
				t.Fatalf("malformed input reached host commands: %s", commands)
			}
			body, err := os.ReadFile(state)
			if err != nil || string(body) != "REELPIN_IMAGE="+previousImage+"\n" {
				t.Fatalf("malformed input changed the previous image: %q, %v", body, err)
			}
		})
	}
}

func TestDeployAllowsTheAlreadySelectedDigest(t *testing.T) {
	script, _, _, restarts := deploymentHarness(t, "api")
	out, err := exec.Command("bash", script, "api", "production", previousImage).CombinedOutput()
	if err != nil {
		t.Fatalf("same-digest deployment failed: %v: %s", err, out)
	}
	count, err := os.ReadFile(restarts)
	if err != nil || string(count) != "1\n" || strings.Contains(string(out), "rolling back") {
		t.Fatalf("same-digest deployment attempted rollback: %q, %v: %s", count, err, out)
	}
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
	deployment, err := os.ReadFile("reelpin-deploy.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(deployment), `reelpin-smoke.sh" http://127.0.0.1:8080`) {
		t.Fatal("deployment smoke must reach Go on 8080, not Python on 8000")
	}
	if strings.Contains(string(workflow), "- name: smoke test") {
		t.Fatal("API smoke must stay inside the deployment rollback boundary")
	}
	worker := strings.Index(string(workflow), "/tmp/reelpin-deploy.sh worker ")
	api := strings.Index(string(workflow), "/tmp/reelpin-deploy.sh api ")
	if worker < 0 || api < 0 || worker > api {
		t.Fatal("release must deploy the worker before the API's final smoke/rollback gate")
	}
	if !strings.Contains(string(workflow), "/tmp/reelpin-migrate.sh") {
		t.Fatal("release must use the gated, bounded migration wrapper")
	}
}
