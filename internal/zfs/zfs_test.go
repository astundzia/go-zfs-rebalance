package zfs

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeRunner records every command and answers with respond.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []string
	respond func(cmdline string) ([]byte, error)
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	cmdline := strings.Join(append([]string{name}, args...), " ")
	f.mu.Lock()
	f.calls = append(f.calls, cmdline)
	f.mu.Unlock()
	return f.respond(cmdline)
}

func (f *fakeRunner) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// answer returns a fakeRunner that maps exact command lines to output; any
// other command fails the test.
func answer(t *testing.T, outputs map[string]string) *fakeRunner {
	return &fakeRunner{respond: func(cmdline string) ([]byte, error) {
		out, ok := outputs[cmdline]
		if !ok {
			t.Errorf("unexpected command %q", cmdline)
			return nil, errors.New("unexpected command")
		}
		return []byte(out), nil
	}}
}

// failWith returns a fakeRunner whose every command fails with err.
func failWith(err error) *fakeRunner {
	return &fakeRunner{respond: func(string) ([]byte, error) { return nil, err }}
}

func TestDatasetForPath(t *testing.T) {
	ctx := context.Background()

	t.Run("ok", func(t *testing.T) {
		r := answer(t, map[string]string{"zfs list -H -o name -- /mnt/tank/media": "tank/media\n"})
		ds, err := (&Client{Runner: r}).DatasetForPath(ctx, "/mnt/tank/media")
		require.NoError(t, err)
		require.Equal(t, "tank/media", ds)
	})

	t.Run("not ZFS", func(t *testing.T) {
		// Real stderr from zfs_path_to_zhandle() for a non-ZFS mount.
		r := failWith(errors.New("zfs list -H -o name -- /tmp: '/tmp': not a ZFS filesystem (exit status 1)"))
		_, err := (&Client{Runner: r}).DatasetForPath(ctx, "/tmp")
		require.ErrorIs(t, err, ErrNotZFS)
	})

	t.Run("module not loaded", func(t *testing.T) {
		r := failWith(errors.New("zfs list: The ZFS modules are not loaded. Try running 'modprobe zfs' as root to load them. (exit status 1)"))
		_, err := (&Client{Runner: r}).DatasetForPath(ctx, "/mnt/tank")
		require.ErrorIs(t, err, ErrNotZFS)
	})

	t.Run("zfs missing", func(t *testing.T) {
		r := failWith(&exec.Error{Name: "zfs", Err: exec.ErrNotFound})
		_, err := (&Client{Runner: r}).DatasetForPath(ctx, "/mnt/tank")
		require.ErrorIs(t, err, ErrNotZFS)
		require.ErrorIs(t, err, exec.ErrNotFound)
	})

	t.Run("zfs missing for real", func(t *testing.T) {
		c := &Client{Runner: ExecRunner{}}
		_, err := c.run(ctx, "go-zfs-rebalance-no-such-command", "list")
		require.ErrorIs(t, err, ErrNotZFS)
	})

	t.Run("other failure is not ErrNotZFS", func(t *testing.T) {
		r := failWith(errors.New("zfs list: out of memory (exit status 1)"))
		_, err := (&Client{Runner: r}).DatasetForPath(ctx, "/mnt/tank")
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrNotZFS)
	})

	t.Run("empty output", func(t *testing.T) {
		r := answer(t, map[string]string{"zfs list -H -o name -- /mnt/tank": ""})
		_, err := (&Client{Runner: r}).DatasetForPath(ctx, "/mnt/tank")
		require.ErrorIs(t, err, ErrNotZFS)
	})
}

func TestPoolOf(t *testing.T) {
	tests := map[string]string{
		"tank":                 "tank",
		"tank/media":           "tank",
		"tank/media/photos":    "tank",
		"tank@daily":           "tank",
		"tank/media@daily-001": "tank",
		"tank#bookmark":        "tank",
		"":                     "",
	}
	for in, want := range tests {
		require.Equal(t, want, PoolOf(in), "PoolOf(%q)", in)
	}
}

func TestClientDistribution(t *testing.T) {
	ctx := context.Background()
	fixture := string(readFixture(t, "classes.txt"))

	r := answer(t, map[string]string{"zpool list -v -H -p -o name,size,allocated,free tank": fixture})
	d, err := (&Client{Runner: r}).Distribution(ctx, "tank")
	require.NoError(t, err)
	require.Equal(t, Distribution{Pool: "tank", Vdevs: classesVdevs}, d)

	r = answer(t, map[string]string{"zpool list -v -H -p -o name,size,allocated,free other": fixture})
	_, err = (&Client{Runner: r}).Distribution(ctx, "other")
	require.ErrorContains(t, err, `"tank"`)
}

// freeingRunner answers `zpool sync` and reports each value of freeing in
// turn, repeating the last one.
func freeingRunner(t *testing.T, freeing ...string) *fakeRunner {
	var mu sync.Mutex
	return &fakeRunner{respond: func(cmdline string) ([]byte, error) {
		switch cmdline {
		case "zpool sync tank":
			return nil, nil
		case "zpool get -Hp -o value freeing tank":
			mu.Lock()
			defer mu.Unlock()
			v := freeing[0]
			if len(freeing) > 1 {
				freeing = freeing[1:]
			}
			return []byte(v + "\n"), nil
		}
		t.Errorf("unexpected command %q", cmdline)
		return nil, errors.New("unexpected command")
	}}
}

func TestWaitForFrees(t *testing.T) {
	t.Run("reaches zero", func(t *testing.T) {
		r := freeingRunner(t, "4096000", "1024", "0")
		c := &Client{Runner: r, pollInterval: time.Millisecond}
		timedOut, err := c.WaitForFrees(context.Background(), "tank", time.Minute)
		require.NoError(t, err)
		require.False(t, timedOut)
		require.Equal(t, []string{
			"zpool sync tank",
			"zpool get -Hp -o value freeing tank",
			"zpool get -Hp -o value freeing tank",
			"zpool get -Hp -o value freeing tank",
		}, r.Calls())
	})

	t.Run("dash means nothing to free", func(t *testing.T) {
		c := &Client{Runner: freeingRunner(t, "-"), pollInterval: time.Millisecond}
		timedOut, err := c.WaitForFrees(context.Background(), "tank", time.Minute)
		require.NoError(t, err)
		require.False(t, timedOut)
	})

	t.Run("times out", func(t *testing.T) {
		r := freeingRunner(t, "4096000")
		c := &Client{Runner: r, pollInterval: 5 * time.Millisecond}
		start := time.Now()
		timedOut, err := c.WaitForFrees(context.Background(), "tank", 50*time.Millisecond)
		require.NoError(t, err)
		require.True(t, timedOut)
		require.Less(t, time.Since(start), 5*time.Second)
		require.Greater(t, len(r.Calls()), 2, "should poll more than once")
	})

	t.Run("ctx cancel", func(t *testing.T) {
		c := &Client{Runner: freeingRunner(t, "4096000"), pollInterval: 5 * time.Millisecond}
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(30*time.Millisecond, cancel)
		timedOut, err := c.WaitForFrees(ctx, "tank", time.Minute)
		require.ErrorIs(t, err, context.Canceled)
		require.False(t, timedOut)
	})

	t.Run("already cancelled", func(t *testing.T) {
		c := &Client{Runner: freeingRunner(t, "0")}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := c.WaitForFrees(ctx, "tank", time.Minute)
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("sync fails", func(t *testing.T) {
		c := &Client{Runner: failWith(errors.New("zpool sync tank: cannot open 'tank': no such pool (exit status 1)"))}
		timedOut, err := c.WaitForFrees(context.Background(), "tank", time.Minute)
		require.ErrorContains(t, err, "no such pool")
		require.False(t, timedOut)
	})

	t.Run("bad freeing value", func(t *testing.T) {
		c := &Client{Runner: freeingRunner(t, "lots")}
		_, err := c.WaitForFrees(context.Background(), "tank", time.Minute)
		require.ErrorContains(t, err, "freeing")
	})
}

func TestSnapshotBytes(t *testing.T) {
	// zfs get -r also lists the snapshots themselves, which report "-".
	r := answer(t, map[string]string{
		"zfs get -Hp -r -o value usedbysnapshots tank/media": "1000\n-\n2500\n0\n-\n500\n",
	})
	n, err := (&Client{Runner: r}).SnapshotBytes(context.Background(), "tank/media")
	require.NoError(t, err)
	require.Equal(t, uint64(4000), n)

	r = answer(t, map[string]string{"zfs get -Hp -r -o value usedbysnapshots tank": "12K\n"})
	_, err = (&Client{Runner: r}).SnapshotBytes(context.Background(), "tank")
	require.Error(t, err)
}

func TestHasSnapshots(t *testing.T) {
	const cmd = "zfs list -H -t snapshot -r -o name -s name tank/media"
	tests := []struct {
		out  string
		want bool
	}{
		{"", false},
		{"\n", false},
		{"tank/media@daily-2026-10-01\ntank/media/photos@daily-2026-10-01\n", true},
	}
	for _, tt := range tests {
		r := answer(t, map[string]string{cmd: tt.out})
		got, err := (&Client{Runner: r}).HasSnapshots(context.Background(), "tank/media")
		require.NoError(t, err)
		require.Equal(t, tt.want, got, "output %q", tt.out)
	}

	_, err := (&Client{Runner: failWith(errors.New("boom"))}).HasSnapshots(context.Background(), "tank/media")
	require.Error(t, err)
}

func TestDedup(t *testing.T) {
	r := answer(t, map[string]string{"zfs get -H -o value dedup tank/media": "sha256,verify\n"})
	got, err := (&Client{Runner: r}).Dedup(context.Background(), "tank/media")
	require.NoError(t, err)
	require.Equal(t, "sha256,verify", got)
}

func TestAvailable(t *testing.T) {
	r := answer(t, map[string]string{"zfs get -Hp -o value available tank/media": "5497558138880\n"})
	got, err := (&Client{Runner: r}).Available(context.Background(), "tank/media")
	require.NoError(t, err)
	require.Equal(t, uint64(5497558138880), got)

	r = answer(t, map[string]string{"zfs get -Hp -o value available tank/media": "5T\n"})
	_, err = (&Client{Runner: r}).Available(context.Background(), "tank/media")
	require.Error(t, err)
}

func TestBcloneUsed(t *testing.T) {
	const cmd = "zpool get -Hp -o value bcloneused tank"
	ctx := context.Background()

	t.Run("supported", func(t *testing.T) {
		r := answer(t, map[string]string{cmd: "123456789\n"})
		n, ok, err := (&Client{Runner: r}).BcloneUsed(ctx, "tank")
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, uint64(123456789), n)
	})

	t.Run("unsupported property", func(t *testing.T) {
		// OpenZFS before 2.2: libzfs rejects the name, then zpool prints usage.
		r := failWith(errors.New(cmd + ": bad property list: invalid property 'bcloneused' usage: get [-Hp] ... (exit status 2)"))
		n, ok, err := (&Client{Runner: r}).BcloneUsed(ctx, "tank")
		require.NoError(t, err)
		require.False(t, ok)
		require.Zero(t, n)
	})

	t.Run("dash", func(t *testing.T) {
		r := answer(t, map[string]string{cmd: "-\n"})
		_, ok, err := (&Client{Runner: r}).BcloneUsed(ctx, "tank")
		require.NoError(t, err)
		require.False(t, ok)
	})

	t.Run("other error", func(t *testing.T) {
		r := failWith(errors.New(cmd + ": cannot open 'tank': no such pool (exit status 1)"))
		_, ok, err := (&Client{Runner: r}).BcloneUsed(ctx, "tank")
		require.Error(t, err)
		require.False(t, ok)
	})
}

func TestNewClientUsesExecRunner(t *testing.T) {
	require.IsType(t, ExecRunner{}, NewClient().Runner)
}

func TestExecRunner(t *testing.T) {
	ctx := context.Background()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("needs sh")
	}

	t.Run("stdout", func(t *testing.T) {
		out, err := ExecRunner{}.Run(ctx, "sh", "-c", "echo hello; echo noise >&2")
		require.NoError(t, err)
		require.Equal(t, "hello\n", string(out))
	})

	t.Run("C locale", func(t *testing.T) {
		out, err := ExecRunner{}.Run(ctx, "sh", "-c", `echo "$LC_ALL $LANG"`)
		require.NoError(t, err)
		require.Equal(t, "C C\n", string(out))
	})

	t.Run("error includes trimmed stderr", func(t *testing.T) {
		_, err := ExecRunner{}.Run(ctx, "sh", "-c", `printf '  cannot open: no such pool\n\n' >&2; exit 3`)
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr)
		require.Equal(t, 3, exitErr.ExitCode())
		require.ErrorContains(t, err, "sh -c")
		require.ErrorContains(t, err, ": cannot open: no such pool (exit status 3)")
	})

	t.Run("long stderr is shortened", func(t *testing.T) {
		_, err := ExecRunner{}.Run(ctx, "sh", "-c", `for i in 1 2 3 4 5 6 7 8; do echo "line $i" >&2; done; exit 1`)
		require.ErrorContains(t, err, "line 1 line 2 line 3 line 4 line 5 ... (exit status 1)")
	})

	t.Run("missing command", func(t *testing.T) {
		_, err := ExecRunner{}.Run(ctx, "go-zfs-rebalance-no-such-command")
		require.ErrorIs(t, err, exec.ErrNotFound)
	})

	t.Run("ctx cancel kills the command", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := ExecRunner{}.Run(ctx, "sleep", "10")
		require.Error(t, err)
		require.Less(t, time.Since(start), 5*time.Second)
	})
}
