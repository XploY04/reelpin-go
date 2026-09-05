// Package media runs the external tools that turn a post into files: yt-dlp for
// downloads and ffmpeg for audio. Everything here is bounded on purpose. These
// commands take a URL a user chose, so they get a deadline, an output cap, and
// a process group that dies with them.
package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ErrTimedOut is a command that ran past its deadline.
var ErrTimedOut = errors.New("command timed out")

// Limits keep one download from taking the worker with it.
const (
	MaxOutputBytes = 256 << 10
	DefaultTimeout = 90 * time.Second
)

type Command struct {
	Name    string
	Args    []string
	Dir     string
	Env     []string
	Timeout time.Duration
}

type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
}

// Run executes one command. Arguments are passed as an array and never as
// shell text, so a URL cannot become a command.
func Run(ctx context.Context, command Command) (Result, error) {
	if command.Timeout <= 0 {
		command.Timeout = DefaultTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, command.Timeout)
	defer cancel()

	process := exec.CommandContext(ctx, command.Name, command.Args...)
	process.Dir = command.Dir
	process.Env = command.Env

	// Its own process group, so killing it kills whatever it spawned rather
	// than leaving orphans holding the disk.
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	process.Cancel = func() error {
		if process.Process == nil {
			return nil
		}
		return syscall.Kill(-process.Process.Pid, syscall.SIGKILL)
	}
	process.WaitDelay = 5 * time.Second

	var stdout, stderr cappedBuffer
	process.Stdout = &stdout
	process.Stderr = &stderr

	started := time.Now()
	err := process.Run()
	result := Result{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(started),
	}

	if ctx.Err() == context.DeadlineExceeded {
		return result, fmt.Errorf("%w after %s: %s", ErrTimedOut, command.Timeout, command.Name)
	}

	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		result.ExitCode = exitError.ExitCode()
		return result, fmt.Errorf("%s exited %d: %s", command.Name, result.ExitCode, lastLine(result.Stderr))
	}
	if err != nil {
		return result, fmt.Errorf("running %s: %w", command.Name, err)
	}
	return result, nil
}

// Runner is the seam tests replace, so nothing here shells out in a unit test.
type Runner interface {
	Run(ctx context.Context, command Command) (Result, error)
}

// ExecRunner runs commands for real.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, command Command) (Result, error) {
	return Run(ctx, command)
}

// cappedBuffer keeps a chatty tool from filling memory. What is dropped is
// output nobody reads anyway; the tail is what carries the error.
type cappedBuffer struct {
	buffer  bytes.Buffer
	dropped int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	remaining := MaxOutputBytes - c.buffer.Len()
	if remaining <= 0 {
		c.dropped += len(p)
		return len(p), nil
	}
	if len(p) > remaining {
		c.buffer.Write(p[:remaining])
		c.dropped += len(p) - remaining
		return len(p), nil
	}
	return c.buffer.Write(p)
}

func (c *cappedBuffer) String() string {
	if c.dropped == 0 {
		return c.buffer.String()
	}
	return fmt.Sprintf("%s\n[%d more bytes dropped]", c.buffer.String(), c.dropped)
}

func lastLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 {
		return ""
	}
	last := strings.TrimSpace(lines[len(lines)-1])
	if len(last) > 300 {
		return last[:300]
	}
	return last
}

var _ io.Writer = (*cappedBuffer)(nil)
