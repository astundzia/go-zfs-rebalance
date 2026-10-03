package main

import (
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/astundzia/go-zfs-rebalance/v2/internal/zfs"
	"github.com/sirupsen/logrus"
)

// ANSI colours, used only when writing to a terminal.
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiBlue   = "\x1b[34m"
)

// Log operations ("op" field) added by this command. The engine's own are documented in
// internal/rebalance.
const (
	opProgress = "progress" // a periodic progress line
	opListed   = "listed"   // one path in a list introduced by the line before
)

// newLogger returns the logger for a run: human-friendly lines on w, in colour if w is a terminal.
func newLogger(w io.Writer, debug, filenameOnly bool) *logrus.Logger {
	log := logrus.New()
	log.SetOutput(w)
	log.SetFormatter(&logFormatter{filenameOnly: filenameOnly, color: wantColor(w)})
	if debug {
		log.SetLevel(logrus.DebugLevel)
	}
	return log
}

// wantColor reports whether w is a terminal and the user hasn't asked for no colour
// (https://no-color.org).
func wantColor(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// logFormatter writes one line per log entry, such as
//
//	3:04:05 PM  ✓ rebalanced  photos/IMG 1.jpg  12.3 MiB at 110.5 MB/s
//	3:04:05 PM  ! skipped  photos/b.jpg  the file changed while it was being copied — nothing was changed
//
// It builds the line from the entry's structured fields ("op", "path", "size", "mbps", "reason")
// and never picks apart the message text. All text is escaped, so a file name can't send control
// codes to the terminal.
type logFormatter struct {
	filenameOnly bool
	color        bool
}

// Format implements logrus.Formatter.
func (f *logFormatter) Format(e *logrus.Entry) ([]byte, error) {
	var b strings.Builder
	b.WriteString(f.paint(ansiDim, e.Time.Format("3:04:05 PM")))
	b.WriteString("  ")

	op, _ := e.Data["op"].(string)
	file, hasPath := e.Data["path"].(string)
	reason, _ := e.Data["reason"].(string)
	switch op {
	case "rebalanced":
		b.WriteString(f.paint(ansiGreen, "✓ rebalanced") + "  " + f.renderPath(file))
		if size, ok := e.Data["size"].(int64); ok && size >= 0 {
			b.WriteString("  " + zfs.FormatBytes(uint64(size)))
			// The speed of copying a small file says more about overheads than the pool.
			if mbps, ok := e.Data["mbps"].(float64); ok && mbps > 0 && size >= 1<<20 {
				b.WriteString(" at " + formatRate(mbps*(1<<20)))
			}
		}
	case "skipped":
		b.WriteString(f.paint(ansiYellow, "! skipped") + "  " + f.renderPath(file))
		if reason != "" {
			b.WriteString("  " + escape(reason))
		}
	case "failed":
		b.WriteString(f.paint(ansiRed, "✗ failed") + "  " + f.renderPath(file))
		if reason != "" {
			b.WriteString("  " + escape(reason))
		}
	case "removed-temp":
		b.WriteString("- removed a leftover temporary file  " + f.renderPath(file))
	case opListed:
		b.WriteString("    " + f.renderPath(file))
	case opProgress:
		b.WriteString(f.paint(ansiBlue+ansiBold, escape(e.Message)))
	default:
		symbol, color := levelStyle(e.Level)
		text := escape(e.Message)
		if symbol != "" {
			text = symbol + " " + text
		}
		b.WriteString(f.paint(color, text))
		if hasPath {
			b.WriteString("  " + f.renderPath(file))
		}
		if reason != "" {
			b.WriteString("  " + escape(reason))
		}
	}
	b.WriteByte('\n')
	return []byte(b.String()), nil
}

// levelStyle is the symbol and colour of a line that isn't about one of the engine's operations.
func levelStyle(level logrus.Level) (symbol, color string) {
	switch {
	case level <= logrus.ErrorLevel:
		return "✗", ansiRed
	case level == logrus.WarnLevel:
		return "!", ansiYellow
	case level >= logrus.DebugLevel:
		return "·", ansiDim
	}
	return "", ""
}

// renderPath is how every file path in the log is shown: relative to the folder being rebalanced,
// or only its last part with --filename-only, and always escaped.
func (f *logFormatter) renderPath(rel string) string {
	if f.filenameOnly {
		rel = path.Base(rel)
	}
	return escape(rel)
}

func (f *logFormatter) paint(color, s string) string {
	if !f.color || color == "" {
		return s
	}
	return color + s + ansiReset
}

// escape makes s safe to print on a terminal. Control characters, invalid UTF-8 and invisible or
// direction-changing characters are written as Go escapes (\x1b, \n, \u202e); everything else,
// including spaces and quotes, is left as it is.
func escape(s string) string {
	if !needsEscape(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case unicode.IsPrint(r):
			b.WriteString(s[i : i+size])
		default:
			q := strconv.QuoteRune(r)
			b.WriteString(q[1 : len(q)-1])
		}
		i += size
	}
	return b.String()
}

func needsEscape(s string) bool {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if (r == utf8.RuneError && size == 1) || !unicode.IsPrint(r) {
			return true
		}
		i += size
	}
	return false
}
