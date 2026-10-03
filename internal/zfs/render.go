package zfs

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// iecUnits are the units FormatBytes uses, each 1024 times the previous one.
var iecUnits = []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}

// columnGap is the number of spaces between table columns.
const columnGap = 3

// FormatBytes formats n in IEC units with one decimal, such as "5.1 TiB".
// Amounts under 1 KiB are shown as whole bytes, such as "512 B".
func FormatBytes(n uint64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	unit, div := scaleBytes(n)
	return fmt.Sprintf("%.1f %s", float64(n)/div, iecUnits[unit])
}

// scaleBytes picks the largest unit in which n stays below 1024 after rounding
// to one decimal (so 1023.96 KiB becomes "1.0 MiB", not "1024.0 KiB"). It
// returns the unit's index in iecUnits and its size in bytes.
func scaleBytes(n uint64) (unit int, div float64) {
	div = 1
	for unit < len(iecUnits)-1 && float64(n)/div >= 1024-0.05 {
		div *= 1024
		unit++
	}
	return unit, div
}

// formatChange formats a signed byte difference with an explicit sign, such
// as "+7.5 TiB" or "-5.1 TiB". No change is "0 B".
func formatChange(delta int64) string {
	switch {
	case delta > 0:
		return "+" + FormatBytes(uint64(delta))
	case delta < 0:
		// uint64(-delta) is also right for math.MinInt64.
		return "-" + FormatBytes(uint64(-delta))
	default:
		return "0 B"
	}
}

// formatFill formats alloc and size in size's unit, such as "0.9/1.8 TiB".
func formatFill(alloc, size uint64) string {
	if size < 1024 {
		return fmt.Sprintf("%d/%d B", alloc, size)
	}
	unit, div := scaleBytes(size)
	return fmt.Sprintf("%.1f/%.1f %s", float64(alloc)/div, float64(size)/div, iecUnits[unit])
}

func formatPct(p float64) string { return fmt.Sprintf("%.1f%%", p) }

func formatPts(p float64) string { return fmt.Sprintf("%.1f pts", p) }

// RenderReport writes d as a table: one row per data vdev with its size,
// allocated space and how full it is, then the spread between the fullest and
// emptiest vdev, then one "also:" line naming the pool's other vdevs.
//
//	Pool tank   SIZE       ALLOC      USED%
//	raidz1-0    29.1 TiB   20.3 TiB   69.7%
//	spread      61.4 pts
//	also: special mirror-3 (0.9/1.8 TiB), log mirror-4, cache nvme0n1
func RenderReport(w io.Writer, d Distribution) error {
	rows := [][]string{{"Pool " + d.Pool, "SIZE", "ALLOC", "USED%"}}
	for _, v := range d.Data() {
		rows = append(rows, []string{v.Name, FormatBytes(v.Size), FormatBytes(v.Alloc), formatPct(v.UsedPct())})
	}
	alignRight(rows, 1, 2, 3)
	rows = append(rows, []string{"spread", formatPts(d.Spread())})
	if err := writeTable(w, rows); err != nil {
		return err
	}
	if others := describeOthers(d); others != "" {
		if _, err := fmt.Fprintf(w, "also: %s\n", others); err != nil {
			return err
		}
	}
	return nil
}

// describeOthers lists the vdevs that are not data vdevs, such as
// "special mirror-3 (0.9/1.8 TiB), log mirror-4". Special and dedup vdevs
// hold real data and can fill up, so their fill level is included.
func describeOthers(d Distribution) string {
	var parts []string
	for _, v := range d.Vdevs {
		switch v.Class {
		case ClassData:
		case ClassSpecial, ClassDedup:
			parts = append(parts, fmt.Sprintf("%s %s (%s)", v.Class, v.Name, formatFill(v.Alloc, v.Size)))
		default:
			parts = append(parts, v.Class+" "+v.Name)
		}
	}
	return strings.Join(parts, ", ")
}

// RenderComparison writes how each data vdev changed between before and
// after: its fill level before and after, and how much its allocated space
// grew or shrank. Then it writes the spread before and after, and finally each
// note on its own line. A vdev found in only one snapshot shows "-" for the
// missing side.
//
//	Pool tank   USED%            ALLOC CHANGE
//	raidz1-0    69.7% -> 46.3%       -5.1 TiB
//	raidz1-2     8.3% -> 42.7%       +7.5 TiB
//	spread      61.4 pts -> 3.6 pts
//	note: ...
func RenderComparison(w io.Writer, before, after Distribution, notes []string) error {
	pool := after.Pool
	if pool == "" {
		pool = before.Pool
	}

	type pair struct {
		name          string
		before, after *Vdev
	}
	var pairs []*pair
	byName := map[string]*pair{}
	get := func(name string) *pair {
		p, ok := byName[name]
		if !ok {
			p = &pair{name: name}
			byName[name] = p
			pairs = append(pairs, p)
		}
		return p
	}
	for _, v := range before.Data() {
		get(v.Name).before = &v
	}
	for _, v := range after.Data() {
		get(v.Name).after = &v
	}

	pctOrDash := func(v *Vdev) string {
		if v == nil {
			return "-"
		}
		return formatPct(v.UsedPct())
	}
	beforeWidth, afterWidth := 0, 0
	for _, p := range pairs {
		beforeWidth = max(beforeWidth, len(pctOrDash(p.before)))
		afterWidth = max(afterWidth, len(pctOrDash(p.after)))
	}

	rows := [][]string{{"Pool " + pool, "USED%", "ALLOC CHANGE"}}
	for _, p := range pairs {
		change := "-"
		if p.before != nil && p.after != nil {
			change = formatChange(int64(p.after.Alloc) - int64(p.before.Alloc))
		}
		used := fmt.Sprintf("%*s -> %*s", beforeWidth, pctOrDash(p.before), afterWidth, pctOrDash(p.after))
		rows = append(rows, []string{p.name, used, change})
	}
	alignRight(rows, 2)
	rows = append(rows, []string{"spread", formatPts(before.Spread()) + " -> " + formatPts(after.Spread())})
	if err := writeTable(w, rows); err != nil {
		return err
	}
	for _, n := range notes {
		if _, err := fmt.Fprintf(w, "note: %s\n", n); err != nil {
			return err
		}
	}
	return nil
}

// alignRight left-pads the given columns of rows so each column's cells have
// the same width, which right-aligns them in a left-aligned tabwriter table.
func alignRight(rows [][]string, cols ...int) {
	for _, c := range cols {
		width := 0
		for _, r := range rows {
			width = max(width, len(r[c]))
		}
		for _, r := range rows {
			r[c] = strings.Repeat(" ", width-len(r[c])) + r[c]
		}
	}
}

// writeTable writes rows as aligned columns. A row's last cell is not part of
// a column, so a shorter row (like "spread") can run past the columns
// without widening them.
func writeTable(w io.Writer, rows [][]string) error {
	t := tabwriter.NewWriter(w, 0, 0, columnGap, ' ', 0)
	for _, r := range rows {
		if _, err := fmt.Fprintln(t, strings.Join(r, "\t")); err != nil {
			return err
		}
	}
	return t.Flush()
}
