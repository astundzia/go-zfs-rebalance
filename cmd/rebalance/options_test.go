package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astundzia/go-zfs-rebalance/v2/internal/fileutil"
)

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		check func(t *testing.T, o options)
	}{
		{"defaults", []string{"/tank"}, func(t *testing.T, o options) {
			if o.path != "/tank" || o.passes != 1 || o.concurrency != 0 || o.checksumType != fileutil.ChecksumSHA256 ||
				o.resume || o.noCleanup || o.noRandom || o.processHardlinks || o.vdevReport || o.report {
				t.Errorf("unexpected defaults: %+v", o)
			}
		}},
		{"options after the folder", []string{"/tank", "--passes", "2", "--no-random"}, func(t *testing.T, o options) {
			if o.path != "/tank" || o.passes != 2 || !o.noRandom {
				t.Errorf("got %+v", o)
			}
		}},
		{"options on both sides", []string{"--debug", "/tank", "--resume", "--concurrency=4", "-filename-only"}, func(t *testing.T, o options) {
			if o.path != "/tank" || !o.debug || !o.resume || o.concurrency != 4 || !o.filenameOnly {
				t.Errorf("got %+v", o)
			}
		}},
		{"-- ends the options", []string{"--no-random", "--", "-weird name"}, func(t *testing.T, o options) {
			if o.path != "-weird name" || !o.noRandom {
				t.Errorf("got %+v", o)
			}
		}},
		{"after --, options are names", []string{"--", "--resume"}, func(t *testing.T, o options) {
			if o.path != "--resume" || o.resume {
				t.Errorf("got %+v", o)
			}
		}},
		{"-- after the folder", []string{"/tank", "--"}, func(t *testing.T, o options) {
			if o.path != "/tank" {
				t.Errorf("got %+v", o)
			}
		}},
		{"-- as an option's value", []string{"--db", "--", "/tank", "--debug"}, func(t *testing.T, o options) {
			if o.db != "--" || o.path != "/tank" || !o.debug {
				t.Errorf("got %+v", o)
			}
		}},
		{"a lone dash is a folder name", []string{"-"}, func(t *testing.T, o options) {
			if o.path != "-" {
				t.Errorf("got %+v", o)
			}
		}},
		{"every option", []string{"/tank", "--passes=3", "--resume", "--db", "/x.db", "--process-hardlinks", "--concurrency", "8",
			"--no-cleanup", "--no-random", "--checksum", " MD5 ", "--size-threshold", "20", "--halt-on-missing",
			"--filename-only", "--debug", "--vdev-report", "--report"}, func(t *testing.T, o options) {
			want := options{path: "/tank", passes: 3, resume: true, db: "/x.db", processHardlinks: true, concurrency: 8,
				noCleanup: true, noRandom: true, checksum: " MD5 ", checksumType: fileutil.ChecksumMD5, sizeThresholdMB: 20,
				haltOnMissing: true, filenameOnly: true, debug: true, vdevReport: true, report: true}
			if o != want {
				t.Errorf("got  %+v\nwant %+v", o, want)
			}
		}},
		{"deprecated --no-cleanup-balance still works", []string{"--no-cleanup-balance", "/tank"}, func(t *testing.T, o options) {
			if !o.noCleanup || !o.oldNoCleanup {
				t.Errorf("got %+v", o)
			}
		}},
		{"help needs no folder", []string{"--help"}, func(t *testing.T, o options) {
			if !o.help {
				t.Errorf("got %+v", o)
			}
		}},
		{"-h after a folder", []string{"/tank", "-h"}, func(t *testing.T, o options) {
			if !o.help {
				t.Errorf("got %+v", o)
			}
		}},
		{"version wins over a bad value", []string{"--version", "--passes", "0"}, func(t *testing.T, o options) {
			if !o.version {
				t.Errorf("got %+v", o)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, err := parseArgs(tt.args)
			if err != nil {
				t.Fatalf("parseArgs(%q): %v", tt.args, err)
			}
			tt.check(t, o)
		})
	}
}

func TestParseArgsErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"two folders", []string{"/a", "/b"}, `please give just one folder, not 2 ("/a", "/b")`},
		{"two folders after --", []string{"--", "/a", "-b"}, "just one folder"},
		{"no folder", []string{"--debug"}, "please give the folder to rebalance"},
		{"passes 0", []string{"--passes", "0", "/a"}, "--passes must be 1 or more (it was 0). A normal run already rewrites every file once"},
		{"negative passes", []string{"/a", "--passes=-2"}, "--passes must be 1 or more"},
		{"passes not a number", []string{"--passes", "two", "/a"}, `--passes needs a whole number, like 2, not "two"`},
		{"concurrency not a number", []string{"/a", "--concurrency=abc"}, `--concurrency needs a whole number, like 4, not "abc"`},
		{"concurrency too big", []string{"/a", "--concurrency=99999999999999999999"}, `--concurrency needs a smaller whole number, like 4, not "99999999999999999999"`},
		{"concurrency typed with one dash", []string{"-concurrency", "1.5", "/a"}, `-concurrency needs a whole number, like 4, not "1.5"`},
		{"size threshold empty", []string{"--size-threshold=", "/a"}, "--size-threshold needs a whole number, like 100"},
		{"value with control characters", []string{"--passes", "\x1b[2J", "/a"}, `--passes needs a whole number, like 2, not "\x1b[2J"`},
		{"on/off option given a value", []string{"--resume=maybe", "/a"}, `--resume is an on/off option: give it on its own, without "=maybe"`},
		{"old option given a value", []string{"/a", "-no-cleanup-balance=sometimes"},
			`-no-cleanup-balance is an on/off option: give it on its own, without "=sometimes"`},
		{"unknown checksum", []string{"--checksum", "foo", "/a"}, `--checksum needs sha256 (the default) or md5, not "foo"`},
		{"negative size threshold", []string{"--size-threshold", "-1", "/a"}, "--size-threshold can't be negative"},
		{"negative concurrency", []string{"--concurrency", "-1", "/a"}, "--concurrency can't be negative"},
		{"unknown option", []string{"--frobnicate", "/a"}, "there's no option called --frobnicate"},
		{"unknown option typed with one dash", []string{"-frobnicate", "/a"}, "there's no option called -frobnicate"},
		{"unknown option with a value", []string{"/a", "--frobnicate=3"}, "there's no option called --frobnicate"},
		{"a folder starting with a dash", []string{"-dash dir"},
			"there's no option called -dash dir. If that's the folder to rebalance, put -- before it, like this: rebalance -- '-dash dir'"},
		{"a path starting with a dash", []string{"-tank/media"},
			"there's no option called -tank/media. If that's the folder to rebalance, put -- before it, like this: rebalance -- -tank/media"},
		{"missing value", []string{"/a", "--db"}, "--db needs a value after it"},
		{"missing value, one dash", []string{"/a", "-db"}, "-db needs a value after it"},
		{"bad syntax", []string{"---x", "/a"}, "there's no option called ---x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseArgs(tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("parseArgs(%q) = %v, want an error containing %q", tt.args, err, tt.want)
			}
			// The flag package's own wording never shows.
			if err != nil && strings.Contains(err.Error(), "flag") {
				t.Errorf("parseArgs(%q) = %v, which talks about flags", tt.args, err)
			}
		})
	}
	if _, err := parseArgs(nil); err != errNoArgs {
		t.Errorf("parseArgs(nil) = %v, want errNoArgs", err)
	}
}

// TestDashFolderHint checks that the hint to put -- before a folder is given only for an argument
// that looks like one.
func TestDashFolderHint(t *testing.T) {
	t.Chdir(tempDir(t))
	if err := os.Mkdir("-photos", 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := parseArgs([]string{"--no-random", "-photos"})
	if err == nil || !strings.HasSuffix(err.Error(), "put -- before it, like this: rebalance -- -photos") {
		t.Errorf("an existing folder: %v", err)
	}
	_, err = parseArgs([]string{"-nope", "/a"})
	if err == nil || strings.Contains(err.Error(), "put --") {
		t.Errorf("a mistyped option got the folder hint: %v", err)
	}
	// Once -- has been given, nothing after it is an option.
	if o, err := parseArgs([]string{"--", "-photos"}); err != nil || o.path != "-photos" {
		t.Errorf("parseArgs(-- -photos) = %+v, %v", o, err)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"-photos":          "-photos",
		"-tank/media_2024": "-tank/media_2024",
		"-dash dir":        "'-dash dir'",
		"-it's":            `'-it'\''s'`,
		"-$HOME":           "'-$HOME'",
		"-日本":              "'-日本'",
		"":                 "''",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestResolveConcurrency(t *testing.T) {
	tests := []struct {
		requested, cpus int
		want            int
		note            bool
	}{
		{0, 1, 2, false},
		{0, 2, 2, false},
		{0, 5, 2, false},
		{0, 16, 8, false},
		{0, 255, 127, false},
		{0, 512, 128, false},
		{1, 64, 1, false},
		{6, 2, 6, false},
		{128, 4, 128, false},
		{129, 4, 128, true},
		{10000, 4, 128, true},
	}
	for _, tt := range tests {
		got, note := resolveConcurrency(tt.requested, tt.cpus)
		if got != tt.want || (note != "") != tt.note {
			t.Errorf("resolveConcurrency(%d, %d) = %d, %q; want %d, note %v", tt.requested, tt.cpus, got, note, tt.want, tt.note)
		}
		if tt.note && !strings.Contains(note, "128") {
			t.Errorf("note %q doesn't say the limit", note)
		}
	}
}

func TestResolveRoot(t *testing.T) {
	dir := tempDir(t)
	writeFile(t, filepath.Join(dir, "file"), "x")
	if err := os.Symlink(dir, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	got, err := resolveRoot(filepath.Join(dir, "link"))
	if err != nil || got != dir {
		t.Errorf("resolveRoot(link) = %q, %v; want %q", got, err, dir)
	}
	t.Chdir(dir)
	if got, err := resolveRoot("."); err != nil || got != dir {
		t.Errorf("resolveRoot(.) = %q, %v; want %q", got, err, dir)
	}
	if _, err := resolveRoot("missing"); err == nil || !strings.Contains(err.Error(), "doesn't exist") {
		t.Errorf("resolveRoot(missing) = %v", err)
	}
	if _, err := resolveRoot("file"); err == nil || !strings.Contains(err.Error(), "is a file, not a folder") {
		t.Errorf("resolveRoot(file) = %v", err)
	}
}
