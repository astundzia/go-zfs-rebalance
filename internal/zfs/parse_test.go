package zfs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// classesVdevs is the pool in testdata/classes*.txt: three raidz1 data vdevs
// plus a special mirror, a log mirror, an L2ARC cache disk and a hot spare.
var classesVdevs = []Vdev{
	{Name: "raidz1-0", Class: ClassData, Size: 32006096289792, Alloc: 22308249112576, Free: 9697847177216},
	{Name: "raidz1-1", Class: ClassData, Size: 32006096289792, Alloc: 17603352956928, Free: 14402743332864},
	{Name: "raidz1-2", Class: ClassData, Size: 32006096289792, Alloc: 2656505991168, Free: 29349590298624},
	{Name: "mirror-3", Class: ClassSpecial, Size: 1992864825344, Alloc: 990453817344, Free: 1002411008000},
	{Name: "mirror-4", Class: ClassLog, Size: 15569256448, Alloc: 1376256, Free: 15567880192},
	{Name: "nvme0n1", Class: ClassCache, Size: 500107862016, Alloc: 412316860416, Free: 87782088704},
	{Name: "1b70c69e-f82d-4e14-99fc-4d8b0a6c29f1", Class: ClassSpare, Size: 8001561821184},
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return b
}

func TestParseZpoolListFixtures(t *testing.T) {
	tests := []struct {
		file   string
		pool   string
		vdevs  []Vdev
		spread float64
	}{
		{
			// OpenZFS 2.3 format; leaf disks are TrueNAS-style partition UUIDs.
			file:   "classes.txt",
			pool:   "tank",
			vdevs:  classesVdevs,
			spread: 61.40,
		},
		{
			// OpenZFS 2.4+: vdev rows carry only the -o columns and class
			// headers come from print_line().
			file:   "classes_v24.txt",
			pool:   "tank",
			vdevs:  classesVdevs,
			spread: 61.40,
		},
		{
			// Single-disk vdevs: SIZE is the partition, larger than ALLOC+FREE.
			file: "stripe.txt",
			pool: "tank",
			vdevs: []Vdev{
				{Name: "sda", Class: ClassData, Size: 4000776716288, Alloc: 2790010753024, Free: 1195718897664},
				{Name: "sdb", Class: ClassData, Size: 4000776716288, Alloc: 2192151306240, Free: 1793578344448},
				{Name: "sdc", Class: ClassData, Size: 4000776716288, Alloc: 330815557632, Free: 3654914093056},
			},
			spread: 61.70,
		},
		{
			file: "mirrors.txt",
			pool: "tank",
			vdevs: []Vdev{
				{Name: "mirror-0", Class: ClassData, Size: 3985729650688, Alloc: 3228441014272, Free: 757288636416},
				{Name: "mirror-1", Class: ClassData, Size: 7988639170560, Alloc: 958636699648, Free: 7030002470912},
			},
			spread: 69.00,
		},
		{
			file: "single.txt",
			pool: "backup",
			vdevs: []Vdev{
				{Name: "raidz2-0", Class: ClassData, Size: 24000277250048, Alloc: 10080116441088, Free: 13920160808960},
			},
			spread: 0,
		},
		{
			// After `zpool remove`, the indirect-0 placeholder is ignored.
			file: "removal.txt",
			pool: "tank",
			vdevs: []Vdev{
				{Name: "mirror-1", Class: ClassData, Size: 3985729650688, Alloc: 2550866972672, Free: 1434862678016},
				{Name: "mirror-2", Class: ClassData, Size: 3985729650688, Alloc: 1235576188928, Free: 2750153461760},
			},
			spread: 33.00,
		},
		{
			// OpenZFS 2.1 format (slightly different header padding) with a
			// dedup class.
			file: "dedup_v21.txt",
			pool: "tank",
			vdevs: []Vdev{
				{Name: "mirror-0", Class: ClassData, Size: 3985729650688, Alloc: 1992864825344, Free: 1992864825344},
				{Name: "mirror-1", Class: ClassData, Size: 3985729650688, Alloc: 996432412672, Free: 2989297238016},
				{Name: "mirror-2", Class: ClassDedup, Size: 1992864825344, Alloc: 199286480896, Free: 1793578344448},
			},
			spread: 25.00,
		},
		{
			// Captured from a live Ubuntu 24.04 OpenZFS 2.2.2 pool: vdev rows
			// ignore -o and print every default column.
			file: "real_v222_mirror.txt",
			pool: "tank",
			vdevs: []Vdev{
				{Name: "mirror-0", Class: ClassData, Size: 10200547328, Alloc: 5245923328, Free: 4954624000},
			},
			spread: 0,
		},
		{
			file: "real_v222_single.txt",
			pool: "old",
			vdevs: []Vdev{
				{Name: "virtio-DATA5", Class: ClassData, Size: 10726932480, Alloc: 2885058560, Free: 7315488768},
			},
			spread: 0,
		},
		{
			// Captured from TrueNAS SCALE 25.10.7 (OpenZFS 2.3.9): pool built by
			// the middleware, so leaf disks are partition UUIDs.
			file: "real_v239_truenas.txt",
			pool: "tank",
			vdevs: []Vdev{
				{Name: "mirror-0", Class: ClassData, Size: 10200547328, Alloc: 5226000384, Free: 4974546944},
				{Name: "mirror-1", Class: ClassData, Size: 10200547328, Alloc: 139264, Free: 10200408064},
			},
			spread: 51.23,
		},
		{
			// Live OpenZFS 2.2.2 capture: space-padded class headers (even
			// with -H), a single-disk log vdev and a "spare" header.
			file: "real_v222_classes.txt",
			pool: "fx",
			vdevs: []Vdev{
				{Name: "raidz1-0", Class: ClassData, Size: 738197504, Alloc: 100663296, Free: 637534208},
				{Name: "mirror-1", Class: ClassSpecial, Size: 251658240, Alloc: 228352, Free: 251429888},
				{Name: "/var/tmp/fx/l1", Class: ClassLog, Size: 268435456, Alloc: 0, Free: 251658240},
				{Name: "/var/tmp/fx/c1", Class: ClassCache, Size: 268435456, Alloc: 5379072, Free: 258337280},
				{Name: "/var/tmp/fx/sp1", Class: ClassSpare, Size: 268435456},
			},
			spread: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			d, err := ParseZpoolList(readFixture(t, tt.file))
			require.NoError(t, err)
			require.Equal(t, tt.pool, d.Pool)
			require.Equal(t, tt.vdevs, d.Vdevs)
			require.InDelta(t, tt.spread, d.Spread(), 0.01)
		})
	}
}

func TestParseZpoolListErrors(t *testing.T) {
	tests := map[string]string{
		"empty":                "",
		"vdev row first":       "\tmirror-0\t100\t50\t50\n",
		"pool row too short":   "tank\t100\n",
		"human-readable sizes": "tank\t3.62T\t1.2T\t2.4T\n\tsda\t3.64T\t1.2T\t2.4T\t-\t-\t1%\t33%\t-\tONLINE\n",
		"short vdev row":       "tank\t100\t50\t50\n\tmirror-0\t100\n",
		"second pool":          "tank\t100\t50\t50\nother\t100\t50\t50\n",
		"unknown header":       "tank\t100\t50\t50\nbogus      -      -      -\n",
		"header with values":   "tank\t100\t50\t50\nspecial    100    50    50\n",
	}
	for name, out := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseZpoolList([]byte(out))
			require.Error(t, err)
		})
	}
}

func TestParseZpoolListSparesHeader(t *testing.T) {
	out := "tank\t100\t50\t50\n\tsda\t100\t50\t50\nspares  -  -  -\n\tsdz\t100\t-\t-\n"
	d, err := ParseZpoolList([]byte(out))
	require.NoError(t, err)
	require.Equal(t, []Vdev{
		{Name: "sda", Class: ClassData, Size: 100, Alloc: 50, Free: 50},
		{Name: "sdz", Class: ClassSpare, Size: 100},
	}, d.Vdevs)
}

func TestUsedPct(t *testing.T) {
	tests := []struct {
		name string
		v    Vdev
		want float64
	}{
		{"empty", Vdev{}, 0},
		{"half", Vdev{Size: 200, Alloc: 100, Free: 100}, 50},
		// A single-disk vdev's SIZE includes space outside its metaslabs.
		{"ignores size", Vdev{Size: 1000, Alloc: 30, Free: 70}, 30},
		{"full", Vdev{Size: 10, Alloc: 10}, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.InDelta(t, tt.want, tt.v.UsedPct(), 1e-9)
		})
	}
}

func TestSpread(t *testing.T) {
	data := func(name string, alloc, free uint64) Vdev {
		return Vdev{Name: name, Class: ClassData, Size: alloc + free, Alloc: alloc, Free: free}
	}
	tests := []struct {
		name  string
		vdevs []Vdev
		want  float64
	}{
		{"no vdevs", nil, 0},
		{"one vdev", []Vdev{data("a", 90, 10)}, 0},
		{"two vdevs", []Vdev{data("a", 90, 10), data("b", 20, 80)}, 70},
		{"middle vdev ignored", []Vdev{data("a", 50, 50), data("b", 90, 10), data("c", 30, 70)}, 60},
		{
			"other classes ignored",
			[]Vdev{
				data("a", 40, 60),
				data("b", 60, 40),
				{Name: "s", Class: ClassSpecial, Alloc: 99, Free: 1},
				{Name: "c", Class: ClassCache, Alloc: 0, Free: 100},
			},
			20,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Distribution{Pool: "tank", Vdevs: tt.vdevs}
			require.InDelta(t, tt.want, d.Spread(), 1e-9)
		})
	}
}

func TestDataKeepsOrder(t *testing.T) {
	d := Distribution{Vdevs: classesVdevs}
	var names []string
	for _, v := range d.Data() {
		names = append(names, v.Name)
	}
	require.Equal(t, []string{"raidz1-0", "raidz1-1", "raidz1-2"}, names)
}
