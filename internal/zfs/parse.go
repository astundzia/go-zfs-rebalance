package zfs

import (
	"errors"
	"fmt"
	"strings"
)

// How `zpool list -v -H -p -o name,size,allocated,free <pool>` prints, per the
// OpenZFS sources (cmd/zpool/zpool_main.c; the release tags below are
// identical to the heads of their zfs-X.Y-release branches):
//
//   - Pool row: print_pool() (zfs-2.1.16 L6150, zfs-2.2.11 L6242) /
//     collect_pool() (zfs-2.3.9 L6820, zfs-2.4.4 L6831). The -o columns joined
//     by "\t" with no leading tab: "tank\t<size>\t<alloc>\t<free>".
//
//   - Vdev rows: print_list_stats() (2.1.16 L6276, 2.2.11 L6371) /
//     collect_list_stats() (2.3.9 L7003, 2.4.4 L7013). In scripted mode the
//     name is printed as "\t%s" (2.1.16 L6306, 2.2.11 L6401, 2.3.9 L7041,
//     2.4.4 L7049) and every value as "\t%s" (print_one_column /
//     collect_vdev_prop: 2.1.16 L6266, 2.2.11 L6361, 2.3.9 L6992, 2.4.4
//     L7003), so each vdev row starts with a tab and nesting depth is lost.
//     Up to 2.3 the rows ignore -o and always carry nine values (SIZE ALLOC
//     FREE CKPOINT EXPANDSZ FRAG CAP DEDUP HEALTH); from 2.4 they carry only
//     the -o columns. Either way the first three values are SIZE, ALLOC, FREE.
//
//   - A value is valid only for top-level vdevs: toplevel = (vs_space != 0)
//     (2.1.16 L6292, 2.2.11 L6387, 2.3.9 L7021, 2.4.4 L7029). Leaf and
//     interior vdevs (disks under a mirror/raidz, replacing-N, spare-N) print
//     "-" for ALLOC and FREE, though SIZE still shows a leaf's physical size
//     (vs_pspace). A single-disk top-level vdev is also a leaf, so its SIZE is
//     the whole partition and is larger than ALLOC+FREE; zpool's own CAP
//     column is ALLOC/(ALLOC+FREE), which is what Vdev.UsedPct uses.
//
//   - Allocation classes are printed after the normal vdevs, in the order
//     dedup, special, logs (class_name[]), then cache and spare, each under a
//     header row. The header is NOT scripted-aware: up to 2.3 it is
//     printf("%-*s      -      -      - ...") (dashes, 2.1.16 L6404/6419/6432,
//     2.2.11 L6499/6514/6527, 2.3.9 L7178/7201/7223); from 2.4 it is
//     print_line() (2.4.4 L6757, called at L7235/7256/7277): the class name
//     padded with spaces, then "-" per column. So a header has no tabs and no
//     leading tab, and its first word is "dedup", "special", "logs", "cache"
//     or "spare".
//
//   - Cache devices are leaves but have vs_space set (l2arc_add_vdev() in
//     module/zfs/arc.c, zfs-2.3.9 L10006), so they print numbers; spares are
//     leaves without vs_space and print "-". Both sections contain only
//     devices, never nested rows, so every row in them is kept.
//
//   - Removed vdevs leave an "indirect-N" placeholder. The skip test
//     strcmp(name, "indirect") (2.3.9 L7031) never matches the "indirect-N"
//     name built by zpool_vdev_name(VDEV_NAME_TYPE_ID), so the row is printed
//     with "-" values and must be ignored here.

// Vdev classes as reported in Vdev.Class.
const (
	ClassData    = "data"
	ClassSpecial = "special"
	ClassDedup   = "dedup"
	ClassLog     = "log"
	ClassCache   = "cache"
	ClassSpare   = "spare"
)

// classHeaders maps zpool's section header words to Vdev classes. zpool list
// prints "spare"; "spares" (zpool status's spelling) is accepted too.
var classHeaders = map[string]string{
	"dedup":   ClassDedup,
	"special": ClassSpecial,
	"logs":    ClassLog,
	"cache":   ClassCache,
	"spare":   ClassSpare,
	"spares":  ClassSpare,
}

// Vdev is one top-level vdev of a pool, or one cache or spare device.
type Vdev struct {
	Name  string // e.g. "raidz1-0", "mirror-3", or a disk name for single-disk vdevs
	Class string // ClassData, ClassSpecial, ClassDedup, ClassLog, ClassCache or ClassSpare

	// Size is zpool's SIZE column. For a single-disk vdev it is the whole
	// partition, a little more than Alloc+Free; for cache and spare devices it
	// is the device's size.
	Size  uint64
	Alloc uint64
	Free  uint64
}

// UsedPct returns how full the vdev is, from 0 to 100. Like zpool's CAP
// column, it is Alloc out of Alloc+Free.
func (v Vdev) UsedPct() float64 {
	total := v.Alloc + v.Free
	if total == 0 {
		return 0
	}
	return float64(v.Alloc) / float64(total) * 100
}

// Distribution is a snapshot of how a pool's space is spread over its vdevs.
type Distribution struct {
	Pool  string
	Vdevs []Vdev // in zpool's order: data vdevs first, then the other classes
}

// Data returns the vdevs that hold normal data, the ones a rebalance evens out.
func (d Distribution) Data() []Vdev {
	var data []Vdev
	for _, v := range d.Vdevs {
		if v.Class == ClassData {
			data = append(data, v)
		}
	}
	return data
}

// Spread returns the gap, in percentage points, between the fullest and the
// emptiest data vdev. It is 0 when the pool has fewer than two data vdevs.
func (d Distribution) Spread() float64 {
	data := d.Data()
	if len(data) < 2 {
		return 0
	}
	lo, hi := data[0].UsedPct(), data[0].UsedPct()
	for _, v := range data[1:] {
		lo = min(lo, v.UsedPct())
		hi = max(hi, v.UsedPct())
	}
	return hi - lo
}

// ParseZpoolList parses the output of
// `zpool list -v -H -p -o name,size,allocated,free <pool>` for a single pool.
// It keeps top-level vdevs and cache and spare devices, and drops leaf disks
// and indirect (removed) vdevs.
func ParseZpoolList(out []byte) (Distribution, error) {
	var d Distribution
	class := ClassData
	sawPool := false
	lineNo := 0
	for line := range strings.Lines(string(out)) {
		lineNo++
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !sawPool {
			fields := strings.Split(line, "\t")
			if len(fields) < 4 || fields[0] == "" {
				return Distribution{}, parseError(lineNo, line, "expected the pool's name, size, allocated and free")
			}
			d.Pool = fields[0]
			sawPool = true
			continue
		}
		if row, ok := strings.CutPrefix(line, "\t"); ok {
			v, keep, err := parseVdevRow(strings.Split(row, "\t"), class)
			if err != nil {
				return Distribution{}, parseError(lineNo, line, err.Error())
			}
			if keep {
				d.Vdevs = append(d.Vdevs, v)
			}
			continue
		}
		c, ok := parseClassHeader(line)
		if !ok {
			return Distribution{}, parseError(lineNo, line, "not a vdev row or a class header")
		}
		class = c
	}
	if !sawPool {
		return Distribution{}, errors.New("zpool list printed no pool")
	}
	return d, nil
}

// parseVdevRow turns the fields of a vdev row (name, size, alloc, free, ...)
// into a Vdev. keep is false for rows that are not reported: leaf and
// interior vdevs of the data and allocation classes, and indirect vdevs.
func parseVdevRow(fields []string, class string) (v Vdev, keep bool, err error) {
	if len(fields) < 4 || fields[0] == "" {
		return Vdev{}, false, errors.New("expected a vdev name, size, allocated and free")
	}
	name, size, alloc, free := fields[0], fields[1], fields[2], fields[3]
	if strings.HasPrefix(name, "indirect-") {
		return Vdev{}, false, nil
	}
	// Outside cache and spare, only top-level vdevs have an ALLOC value.
	if class != ClassCache && class != ClassSpare && alloc == "-" {
		return Vdev{}, false, nil
	}
	v = Vdev{Name: name, Class: class}
	for _, f := range []struct {
		dst *uint64
		src string
	}{{&v.Size, size}, {&v.Alloc, alloc}, {&v.Free, free}} {
		n, err := parseOptionalUint(f.src)
		if err != nil {
			return Vdev{}, false, fmt.Errorf("%q is not an exact byte count (was -p used?)", f.src)
		}
		*f.dst = n
	}
	return v, true, nil
}

// parseClassHeader recognises a class section header such as
// "special      -      -      -". The first word is the class and every
// other word is "-".
func parseClassHeader(line string) (string, bool) {
	words := strings.Fields(line)
	if len(words) == 0 {
		return "", false
	}
	class, ok := classHeaders[words[0]]
	if !ok {
		return "", false
	}
	for _, w := range words[1:] {
		if w != "-" {
			return "", false
		}
	}
	return class, true
}

func parseError(lineNo int, line, why string) error {
	return fmt.Errorf("couldn't read zpool list output, line %d (%q): %s", lineNo, line, why)
}
