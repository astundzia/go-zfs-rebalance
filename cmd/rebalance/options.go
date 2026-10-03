package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/astundzia/go-zfs-rebalance/v2/internal/fileutil"
)

// maxConcurrency is the most files that are ever rewritten at the same time.
const maxConcurrency = 128

const helpText = `rebalance rewrites files in place, so ZFS spreads them across all your drives.

Usage: rebalance [options] <folder>

Examples:
  See how evenly the pool is filled (this changes nothing):
    rebalance --report /mnt/tank/media
  Rebalance, with a before-and-after table:
    rebalance --vdev-report /mnt/tank/media
  Carry on after stopping part-way:
    rebalance --resume /mnt/tank/media

Options:
  --report              Show how full each vdev is, and change nothing
  --vdev-report         Also show that table before and after the run
  --resume              Carry on where the last run for this folder stopped
  --passes N            Rewrite each file at most N times in all (default 1)
  --concurrency N       Files to rewrite at once (default: half the CPU cores)
  --process-hardlinks   Also rewrite hardlinked files, keeping them linked
  --no-cleanup          Keep temporary files left by an interrupted run
  --no-random           Go through files in folder order, not at random
  --checksum TYPE       How each copy is checked: sha256 (default) or md5
  --size-threshold MB   Only list rewritten files of at least this size
  --halt-on-missing     Stop if a file disappears before it is rewritten
  --filename-only       Show file names without their folders
  --db FILE             Keep progress in FILE instead of the usual place
  --debug               Show more detail
  --version             Show the version
  -h, --help            Show this help

More help: https://github.com/astundzia/go-zfs-rebalance#readme
`

func writeHelp(w io.Writer) { _, _ = io.WriteString(w, helpText) }

// options are the command-line settings.
type options struct {
	path             string
	passes           int
	resume           bool
	db               string
	processHardlinks bool
	concurrency      int // 0 means automatic; see resolveConcurrency
	noCleanup        bool
	oldNoCleanup     bool // the deprecated --no-cleanup-balance was given
	noRandom         bool
	checksum         string
	checksumType     fileutil.ChecksumType // checksum, checked by validate
	sizeThresholdMB  int
	haltOnMissing    bool
	filenameOnly     bool
	debug            bool
	vdevReport       bool
	report           bool
	version          bool
	help             bool
}

// errNoArgs means the command was run with no arguments at all.
var errNoArgs = errors.New("no arguments")

// newFlagSet defines every option, storing values in o. Parse errors are returned, not printed.
func newFlagSet(o *options) *flag.FlagSet {
	fs := flag.NewFlagSet("rebalance", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	fs.Func("passes", "", wholeNumber(&o.passes))
	fs.BoolVar(&o.resume, "resume", false, "")
	fs.StringVar(&o.db, "db", "", "")
	fs.BoolVar(&o.processHardlinks, "process-hardlinks", false, "")
	fs.Func("concurrency", "", wholeNumber(&o.concurrency))
	fs.BoolVar(&o.noCleanup, "no-cleanup", false, "")
	fs.BoolFunc("no-cleanup-balance", "", func(s string) error {
		on, err := strconv.ParseBool(s)
		if err != nil {
			return errors.New("it must be true or false")
		}
		o.noCleanup = o.noCleanup || on
		o.oldNoCleanup = true
		return nil
	})
	fs.BoolVar(&o.noRandom, "no-random", false, "")
	fs.StringVar(&o.checksum, "checksum", string(fileutil.ChecksumSHA256), "")
	fs.Func("size-threshold", "", wholeNumber(&o.sizeThresholdMB))
	fs.BoolVar(&o.haltOnMissing, "halt-on-missing", false, "")
	fs.BoolVar(&o.filenameOnly, "filename-only", false, "")
	fs.BoolVar(&o.debug, "debug", false, "")
	fs.BoolVar(&o.vdevReport, "vdev-report", false, "")
	fs.BoolVar(&o.report, "report", false, "")
	fs.BoolVar(&o.version, "version", false, "")
	fs.BoolVar(&o.help, "h", false, "")
	fs.BoolVar(&o.help, "help", false, "")
	return fs
}

func wholeNumber(p *int) func(string) error {
	return func(s string) error {
		n, err := strconv.Atoi(s)
		if err != nil {
			return errors.New("it must be a whole number, like 2")
		}
		*p = n
		return nil
	}
}

// parseArgs reads the command line. Options may come before or after the folder, and "--" ends
// the options, so a folder whose name starts with "-" can be given after it.
func parseArgs(args []string) (options, error) {
	o := options{passes: 1}
	if len(args) == 0 {
		return o, errNoArgs
	}
	fs := newFlagSet(&o)
	var paths []string
	for rest := args; len(rest) > 0; {
		if err := fs.Parse(rest); err != nil {
			return o, friendlyFlagError(err, rest)
		}
		left := fs.Args()
		if endedWithTerminator(rest[:len(rest)-len(left)]) {
			paths = append(paths, left...)
			break
		}
		if len(left) == 0 {
			break
		}
		paths = append(paths, left[0])
		rest = left[1:]
	}
	if o.help || o.version {
		return o, nil
	}
	switch len(paths) {
	case 0:
		return o, errors.New("please give the folder to rebalance, for example: rebalance /mnt/tank/media")
	case 1:
		o.path = paths[0]
	default:
		quoted := make([]string, len(paths))
		for i, p := range paths {
			quoted[i] = strconv.Quote(p)
		}
		return o, fmt.Errorf("please give just one folder, not %d (%s). If a folder's name has spaces in it, put it in quotes",
			len(paths), strings.Join(quoted, ", "))
	}
	return o, o.validate()
}

// endedWithTerminator reports whether flag parsing that consumed these arguments stopped at a "--",
// which the flag package swallows. A "--" that was an option's value, as in "--db --", doesn't
// count: parsing everything before it again then fails, because the option has no value.
func endedWithTerminator(consumed []string) bool {
	n := len(consumed)
	if n == 0 || consumed[n-1] != "--" {
		return false
	}
	var scratch options
	probe := newFlagSet(&scratch)
	return probe.Parse(consumed[:n-1]) == nil && probe.NArg() == 0
}

// friendlyFlagError rewords the flag package's two most common errors, naming the option as it was
// typed in args. They have no types to check, so their stable wording is matched instead.
func friendlyFlagError(err error, args []string) error {
	msg := err.Error()
	if name, ok := strings.CutPrefix(msg, "flag provided but not defined: -"); ok {
		typed, whole := findOption(name, args)
		if looksLikeFolder(whole) {
			return fmt.Errorf("there's no option called %s. If that's the folder to rebalance, put -- before it, like this: rebalance -- %s",
				typed, shellQuote(whole))
		}
		return fmt.Errorf("there's no option called %s", typed)
	}
	if name, ok := strings.CutPrefix(msg, "flag needs an argument: -"); ok {
		typed, _ := findOption(name, args)
		return fmt.Errorf("%s needs a value after it", typed)
	}
	return err
}

// findOption finds the argument the flag package called name: the flag package drops the dashes
// in front and anything after an "=". It returns the option as it was typed, without any "=value",
// and the whole argument. If no argument matches, both are "--" + name, as the help shows it.
func findOption(name string, args []string) (typed, whole string) {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		bare := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
		if bare == arg {
			continue
		}
		if before, _, _ := strings.Cut(bare, "="); before == name {
			return arg[:len(arg)-len(bare)] + name, arg
		}
	}
	return "--" + name, "--" + name
}

// looksLikeFolder reports whether arg, which starts with "-", is more likely a folder name than a
// mistyped option: it has a space or slash in it, or there is something by that name here.
func looksLikeFolder(arg string) bool {
	if strings.ContainsAny(arg, "/ \t") {
		return true
	}
	_, err := os.Lstat(arg)
	return err == nil
}

// shellQuote quotes s for a POSIX shell when it needs it.
func shellQuote(s string) string {
	safe := func(r rune) bool {
		return r < utf8.RuneSelf && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./,:+=@%", r))
	}
	if s != "" && strings.IndexFunc(s, func(r rune) bool { return !safe(r) }) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// validate checks option values that can be wrong even when they parse.
func (o *options) validate() error {
	switch {
	case o.passes < 1:
		return fmt.Errorf("--passes must be 1 or more (it was %d). A normal run already rewrites every file once, which is --passes 1, the default", o.passes)
	case o.concurrency < 0:
		return fmt.Errorf("--concurrency can't be negative (it was %d). Leave it out, or use 0, to choose automatically", o.concurrency)
	case o.sizeThresholdMB < 0:
		return fmt.Errorf("--size-threshold can't be negative (it was %d). Leave it out, or use 0, to list every file", o.sizeThresholdMB)
	}
	sum, err := fileutil.ParseChecksumType(o.checksum)
	if err != nil {
		return fmt.Errorf("--checksum: %w", err)
	}
	o.checksumType = sum
	return nil
}

// resolveConcurrency turns --concurrency into the number of files to work on at once. 0 means
// half the CPU cores, at least 2. Anything above maxConcurrency is lowered to it, with a note.
func resolveConcurrency(requested, cpus int) (n int, note string) {
	switch {
	case requested == 0:
		return min(max(cpus/2, 2), maxConcurrency), ""
	case requested > maxConcurrency:
		return maxConcurrency, fmt.Sprintf("--concurrency is limited to %d, so %d files will be worked on at once.", maxConcurrency, maxConcurrency)
	}
	return requested, ""
}

// resolveRoot turns the folder given on the command line into an absolute path with symlinks
// resolved, and checks that it is a folder.
func resolveRoot(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("couldn't work out the full path of %q (%s)", p, reasonOf(err))
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		var fi os.FileInfo
		if fi, err = os.Stat(resolved); err == nil && !fi.IsDir() {
			return "", fmt.Errorf("%q is a file, not a folder. Give the folder to rebalance", abs)
		}
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("the folder %q doesn't exist", abs)
	case err != nil:
		return "", fmt.Errorf("couldn't open the folder %q (%s)", abs, reasonOf(err))
	}
	return resolved, nil
}

// reasonOf is a short description of err without the path, such as "permission denied".
func reasonOf(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}
