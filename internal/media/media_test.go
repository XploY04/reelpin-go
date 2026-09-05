package media

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunPassesArgumentsWithoutAShell(t *testing.T) {
	// If arguments went through a shell, the semicolon would run a second
	// command and the output would not be the literal string.
	result, err := Run(context.Background(), Command{
		Name: "echo",
		Args: []string{"hello; echo injected"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(result.Stdout, "injected\n") && !strings.Contains(result.Stdout, "; echo injected") {
		t.Fatalf("the argument reached a shell: %q", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "hello; echo injected") {
		t.Fatalf("stdout = %q", result.Stdout)
	}
}

func TestRunReportsExitCodes(t *testing.T) {
	result, err := Run(context.Background(), Command{Name: "sh", Args: []string{"-c", "echo bad >&2; exit 3"}})
	if err == nil {
		t.Fatal("a failing command reported success")
	}
	if result.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", result.ExitCode)
	}
	if !strings.Contains(result.Stderr, "bad") {
		t.Errorf("stderr = %q", result.Stderr)
	}
}

func TestRunEnforcesItsDeadline(t *testing.T) {
	started := time.Now()
	_, err := Run(context.Background(), Command{
		Name:    "sh",
		Args:    []string{"-c", "sleep 30"},
		Timeout: 300 * time.Millisecond,
	})
	if !errors.Is(err, ErrTimedOut) {
		t.Fatalf("err = %v, want ErrTimedOut", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("the command ran for %s past its deadline", elapsed)
	}
}

func TestRunKillsTheWholeProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-alive")

	// The child outlives its parent unless the whole group is killed.
	_, err := Run(context.Background(), Command{
		Name:    "sh",
		Args:    []string{"-c", "(sleep 3; touch " + marker + ") & sleep 30"},
		Timeout: 300 * time.Millisecond,
	})
	if !errors.Is(err, ErrTimedOut) {
		t.Fatalf("err = %v, want ErrTimedOut", err)
	}

	time.Sleep(4 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("an orphaned child survived the timeout")
	}
}

func TestRunCapsOutput(t *testing.T) {
	result, err := Run(context.Background(), Command{
		Name: "sh",
		Args: []string{"-c", "yes abcdefghij | head -c 2000000"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Stdout) > MaxOutputBytes+200 {
		t.Fatalf("stdout is %d bytes, want it capped near %d", len(result.Stdout), MaxOutputBytes)
	}
	if !strings.Contains(result.Stdout, "more bytes dropped") {
		t.Error("the cap did not say that output was dropped")
	}
}

// fakeRunner replaces the binaries, so no test needs yt-dlp or a network.
type fakeRunner struct {
	result  Result
	err     error
	lastCmd Command
	onRun   func(Command) (Result, error)
}

func (f *fakeRunner) Run(_ context.Context, command Command) (Result, error) {
	f.lastCmd = command
	if f.onRun != nil {
		return f.onRun(command)
	}
	return f.result, f.err
}

// admitPublicly fakes DNS so admission tests never touch the network: every
// allowlisted host resolves to one public address.
func admitPublicly(t *testing.T) {
	t.Helper()
	previous := lookupIP
	lookupIP = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	t.Cleanup(func() { lookupIP = previous })
}

func TestDownloadReturnsThePathTheToolReports(t *testing.T) {
	admitPublicly(t)
	workDir := t.TempDir()
	videoPath := filepath.Join(workDir, "source.mp4")
	if err := os.WriteFile(videoPath, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := &fakeRunner{result: Result{Stdout: "some noise\n" + videoPath + "\n"}}
	download, err := NewYTDLP(runner).Download(context.Background(),
		"https://www.instagram.com/reel/C8abc123/", workDir, DownloadOptions{MaxBytes: 100 << 20})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if download.VideoPath != videoPath {
		t.Errorf("path = %q, want %q", download.VideoPath, videoPath)
	}
	if !download.Anonymous {
		t.Error("an attempt with no cookie file is anonymous")
	}

	// The URL is the last argument and never interpolated into a flag.
	args := runner.lastCmd.Args
	if args[len(args)-1] != "https://www.instagram.com/reel/C8abc123/" {
		t.Errorf("the url is not the final argument: %v", args)
	}
	if !contains(args, "--max-filesize") {
		t.Error("no size limit was passed")
	}
}

func TestDownloadRefusesAPathOutsideTheRunDirectory(t *testing.T) {
	admitPublicly(t)
	workDir := t.TempDir()
	escaped := filepath.Join(t.TempDir(), "elsewhere.mp4")
	if err := os.WriteFile(escaped, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := &fakeRunner{result: Result{Stdout: escaped}}
	if _, err := NewYTDLP(runner).Download(context.Background(), "https://www.instagram.com/reel/C8abc123/", workDir, DownloadOptions{}); err == nil {
		t.Fatal("a file outside the run directory was accepted")
	}
}

func TestDownloadClassifiesToolFailures(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   error
	}{
		{name: "rate limited", stderr: "ERROR: HTTP Error 429: Too Many Requests", want: ErrRateLimited},
		{name: "login", stderr: "ERROR: Login required to view this post", want: ErrLoginRequired},
		{name: "cookies", stderr: "ERROR: use --cookies to pass an authenticated session", want: ErrLoginRequired},
		{name: "gone", stderr: "ERROR: Video not available", want: ErrUnavailable},
		{name: "private", stderr: "ERROR: This account is private", want: ErrPrivate},
		{name: "too large", stderr: "File is larger than max-filesize", want: ErrTooLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			admitPublicly(t)
			runner := &fakeRunner{
				result: Result{Stderr: tt.stderr, ExitCode: 1},
				err:    errors.New("yt-dlp exited 1"),
			}
			_, err := NewYTDLP(runner).Download(context.Background(), "https://www.instagram.com/reel/C8abc123/", t.TempDir(), DownloadOptions{})
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestExtractAudioReportsAVideoWithNoSound(t *testing.T) {
	runner := &fakeRunner{
		result: Result{Stderr: "Output file does not contain any stream", ExitCode: 1},
		err:    errors.New("ffmpeg exited 1"),
	}
	workDir := t.TempDir()
	if _, err := NewFFmpeg(runner).ExtractAudio(context.Background(), filepath.Join(workDir, "video.mp4"), workDir); !errors.Is(err, ErrNoAudioStream) {
		t.Fatalf("err = %v, want ErrNoAudioStream", err)
	}
}

func TestExtractAudioProducesMonoSpeechAudio(t *testing.T) {
	workDir := t.TempDir()
	runner := &fakeRunner{onRun: func(command Command) (Result, error) {
		// Stand in for ffmpeg by writing the file it would have written.
		return Result{}, os.WriteFile(filepath.Join(workDir, "audio.mp3"), []byte("audio"), 0o600)
	}}

	path, err := NewFFmpeg(runner).ExtractAudio(context.Background(), filepath.Join(workDir, "video.mp4"), workDir)
	if err != nil {
		t.Fatalf("ExtractAudio: %v", err)
	}
	if path != filepath.Join(workDir, "audio.mp3") {
		t.Errorf("path = %q", path)
	}
	// Mono, 16 kHz: a transcript needs no more, and the model is billed by size.
	for _, want := range []string{"-vn", "-ac", "1", "-ar", "16000"} {
		if !contains(runner.lastCmd.Args, want) {
			t.Errorf("ffmpeg was not asked for %s: %v", want, runner.lastCmd.Args)
		}
	}
}

func TestExtractAudioTreatsAnEmptyFileAsNoAudio(t *testing.T) {
	workDir := t.TempDir()
	runner := &fakeRunner{onRun: func(Command) (Result, error) {
		return Result{}, os.WriteFile(filepath.Join(workDir, "audio.mp3"), nil, 0o600)
	}}
	if _, err := NewFFmpeg(runner).ExtractAudio(context.Background(), filepath.Join(workDir, "video.mp4"), workDir); !errors.Is(err, ErrNoAudioStream) {
		t.Fatalf("err = %v, want ErrNoAudioStream", err)
	}
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
