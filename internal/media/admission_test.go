package media

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdmissionAllowsOnlyDownloadPlatforms(t *testing.T) {
	admitPublicly(t)

	allowed := []string{
		"https://www.instagram.com/reel/C8abc123/",
		"https://youtu.be/dQw4w9WgXcQ",
		"https://vm.tiktok.com/ZM12345/",
	}
	for _, rawURL := range allowed {
		if err := AdmitDownloadURL(context.Background(), rawURL); err != nil {
			t.Errorf("%s was refused: %v", rawURL, err)
		}
	}

	refused := []string{
		"https://example.com/video.mp4",          // not a download platform
		"http://www.instagram.com/reel/C8/",      // not https
		"https://user:pass@www.instagram.com/x",  // credentials
		"https://www.instagram.com:8443/reel/C8", // odd port
		"ftp://www.instagram.com/reel/C8",        // wrong scheme
		"https://evil.com/?www.instagram.com",    // host is what matters
	}
	for _, rawURL := range refused {
		if err := AdmitDownloadURL(context.Background(), rawURL); !errors.Is(err, ErrNotAdmitted) {
			t.Errorf("%s err = %v, want ErrNotAdmitted", rawURL, err)
		}
	}
}

func TestAdmissionChecksEveryResolvedAddress(t *testing.T) {
	tests := []struct {
		name      string
		addresses []string
		admitted  bool
	}{
		{"public only", []string{"93.184.216.34"}, true},
		{"private", []string{"10.0.0.5"}, false},
		{"loopback", []string{"127.0.0.1"}, false},
		{"metadata", []string{"169.254.169.254"}, false},
		// A mixed answer is the rebinding shape: one good address for the
		// checker, one bad one for the fetch. Any bad address refuses the lot.
		{"mixed", []string{"93.184.216.34", "192.168.1.10"}, false},
		{"ipv6 private", []string{"fd00::1"}, false},
		{"ipv6 public", []string{"2606:2800:220:1:248:1893:25c8:1946"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			previous := lookupIP
			lookupIP = func(context.Context, string, string) ([]netip.Addr, error) {
				answers := make([]netip.Addr, 0, len(tt.addresses))
				for _, address := range tt.addresses {
					answers = append(answers, netip.MustParseAddr(address))
				}
				return answers, nil
			}
			t.Cleanup(func() { lookupIP = previous })

			err := AdmitDownloadURL(context.Background(), "https://www.instagram.com/reel/C8abc123/")
			if tt.admitted && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tt.admitted && !errors.Is(err, ErrNotAdmitted) {
				t.Fatalf("err = %v, want ErrNotAdmitted", err)
			}
		})
	}
}

func TestProbeRefusesLongAndOversizedMediaBeforeDownloading(t *testing.T) {
	admitPublicly(t)

	tests := []struct {
		name     string
		stdout   string
		want     error
		duration int
	}{
		{"within caps", "300\n1048576\n", nil, 300},
		{"too long", fmt.Sprintf("%d\n1048576\n", MaxDurationSeconds+1), ErrTooLong, MaxDurationSeconds + 1},
		{"too large", fmt.Sprintf("300\n%d\n", int64(MaxMediaBytes)+1), ErrTooLarge, 300},
		{"unknown values pass to the post-download check", "NA\nNA\n", nil, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{result: Result{Stdout: tt.stdout}}
			duration, _, err := NewYTDLP(runner).Probe(context.Background(), "https://www.instagram.com/reel/C8abc123/")
			if tt.want == nil && err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if duration != tt.duration {
				t.Errorf("duration = %d, want %d", duration, tt.duration)
			}
			if !contains(runner.lastCmd.Args, "--skip-download") {
				t.Error("the probe downloaded")
			}
		})
	}
}

func TestProbeNeverSpawnsForAnUnadmittedURL(t *testing.T) {
	runner := &fakeRunner{}
	_, _, err := NewYTDLP(runner).Probe(context.Background(), "https://example.com/v")
	if !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("err = %v", err)
	}
	if runner.lastCmd.Name != "" {
		t.Fatal("the tool ran for a URL that was never admitted")
	}
}

func TestSniffKindReadsBytesNotNames(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, content []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	mp4 := append([]byte{0, 0, 0, 0x18}, []byte("ftypmp42more")...)
	tests := []struct {
		path string
		want Kind
	}{
		{write("video.mp4", mp4), KindMP4},
		{write("photo.jpg", append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, make([]byte, 12)...)), KindJPEG},
		{write("image.png", append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, make([]byte, 8)...)), KindPNG},
		{write("anim.gif", append([]byte("GIF89a"), make([]byte, 10)...)), KindGIF},
		{write("pic.webp", append([]byte("RIFF\x00\x00\x00\x00WEBP"), make([]byte, 4)...)), KindWebP},
		// The false-MIME case: named like a video, is actually a login page.
		{write("fake.mp4", []byte("<html><body>Log in to continue</body></html>")), KindUnknown},
	}
	for _, tt := range tests {
		kind, err := SniffKind(tt.path)
		if err != nil {
			t.Fatalf("SniffKind(%s): %v", tt.path, err)
		}
		if kind != tt.want {
			t.Errorf("SniffKind(%s) = %s, want %s", filepath.Base(tt.path), kind, tt.want)
		}
	}

	if _, err := SniffKind(write("tiny.bin", []byte("x"))); err == nil {
		t.Error("a one-byte file was identified")
	}
}

func TestChildrenNeverInheritProxies(t *testing.T) {
	env := []string{"HTTP_PROXY=http://proxy:3128", "https_proxy=http://proxy:3128", "PATH=/usr/bin", "ALL_PROXY=socks5://x"}
	cleaned := scrubProxies(env)
	joined := strings.Join(cleaned, " ")
	if strings.Contains(strings.ToLower(joined), "proxy") {
		t.Fatalf("a proxy variable survived: %v", cleaned)
	}
	if !strings.Contains(joined, "PATH=/usr/bin") {
		t.Fatal("scrubbing removed a normal variable")
	}
}

func TestRunScrubsTheRealEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://smuggle:3128")

	// `env` prints its environment; the child must not see the proxy.
	result, err := Run(context.Background(), Command{Name: "env"})
	if err != nil {
		t.Fatalf("running env: %v", err)
	}
	if strings.Contains(result.Stdout, "smuggle") {
		t.Fatal("the child saw HTTP_PROXY")
	}
}

func TestVerifyNamesTheBrokenTool(t *testing.T) {
	missing := &fakeRunner{err: errors.New("exec: \"yt-dlp\": executable file not found in $PATH")}
	err := Verify(context.Background(), missing)
	if err == nil || !strings.Contains(err.Error(), "yt-dlp") {
		t.Fatalf("err = %v, want the tool named", err)
	}

	healthy := &fakeRunner{result: Result{Stdout: "2026.08.19\n"}}
	if err := Verify(context.Background(), healthy); err != nil {
		t.Fatalf("a healthy toolchain failed verification: %v", err)
	}

	silent := &fakeRunner{result: Result{}}
	if err := Verify(context.Background(), silent); err == nil {
		t.Fatal("a binary that answers nothing passed verification")
	}
}
