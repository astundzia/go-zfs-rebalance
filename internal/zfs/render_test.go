package zfs

import (
	"bytes"
	"errors"
	"flag"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden from the current output")

// checkGolden compares got with testdata/<name>.golden, or rewrites the file
// when the test runs with -update.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		require.NoError(t, os.WriteFile(path, got, 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "run `go test ./internal/zfs -update` to create it")
	require.Equal(t, string(want), string(got))
}

func TestFormatBytes(t *testing.T) {
	const (
		kib = 1 << (10 * (iota + 1))
		mib
		gib
		tib
		pib
	)
	tests := []struct {
		n    uint64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{1023, "1023 B"},
		{kib, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{mib - 1, "1.0 MiB"}, // rounds up into the next unit, never "1024.0 KiB"
		{mib, "1.0 MiB"},
		{1023 * mib, "1023.0 MiB"},
		{gib + gib/2, "1.5 GiB"},
		{5*tib + tib/10, "5.1 TiB"},
		{pib, "1.0 PiB"},
		{2048 * pib, "2048.0 PiB"}, // PiB is the largest unit
		{math.MaxUint64, "16384.0 PiB"},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, FormatBytes(tt.n), "FormatBytes(%d)", tt.n)
	}
}

func TestFormatChange(t *testing.T) {
	tests := []struct {
		delta int64
		want  string
	}{
		{0, "0 B"},
		{512, "+512 B"},
		{-512, "-512 B"},
		{7*(1<<40) + (1<<40)/2, "+7.5 TiB"},
		{-(5*(1<<40) + (1<<40)/10), "-5.1 TiB"},
		{math.MinInt64, "-8192.0 PiB"},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, formatChange(tt.delta), "formatChange(%d)", tt.delta)
	}
}

func TestFormatFill(t *testing.T) {
	require.Equal(t, "0.9/1.8 TiB", formatFill(990453817344, 1992864825344))
	require.Equal(t, "0.5/1.0 KiB", formatFill(512, 1024)) // alloc uses the size's unit
	require.Equal(t, "10/100 B", formatFill(10, 100))
}

func TestRenderReportGolden(t *testing.T) {
	for _, name := range []string{"classes", "stripe", "single", "removal", "dedup_v21"} {
		t.Run(name, func(t *testing.T) {
			d, err := ParseZpoolList(readFixture(t, name+".txt"))
			require.NoError(t, err)
			var buf bytes.Buffer
			require.NoError(t, RenderReport(&buf, d))
			checkGolden(t, "report_"+name, buf.Bytes())
		})
	}
}

// fillVdev returns a data vdev of the given size that is pct percent full.
func fillVdev(name string, size uint64, pct float64) Vdev {
	alloc := uint64(float64(size) * pct / 100)
	return Vdev{Name: name, Class: ClassData, Size: size, Alloc: alloc, Free: size - alloc}
}

func TestRenderComparisonGolden(t *testing.T) {
	const size = 24000277250048 // 21.8 TiB
	special := Vdev{Name: "mirror-3", Class: ClassSpecial, Size: 1992864825344, Alloc: 990453817344, Free: 1002411008000}

	tests := []struct {
		name          string
		before, after Distribution
		notes         []string
	}{
		{
			// The layout the user approved.
			name: "comparison_basic",
			before: Distribution{Pool: "tank", Vdevs: []Vdev{
				fillVdev("raidz1-0", size, 69.7), fillVdev("raidz1-1", size, 55.0), fillVdev("raidz1-2", size, 8.3), special,
			}},
			after: Distribution{Pool: "tank", Vdevs: []Vdev{
				fillVdev("raidz1-0", size, 46.3), fillVdev("raidz1-1", size, 44.0), fillVdev("raidz1-2", size, 42.7), special,
			}},
		},
		{
			// mirror-1 was removed and mirror-2 added during the run.
			name: "comparison_changed_vdevs",
			before: Distribution{Pool: "tank", Vdevs: []Vdev{
				fillVdev("mirror-0", 3985729650688, 81.0), fillVdev("mirror-1", 3985729650688, 12.5),
			}},
			after: Distribution{Pool: "tank", Vdevs: []Vdev{
				fillVdev("mirror-0", 3985729650688, 81.0), fillVdev("mirror-2", 7988639170560, 40.0),
			}},
			notes: []string{
				"old blocks are still pinned by snapshots, so the old vdevs won't shrink until those snapshots are destroyed",
				"timed out waiting for ZFS to finish freeing space, so the numbers may still change",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, RenderComparison(&buf, tt.before, tt.after, tt.notes))
			checkGolden(t, tt.name, buf.Bytes())
		})
	}
}

func TestRenderComparisonFromFixtures(t *testing.T) {
	before, err := ParseZpoolList(readFixture(t, "classes.txt"))
	require.NoError(t, err)
	after, err := ParseZpoolList(readFixture(t, "classes_v24.txt"))
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, RenderComparison(&buf, before, after, nil))
	require.Contains(t, buf.String(), "raidz1-0    69.7% -> 69.7%            0 B\n")
	require.Contains(t, buf.String(), "spread      61.4 pts -> 61.4 pts\n")
	require.NotContains(t, buf.String(), "mirror-3", "only data vdevs are compared")
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestRenderWriteErrors(t *testing.T) {
	d := Distribution{Pool: "tank", Vdevs: classesVdevs}
	require.Error(t, RenderReport(failingWriter{}, d))
	require.Error(t, RenderComparison(failingWriter{}, d, d, []string{"a note"}))
}
