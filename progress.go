package main

// progress.go holds helpers for live, throttled progress output. Output is
// suppressed when stderr is not a terminal so CI logs and piped output stay
// clean.

import (
	"fmt"
	"os"
	"time"
)

// isTerminal reports whether f is attached to a character device (a TTY).
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// fmtETA renders a remaining-time estimate as a compact human string.
func fmtETA(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
