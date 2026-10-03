package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

var logTime = time.Date(2026, 10, 3, 15, 4, 5, 0, time.Local)

func format(t *testing.T, f *logFormatter, level logrus.Level, msg string, fields logrus.Fields) string {
	t.Helper()
	e := &logrus.Entry{Logger: logrus.New(), Time: logTime, Level: level, Message: msg, Data: fields}
	out, err := f.Format(e)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestFormatterLines(t *testing.T) {
	tests := []struct {
		name   string
		level  logrus.Level
		msg    string
		fields logrus.Fields
		want   string
	}{
		{"rebalanced", logrus.InfoLevel, "Rebalanced",
			logrus.Fields{"op": "rebalanced", "path": "photos/IMG 1.jpg", "size": int64(12_897_485), "mbps": 100.0},
			"3:04:05 PM  ✓ rebalanced  photos/IMG 1.jpg  12.3 MiB at 104.9 MB/s\n"},
		{"rebalanced small file has no speed", logrus.DebugLevel, "Rebalanced",
			logrus.Fields{"op": "rebalanced", "path": "a.txt", "size": int64(5), "mbps": 0.2},
			"3:04:05 PM  ✓ rebalanced  a.txt  5 B\n"},
		{"skipped", logrus.InfoLevel, "Skipped",
			logrus.Fields{"op": "skipped", "path": "b.jpg", "reason": "the file changed while it was being copied — nothing was changed"},
			"3:04:05 PM  ! skipped  b.jpg  the file changed while it was being copied — nothing was changed\n"},
		{"failed", logrus.ErrorLevel, "Couldn't rebalance",
			logrus.Fields{"op": "failed", "path": "c.jpg", "reason": "couldn't read the file (input/output error) — nothing was changed"},
			"3:04:05 PM  ✗ failed  c.jpg  couldn't read the file (input/output error) — nothing was changed\n"},
		{"removed temp", logrus.InfoLevel, "Removed a temporary file left by an earlier run",
			logrus.Fields{"op": "removed-temp", "path": "d/.zfs-rebalance.0123456789ab.tmp"},
			"3:04:05 PM  - removed a leftover temporary file  d/.zfs-rebalance.0123456789ab.tmp\n"},
		{"warning with path and reason", logrus.WarnLevel, "Couldn't look inside this folder, so the files in it were left alone",
			logrus.Fields{"op": "warning", "path": "private", "reason": "permission denied"},
			"3:04:05 PM  ! Couldn't look inside this folder, so the files in it were left alone  private  permission denied\n"},
		{"debug with path", logrus.DebugLevel, "Stopped before this file was finished; it was left as it was",
			logrus.Fields{"path": "big.iso"},
			"3:04:05 PM  · Stopped before this file was finished; it was left as it was  big.iso\n"},
		{"plain info", logrus.InfoLevel, "Found 3 files to rebalance (13 B).", nil,
			"3:04:05 PM  Found 3 files to rebalance (13 B).\n"},
		{"plain error", logrus.ErrorLevel, "Can't start: another rebalance is already running", nil,
			"3:04:05 PM  ✗ Can't start: another rebalance is already running\n"},
		{"progress", logrus.InfoLevel, "Progress: 1 of 2 files", logrus.Fields{"op": opProgress},
			"3:04:05 PM  Progress: 1 of 2 files\n"},
		{"listed", logrus.WarnLevel, "", logrus.Fields{"op": opListed, "path": "old/x.balance"},
			"3:04:05 PM      old/x.balance\n"},
	}
	f := &logFormatter{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := format(t, f, tt.level, tt.msg, tt.fields); got != tt.want {
				t.Errorf("got  %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestFormatterEscapesText(t *testing.T) {
	f := &logFormatter{}
	evil := "dir/evil\x1b[31mred\nnext\x7f\xff\u202etxt.exe"
	got := format(t, f, logrus.WarnLevel, "line\rbreak", logrus.Fields{"op": "skipped", "path": evil, "reason": "tab\there"})
	want := "3:04:05 PM  ! skipped  dir/evil\\x1b[31mred\\nnext\\x7f\\xff\\u202etxt.exe  tab\\there\n"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	got = format(t, f, logrus.WarnLevel, "line\rbreak", logrus.Fields{"path": "a\nb"})
	if want := "3:04:05 PM  ! line\\rbreak  a\\nb\n"; got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}

	// Printable text, including other scripts, quotes and the replacement character, is unchanged.
	for _, s := range []string{"plain.txt", "Grüße/日本語 \"quoted\" 'x'.jpg", "a\\b", "\uFFFD"} {
		if escape(s) != s {
			t.Errorf("escape(%q) = %q, want it unchanged", s, escape(s))
		}
	}
}

func TestFormatterFilenameOnly(t *testing.T) {
	f := &logFormatter{filenameOnly: true}
	got := format(t, f, logrus.InfoLevel, "Rebalanced", logrus.Fields{"op": "rebalanced", "path": "photos/2024/IMG 1.jpg", "size": int64(3)})
	if want := "3:04:05 PM  ✓ rebalanced  IMG 1.jpg  3 B\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	got = format(t, f, logrus.WarnLevel, "", logrus.Fields{"op": opListed, "path": "a/b/c\n.balance"})
	if want := "3:04:05 PM      c\\n.balance\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatterColor(t *testing.T) {
	f := &logFormatter{color: true}
	got := format(t, f, logrus.InfoLevel, "Rebalanced", logrus.Fields{"op": "rebalanced", "path": "x\x1b[2Jy", "size": int64(1)})
	want := ansiDim + "3:04:05 PM" + ansiReset + "  " + ansiGreen + "✓ rebalanced" + ansiReset + "  x\\x1b[2Jy  1 B\n"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	got = format(t, f, logrus.ErrorLevel, "Couldn't rebalance", logrus.Fields{"op": "failed", "path": "z", "reason": "r"})
	if !strings.Contains(got, ansiRed+"✗ failed"+ansiReset) {
		t.Errorf("failed line isn't red: %q", got)
	}
}

func TestColorOnlyOnATerminal(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	if wantColor(&bytes.Buffer{}) {
		t.Error("colour chosen for a buffer")
	}
	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if wantColor(f) {
		t.Error("colour chosen for a regular file")
	}
	if tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
		defer tty.Close()
		if !wantColor(tty) {
			t.Error("no colour for a terminal")
		}
		t.Setenv("NO_COLOR", "1")
		if wantColor(tty) {
			t.Error("colour chosen although NO_COLOR is set")
		}
	}

	var buf syncBuffer
	newLogger(&buf, false, false).WithFields(logrus.Fields{"op": "failed", "path": "a", "reason": "b"}).Error("x")
	if strings.Contains(buf.String(), "\x1b") {
		t.Errorf("escape codes written to a non-terminal: %q", buf.String())
	}
}

func TestLoggerLevels(t *testing.T) {
	var buf syncBuffer
	log := newLogger(&buf, false, false)
	log.Debug("hidden")
	log.Info("shown")
	if s := buf.String(); strings.Contains(s, "hidden") || !strings.Contains(s, "shown") {
		t.Errorf("default level: %q", s)
	}
	log = newLogger(&buf, true, false)
	log.Debug("now shown")
	if !strings.Contains(buf.String(), "· now shown") {
		t.Errorf("--debug didn't show debug lines: %q", buf.String())
	}
}
