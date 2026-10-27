package cookies

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const netscapeFile = "# Netscape HTTP Cookie File\n.instagram.com\tTRUE\t/\tTRUE\t0\tsessionid\tsecret-value\n"

func encoded(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }

func TestSlotsAreOrderedAndValidated(t *testing.T) {
	jar := New(map[string]string{
		"active":   encoded(netscapeFile),
		"backup":   encoded(netscapeFile),
		"tertiary": "not base64 at all!!",
		// The deprecated single-slot variables are deliberately not read.
		"instagram_cookies": encoded(netscapeFile),
	})

	slots := jar.Slots()
	if len(slots) != 2 {
		t.Fatalf("slots = %d, want the two valid ones", len(slots))
	}
	if slots[0].Name != "active" || slots[1].Name != "backup" {
		t.Errorf("slots = %v, want active first", []string{slots[0].Name, slots[1].Name})
	}
}

func TestJSONCookiesAreRejected(t *testing.T) {
	jar := New(map[string]string{"active": encoded(`[{"name":"sessionid","value":"x"}]`)})
	if len(jar.Slots()) != 0 {
		t.Fatal("a JSON cookie export was accepted as a Netscape file")
	}
}

func TestWriteFileIsPrivateAndInsideTheRunDirectory(t *testing.T) {
	jar := New(map[string]string{"active": encoded(netscapeFile)})
	workDir := t.TempDir()

	path, err := jar.Slots()[0].WriteFile(workDir)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if filepath.Dir(path) != workDir {
		t.Errorf("cookie file at %q, want it inside the run directory", path)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("cookie file mode = %v, want 0600", info.Mode().Perm())
	}

	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "sessionid") {
		t.Error("the cookie file does not contain the cookies")
	}
}

func TestHealthRetiresARepeatedlyRefusedSlot(t *testing.T) {
	jar := New(map[string]string{"active": encoded(netscapeFile), "backup": encoded(netscapeFile)})

	for i := 0; i < 3; i++ {
		jar.RecordFailure(0)
	}
	slots := jar.Slots()
	if len(slots) != 1 || slots[0].Name != "backup" {
		t.Fatalf("slots = %+v, want the refused one retired", slots)
	}
	if jar.AllExhausted() {
		t.Error("one retired slot is not an outage while another works")
	}

	for i := 0; i < 3; i++ {
		jar.RecordFailure(1)
	}
	if !jar.AllExhausted() {
		t.Fatal("every slot is retired and that is not reported")
	}

	// A success brings a slot back.
	jar.RecordSuccess(0)
	if jar.AllExhausted() || len(jar.Slots()) != 1 {
		t.Errorf("a working slot was not restored: %+v", jar.Report())
	}
}

// Health is meant to be logged and served, so it must carry no cookie data.
func TestHealthReportCarriesNoCookies(t *testing.T) {
	jar := New(map[string]string{"active": encoded(netscapeFile)})
	jar.RecordSuccess(0)

	for _, health := range jar.Report() {
		rendered := strings.ToLower(health.Name)
		if strings.Contains(rendered, "sessionid") || strings.Contains(rendered, "secret-value") {
			t.Fatal("the health report leaks cookie data")
		}
	}
	if len(jar.Report()) != 1 || jar.Report()[0].Successes != 1 {
		t.Errorf("report = %+v", jar.Report())
	}
}

func TestNoSlotsIsNotAnOutage(t *testing.T) {
	jar := New(nil)
	if len(jar.Slots()) != 0 {
		t.Fatal("slots appeared from nowhere")
	}
	if jar.AllExhausted() {
		t.Error("a deployment with no cookies configured is not an exhausted one")
	}
}
