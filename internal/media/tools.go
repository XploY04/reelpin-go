package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Tool timeouts. A download gets longer than a transcode because it is waiting
// on someone else's server.
const (
	DownloadTimeout = 120 * time.Second
	AudioTimeout    = 60 * time.Second
)

// Downloader fetches media for a URL.
type Downloader interface {
	Download(ctx context.Context, url, workDir string, options DownloadOptions) (Download, error)
}

type DownloadOptions struct {
	// CookieFile is a Netscape cookie file for an authenticated attempt. Empty
	// means anonymous, which is always tried first.
	CookieFile string
	// MaxBytes refuses a file larger than this before it fills the disk.
	MaxBytes int64
}

type Download struct {
	VideoPath string
	Title     string
	// Anonymous records whether the attempt used credentials, so cookie health
	// can be tracked without logging cookies.
	Anonymous bool
}

// YTDLP wraps the downloader binary.
type YTDLP struct {
	Binary string
	Runner Runner
}

func NewYTDLP(runner Runner) *YTDLP {
	if runner == nil {
		runner = ExecRunner{}
	}
	return &YTDLP{Binary: "yt-dlp", Runner: runner}
}

func (y *YTDLP) Download(ctx context.Context, url, workDir string, options DownloadOptions) (Download, error) {
	// The gate runs immediately before every spawn, not once upstream:
	// whatever path a URL took to get here, an unadmitted one never becomes a
	// process.
	if err := AdmitDownloadURL(ctx, url); err != nil {
		return Download{}, err
	}

	output := filepath.Join(workDir, "source.%(ext)s")

	args := []string{
		"--no-playlist",
		"--no-warnings",
		"--no-progress",
		"--restrict-filenames",
		// One format, already merged, so no post-processing step is needed.
		"-f", "mp4/best[ext=mp4]/best",
		"-o", output,
		"--print", "after_move:filepath",
		"--socket-timeout", "30",
		"--retries", "2",
	}
	if options.MaxBytes > 0 {
		args = append(args, "--max-filesize", fmt.Sprintf("%d", options.MaxBytes))
	}
	if options.CookieFile != "" {
		args = append(args, "--cookies", options.CookieFile)
	}
	// The URL goes last and as its own argument: it is user input.
	args = append(args, url)

	result, err := y.Runner.Run(ctx, Command{
		Name:    y.Binary,
		Args:    args,
		Dir:     workDir,
		Timeout: DownloadTimeout,
	})
	if err != nil {
		return Download{}, classifyDownload(err, result)
	}

	path := strings.TrimSpace(lastNonEmptyLine(result.Stdout))
	if path == "" {
		return Download{}, errors.New("the downloader reported no file")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workDir, path)
	}
	// The path came from a tool, but it still must stay inside the run's own
	// directory.
	if !withinDir(workDir, path) {
		return Download{}, fmt.Errorf("the downloader wrote outside the run directory")
	}
	if _, err := os.Stat(path); err != nil {
		return Download{}, fmt.Errorf("the downloaded file is missing: %w", err)
	}

	return Download{VideoPath: path, Anonymous: options.CookieFile == ""}, nil
}

// FFmpeg extracts a small mono audio track, which is all a transcript needs and
// a fraction of the bytes a model would otherwise be sent.
type FFmpeg struct {
	Binary string
	Runner Runner
}

func NewFFmpeg(runner Runner) *FFmpeg {
	if runner == nil {
		runner = ExecRunner{}
	}
	return &FFmpeg{Binary: "ffmpeg", Runner: runner}
}

func (f *FFmpeg) ExtractAudio(ctx context.Context, videoPath, workDir string) (string, error) {
	// ffmpeg accepts local files only, and only this job's own. It can fetch
	// URLs natively, which would bypass every network check in the service.
	if strings.Contains(videoPath, "://") {
		return "", fmt.Errorf("ffmpeg only reads local files, got %q", videoPath)
	}
	if !withinDir(workDir, videoPath) {
		return "", fmt.Errorf("ffmpeg only reads this job's own files")
	}

	audioPath := filepath.Join(workDir, "audio.mp3")

	result, err := f.Runner.Run(ctx, Command{
		Name: f.Binary,
		Args: []string{
			"-hide_banner", "-loglevel", "error",
			"-y",
			"-i", videoPath,
			"-vn",
			"-ac", "1",
			"-ar", "16000",
			"-b:a", "64k",
			audioPath,
		},
		Dir:     workDir,
		Timeout: AudioTimeout,
	})
	if err != nil {
		if strings.Contains(strings.ToLower(result.Stderr), "does not contain any stream") ||
			strings.Contains(strings.ToLower(result.Stderr), "audio stream") {
			return "", ErrNoAudioStream
		}
		return "", err
	}

	info, err := os.Stat(audioPath)
	if err != nil || info.Size() == 0 {
		return "", ErrNoAudioStream
	}
	return audioPath, nil
}

// ErrNoAudioStream is a video with nothing to transcribe. It is content's
// nature, not a failure to retry.
var ErrNoAudioStream = errors.New("the video has no audio stream")

// classifyDownload turns a tool's exit into something the pipeline can act on,
// without letting the tool's text reach a user.
func classifyDownload(err error, result Result) error {
	if errors.Is(err, ErrTimedOut) {
		return err
	}

	message := strings.ToLower(result.Stderr)
	switch {
	case strings.Contains(message, "429"), strings.Contains(message, "rate-limit"),
		strings.Contains(message, "rate limit"):
		return ErrRateLimited
	case strings.Contains(message, "login required"), strings.Contains(message, "sign in"),
		strings.Contains(message, "cookies"), strings.Contains(message, "authentication"):
		return ErrLoginRequired
	case strings.Contains(message, "not available"), strings.Contains(message, "removed"),
		strings.Contains(message, "404"), strings.Contains(message, "does not exist"):
		return ErrUnavailable
	case strings.Contains(message, "private"):
		return ErrPrivate
	case strings.Contains(message, "file is larger than max-filesize"):
		return ErrTooLarge
	}
	return err
}

// The download outcomes the handlers branch on.
var (
	ErrRateLimited   = errors.New("the platform is rate limiting downloads")
	ErrLoginRequired = errors.New("the platform requires an authenticated session")
	ErrUnavailable   = errors.New("the post is unavailable")
	ErrPrivate       = errors.New("the post is private")
	ErrTooLarge      = errors.New("the media is too large")
)

func lastNonEmptyLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

func withinDir(dir, path string) bool {
	absoluteDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(absoluteDir, absolutePath)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// Verify proves both binaries exist and answer before any work is accepted. A
// worker with a missing or broken tool must fail readiness with a reason a
// person can act on, not fail its first job with a confusing download error.
func Verify(ctx context.Context, runner Runner) error {
	if runner == nil {
		runner = ExecRunner{}
	}

	checks := []struct {
		name string
		args []string
	}{
		{"yt-dlp", []string{"--version"}},
		{"ffmpeg", []string{"-version"}},
	}
	for _, check := range checks {
		result, err := runner.Run(ctx, Command{
			Name:    check.name,
			Args:    check.args,
			Timeout: 10 * time.Second,
		})
		if err != nil {
			return fmt.Errorf("%s is not usable on this host: %w", check.name, err)
		}
		if lastNonEmptyLine(result.Stdout) == "" {
			return fmt.Errorf("%s answered with no version; the binary is broken", check.name)
		}
	}
	return nil
}
