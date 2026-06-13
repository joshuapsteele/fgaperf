package main

// color.go keeps terminal styling tiny and opt-in. Styling is only emitted for
// real terminals and is disabled by NO_COLOR, so redirected output stays plain.

import (
	"os"
	"strings"
)

func styleFor(f *os.File, code, s string) string {
	if os.Getenv("NO_COLOR") != "" || !isTerminal(f) {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func boldOut(s string) string   { return styleFor(os.Stdout, "1", s) }
func dimErr(s string) string    { return styleFor(os.Stderr, "2", s) }
func redErr(s string) string    { return styleFor(os.Stderr, "31", s) }
func greenErr(s string) string  { return styleFor(os.Stderr, "32", s) }
func yellowErr(s string) string { return styleFor(os.Stderr, "33", s) }

func stripANSIForTest(s string) string {
	for {
		i := strings.IndexByte(s, '\x1b')
		if i < 0 {
			return s
		}
		j := strings.IndexByte(s[i:], 'm')
		if j < 0 {
			return s[:i]
		}
		s = s[:i] + s[i+j+1:]
	}
}
