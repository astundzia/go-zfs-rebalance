// Package zfs inspects ZFS pools and datasets by running the zfs and zpool
// command-line tools: which dataset and pool a folder lives on, how full each
// vdev is, and a few properties used for safety checks before a rebalance.
package zfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	// ErrNotZFS means the zfs tools say the path is not on a ZFS dataset, or
	// they can't reach the ZFS kernel module (as inside some containers).
	ErrNotZFS = errors.New("not on a ZFS pool")

	// ErrToolsMissing means the zfs or zpool command couldn't be found, on
	// PATH or in the usual sbin folders. It says nothing about whether a path
	// is on ZFS; use OnZFS for that.
	ErrToolsMissing = errors.New("the zfs and zpool commands weren't found")

	// ErrExecBlocked means the system refused to start the zfs or zpool
	// command with ENOSYS. TrueNAS does this once the sudo session that
	// started the run has ended (its sudo logs every command a session
	// starts, and stops them all from starting when it goes away).
	ErrExecBlocked = errors.New("the system won't let this run start other programs")
)

// sbinDirs are where zfs and zpool usually live. They are tried when a command
// isn't on PATH, as for cron jobs or normal users on Debian.
var sbinDirs = []string{"/usr/sbin", "/sbin", "/usr/local/sbin"}

// defaultPollInterval is how often WaitForFrees re-checks the pool.
const defaultPollInterval = 2 * time.Second

// maxStderrLines caps how much of a failing command's stderr goes into an
// error; zpool prints its whole usage text after some errors.
const maxStderrLines = 5

// Runner runs an external command and returns its standard output. A failed
// command's error should include its trimmed standard error text.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// CommandError is a command run by ExecRunner that failed.
type CommandError struct {
	Command string // the command line, such as "zpool list -v tank"
	Stderr  string // the first lines it wrote to standard error, joined into one line
	Err     error  // why it failed, such as an *exec.ExitError or *exec.Error
}

// Error is the command line, what it wrote to standard error, and why it failed.
func (e *CommandError) Error() string {
	if e.Stderr != "" {
		return fmt.Sprintf("%s: %s (%v)", e.Command, e.Stderr, e.Err)
	}
	return fmt.Sprintf("%s: %v", e.Command, e.Err)
}

// Unwrap returns why the command failed.
func (e *CommandError) Unwrap() error { return e.Err }

// ExecRunner runs commands with os/exec in the C locale, so their output is
// stable regardless of the user's language settings. A command that isn't on
// PATH is also looked for in /usr/sbin, /sbin and /usr/local/sbin.
type ExecRunner struct{}

// Run executes name with args and returns its standard output. The command is
// killed if ctx is cancelled. A failure is returned as a *CommandError.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, findCommand(name), args...)
	// Later entries win, so these override the user's locale.
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &CommandError{
			Command: strings.Join(append([]string{name}, args...), " "),
			Stderr:  summarizeStderr(stderr.String()),
			Err:     err,
		}
	}
	return stdout.Bytes(), nil
}

// findCommand returns what to run for name: name itself when it is a path or
// on PATH, otherwise the first executable of that name in sbinDirs, and name
// again when there is none, so that running it reports exec.ErrNotFound.
func findCommand(name string) string {
	if strings.Contains(name, "/") {
		return name
	}
	if _, err := exec.LookPath(name); err == nil {
		return name
	}
	for _, dir := range sbinDirs {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p
		}
	}
	return name
}

// summarizeStderr joins the first few non-empty lines of s into one line.
func summarizeStderr(s string) string {
	var lines []string
	for line := range strings.Lines(s) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(lines) == maxStderrLines {
			lines = append(lines, "...")
			break
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, " ")
}

// Client runs zfs and zpool commands through Runner (ExecRunner if nil).
type Client struct {
	Runner Runner

	// pollInterval overrides defaultPollInterval in tests.
	pollInterval time.Duration
}

// NewClient returns a Client that runs the real zfs and zpool binaries.
func NewClient() *Client {
	return &Client{Runner: ExecRunner{}}
}

// run executes a command, reporting cancellation as ctx's error, a missing
// command as ErrToolsMissing and a refused start as ErrExecBlocked.
func (c *Client) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r := c.Runner
	if r == nil {
		r = ExecRunner{}
	}
	out, err := r.Run(ctx, name, args...)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		switch {
		case errors.Is(err, exec.ErrNotFound):
			return nil, fmt.Errorf("%w: %w", ErrToolsMissing, err)
		case errors.Is(err, syscall.ENOSYS):
			return nil, fmt.Errorf("%w: %w", ErrExecBlocked, err)
		}
		return nil, err
	}
	return out, nil
}

// notZFSMarkers are fragments of libzfs messages that mean "this isn't ZFS"
// rather than a real failure: zfs_path_to_zhandle() for a non-ZFS mount, and
// libzfs_error_init() when the kernel module or /dev/zfs is unavailable.
var notZFSMarkers = []string{
	"not a zfs filesystem",
	"zfs modules",
	"/dev/zfs",
	"libzfs library",
}

func isNotZFSMessage(msg string) bool {
	msg = strings.ToLower(msg)
	for _, m := range notZFSMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// DatasetForPath returns the name of the ZFS dataset that contains path, such
// as "tank/media". It returns an error wrapping ErrNotZFS when zfs says the
// path is not on ZFS, ErrToolsMissing when zfs isn't installed, and
// ErrExecBlocked when it can't be started.
func (c *Client) DatasetForPath(ctx context.Context, path string) (string, error) {
	// zfs treats an argument that doesn't start with "/" as a dataset name.
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	out, err := c.run(ctx, "zfs", "list", "-H", "-o", "name", "--", abs)
	if err != nil {
		if !errors.Is(err, ErrToolsMissing) && !errors.Is(err, ErrExecBlocked) && isNotZFSMessage(err.Error()) {
			return "", fmt.Errorf("%w: %w", ErrNotZFS, err)
		}
		return "", err
	}
	name, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if name == "" {
		return "", fmt.Errorf("%w: zfs found no dataset for %s", ErrNotZFS, abs)
	}
	return name, nil
}

// PoolOf returns the pool part of a dataset or snapshot name: "tank" for
// "tank/media/photos".
func PoolOf(dataset string) string {
	if i := strings.IndexAny(dataset, "/@#"); i >= 0 {
		return dataset[:i]
	}
	return dataset
}

// Distribution reads the current size and allocation of every vdev in pool.
func (c *Client) Distribution(ctx context.Context, pool string) (Distribution, error) {
	out, err := c.run(ctx, "zpool", "list", "-v", "-H", "-p", "-o", "name,size,allocated,free", pool)
	if err != nil {
		return Distribution{}, err
	}
	d, err := ParseZpoolList(out)
	if err != nil {
		return Distribution{}, err
	}
	if d.Pool != pool {
		return Distribution{}, fmt.Errorf("zpool list: asked about pool %q but got %q", pool, d.Pool)
	}
	return d, nil
}

// WaitForFrees lets ZFS finish releasing freed space in pool, so a vdev report
// taken right after a run shows the old copies' space as free. It runs
// `zpool sync` (which also waits out the transaction groups that hold freed
// blocks back), then checks the pool's "freeing" property (space still being
// released from destroyed datasets and snapshots) every two seconds until it
// is zero. timeout bounds the whole wait, including the sync; when it runs
// out, WaitForFrees returns timedOut=true and no error. A cancelled ctx
// returns ctx's error.
func (c *Client) WaitForFrees(ctx context.Context, pool string, timeout time.Duration) (timedOut bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// stopped reports why waitCtx ended, if it has.
	stopped := func() (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		return waitCtx.Err() != nil, nil
	}

	if _, err := c.run(waitCtx, "zpool", "sync", pool); err != nil {
		if timedOut, ctxErr := stopped(); timedOut || ctxErr != nil {
			return timedOut, ctxErr
		}
		return false, err
	}

	interval := c.pollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		out, err := c.run(waitCtx, "zpool", "get", "-Hp", "-o", "value", "freeing", pool)
		if err != nil {
			if timedOut, ctxErr := stopped(); timedOut || ctxErr != nil {
				return timedOut, ctxErr
			}
			return false, err
		}
		freeing, err := parseOptionalUint(strings.TrimSpace(string(out)))
		if err != nil {
			return false, fmt.Errorf("zpool get freeing: %w", err)
		}
		if freeing == 0 {
			return false, nil
		}
		select {
		case <-waitCtx.Done():
			return stopped()
		case <-ticker.C:
		}
	}
}

// SnapshotBytes returns the space held only by snapshots of dataset and all
// of its children (the sum of their usedbysnapshots).
func (c *Client) SnapshotBytes(ctx context.Context, dataset string) (uint64, error) {
	out, err := c.run(ctx, "zfs", "get", "-Hp", "-r", "-o", "value", "usedbysnapshots", dataset)
	if err != nil {
		return 0, err
	}
	var total uint64
	for line := range strings.Lines(string(out)) {
		// Snapshots themselves report "-" for this property.
		n, err := parseOptionalUint(strings.TrimSpace(line))
		if err != nil {
			return 0, fmt.Errorf("zfs get usedbysnapshots: %w", err)
		}
		total += n
	}
	return total, nil
}

// HasSnapshots reports whether dataset or any of its children has a snapshot.
func (c *Client) HasSnapshots(ctx context.Context, dataset string) (bool, error) {
	out, err := c.run(ctx, "zfs", "list", "-H", "-t", "snapshot", "-r", "-o", "name", "-s", "name", dataset)
	if err != nil {
		return false, err
	}
	return len(bytes.TrimSpace(out)) > 0, nil
}

// DatasetDedup is one dataset's dedup setting, and where it is mounted.
type DatasetDedup struct {
	Name       string // such as "tank/media"
	Dedup      string // such as "off", "on" or "sha256,verify"
	Mountpoint string // such as "/mnt/tank/media", "legacy" or "none"
	Mounted    bool
}

// Dedup returns the dedup setting of dataset and of every filesystem below it,
// in the order zfs lists them (each parent before its children).
func (c *Client) Dedup(ctx context.Context, dataset string) ([]DatasetDedup, error) {
	out, err := c.run(ctx, "zfs", "get", "-r", "-H", "-t", "filesystem", "-o", "name,property,value", "dedup,mountpoint,mounted", dataset)
	if err != nil {
		return nil, err
	}
	var list []DatasetDedup
	index := make(map[string]int)
	for line := range strings.Lines(string(out)) {
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			return nil, fmt.Errorf("zfs get dedup: unexpected line %q", line)
		}
		name, prop, value := fields[0], fields[1], fields[2]
		i, ok := index[name]
		if !ok {
			i = len(list)
			index[name] = i
			list = append(list, DatasetDedup{Name: name})
		}
		switch prop {
		case "dedup":
			list[i].Dedup = value
		case "mountpoint":
			list[i].Mountpoint = value
		case "mounted":
			list[i].Mounted = value == "yes"
		}
	}
	return list, nil
}

// Available returns the free space, in bytes, that dataset can still use.
func (c *Client) Available(ctx context.Context, dataset string) (uint64, error) {
	out, err := c.run(ctx, "zfs", "get", "-Hp", "-o", "value", "available", dataset)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("zfs get available: %w", err)
	}
	return n, nil
}

// BcloneUsed returns how many bytes of pool are shared through block cloning.
// ok is false when this version of ZFS has no bcloneused property (before
// OpenZFS 2.2).
func (c *Client) BcloneUsed(ctx context.Context, pool string) (used uint64, ok bool, err error) {
	out, err := c.run(ctx, "zpool", "get", "-Hp", "-o", "value", "bcloneused", pool)
	if err != nil {
		// libzfs addlist(): "bad property list: invalid property 'bcloneused'".
		if msg := err.Error(); strings.Contains(msg, "invalid property") || strings.Contains(msg, "bad property list") {
			return 0, false, nil
		}
		return 0, false, err
	}
	value := strings.TrimSpace(string(out))
	if value == "-" {
		return 0, false, nil
	}
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("zpool get bcloneused: %w", err)
	}
	return n, true, nil
}

// parseOptionalUint parses a -p (exact) numeric value, where "-" or an empty
// string means "not applicable" and counts as zero.
func parseOptionalUint(s string) (uint64, error) {
	if s == "-" || s == "" {
		return 0, nil
	}
	return strconv.ParseUint(s, 10, 64)
}
