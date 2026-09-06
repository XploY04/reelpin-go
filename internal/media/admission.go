package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/safehttp"
)

// The hard limits every job lives under. Enforced before provider work, so an
// oversized post costs a metadata probe, never a download or a model call.
const (
	MaxHTMLBytes       = 5 << 20
	MaxImageBytes      = 20 << 20
	MaxMediaBytes      = 500 << 20
	MaxDurationSeconds = 30 * 60
	MaxJobBytes        = 1 << 30
)

// ErrNotAdmitted is a URL the downloader refuses to touch: wrong host,
// credentials in the URL, or a non-public address. It is terminal for the
// attempt, never retried.
var ErrNotAdmitted = errors.New("the downloader does not accept this URL")

// ErrTooLong is media past the duration cap, known before downloading.
var ErrTooLong = errors.New("the media is longer than the duration cap")

// downloadHosts is the explicit allowlist. yt-dlp reaches the network with a
// URL a user chose, so it only ever gets hosts a platform handler owns;
// everything else downloads through safehttp, which does its own checking.
var downloadHosts = map[string]bool{
	"instagram.com":     true,
	"www.instagram.com": true,
	"instagr.am":        true,
	"youtube.com":       true,
	"www.youtube.com":   true,
	"m.youtube.com":     true,
	"youtu.be":          true,
	"tiktok.com":        true,
	"www.tiktok.com":    true,
	"vm.tiktok.com":     true,
	"vt.tiktok.com":     true,
}

// lookupIP is a seam so admission tests never touch real DNS.
var lookupIP = net.DefaultResolver.LookupNetIP

// AdmitDownloadURL is the gate in front of every yt-dlp spawn. The process
// itself cannot be address-checked per connection the way safehttp is, so the
// URL is checked as hard as possible before the process exists: allowlisted
// host, https only, no credentials, and every resolved address public. A
// network-level egress boundary on the worker host is the deployment half of
// this defence.
func AdmitDownloadURL(ctx context.Context, rawURL string) error {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("%w: not a URL", ErrNotAdmitted)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("%w: scheme %q", ErrNotAdmitted, parsed.Scheme)
	}
	if parsed.User != nil {
		return fmt.Errorf("%w: credentials in the URL", ErrNotAdmitted)
	}
	host := strings.ToLower(parsed.Hostname())
	if !downloadHosts[host] {
		return fmt.Errorf("%w: host %q is not a download platform", ErrNotAdmitted, host)
	}
	if port := parsed.Port(); port != "" && port != "443" {
		return fmt.Errorf("%w: port %s", ErrNotAdmitted, port)
	}

	addresses, err := lookupIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return fmt.Errorf("%w: %s does not resolve", ErrNotAdmitted, host)
	}
	for _, address := range addresses {
		if !safehttp.IsPublicAddress(address.Unmap()) {
			return fmt.Errorf("%w: %s resolves to a non-public address", ErrNotAdmitted, host)
		}
	}
	return nil
}

// Probe asks yt-dlp for duration and approximate size without downloading, so
// the caps reject a two-hour video before a byte moves. A platform that
// reports nothing gets zero values, and the post-download checks still hold.
func (y *YTDLP) Probe(ctx context.Context, rawURL string) (durationSeconds int, approxBytes int64, err error) {
	if err := AdmitDownloadURL(ctx, rawURL); err != nil {
		return 0, 0, err
	}

	result, err := y.Runner.Run(ctx, Command{
		Name: y.Binary,
		Args: []string{
			"--no-playlist", "--no-warnings", "--skip-download",
			"--print", "duration",
			"--print", "filesize_approx",
			"--socket-timeout", "30",
			rawURL,
		},
		Timeout: 45 * time.Second,
	})
	if err != nil {
		return 0, 0, classifyDownload(err, result)
	}

	lines := strings.Fields(result.Stdout)
	if len(lines) > 0 {
		durationSeconds = int(parseFloat(lines[0]))
	}
	if len(lines) > 1 {
		approxBytes = int64(parseFloat(lines[1]))
	}

	if durationSeconds > MaxDurationSeconds {
		return durationSeconds, approxBytes,
			fmt.Errorf("%w: %ds against a cap of %ds", ErrTooLong, durationSeconds, MaxDurationSeconds)
	}
	if approxBytes > MaxMediaBytes {
		return durationSeconds, approxBytes,
			fmt.Errorf("%w: about %d bytes against a cap of %d", ErrTooLarge, approxBytes, MaxMediaBytes)
	}
	return durationSeconds, approxBytes, nil
}

// parseFloat reads yt-dlp's numeric prints, which may be "NA", an int or a
// float. Unknown reads as zero: the caps then rely on the post-download check.
func parseFloat(value string) float64 {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "na") || strings.EqualFold(value, "none") {
		return 0
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0
	}
	return parsed
}

// Kind is what a file's own bytes say it is.
type Kind string

const (
	KindMP4     Kind = "mp4"
	KindJPEG    Kind = "jpeg"
	KindPNG     Kind = "png"
	KindGIF     Kind = "gif"
	KindWebP    Kind = "webp"
	KindUnknown Kind = "unknown"
)

// SniffKind reads a file's magic bytes. The Content-Type header and the file
// extension are both claims someone else made; a file that is about to be
// handed to ffmpeg or a model gets identified by its own bytes.
func SniffKind(path string) (Kind, error) {
	file, err := os.Open(path)
	if err != nil {
		return KindUnknown, err
	}
	defer file.Close()

	header := make([]byte, 16)
	read, err := file.Read(header)
	if err != nil || read < 12 {
		return KindUnknown, fmt.Errorf("the file is too small to identify")
	}
	header = header[:read]

	switch {
	case bytes.Equal(header[4:8], []byte("ftyp")):
		return KindMP4, nil
	case bytes.HasPrefix(header, []byte{0xFF, 0xD8, 0xFF}):
		return KindJPEG, nil
	case bytes.HasPrefix(header, []byte{0x89, 'P', 'N', 'G'}):
		return KindPNG, nil
	case bytes.HasPrefix(header, []byte("GIF8")):
		return KindGIF, nil
	case bytes.HasPrefix(header, []byte("RIFF")) && bytes.Equal(header[8:12], []byte("WEBP")):
		return KindWebP, nil
	}
	return KindUnknown, nil
}

// checkAddress exists for tests that need a single address decision.
func checkAddress(address netip.Addr) bool {
	return safehttp.IsPublicAddress(address)
}
