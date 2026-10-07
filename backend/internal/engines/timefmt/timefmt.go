// Package timefmt reads and writes the times devices put in requests and
// answers about their recordings: RFC 3339, Unix seconds or milliseconds,
// or a Go layout such as 2006_01_02_15_04_05 for Dahua.
package timefmt

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Names of the formats that are not layouts.
const (
	RFC3339 = "rfc3339"
	Unix    = "unix"
	UnixMS  = "unix_ms"
)

// Check reports whether a format is known: a name, or a layout with a
// year.
func Check(format string) error {
	switch format {
	case "", RFC3339, Unix, UnixMS:
		return nil
	}
	if !strings.Contains(format, "2006") {
		return fmt.Errorf("time format %q is not rfc3339, unix, unix_ms nor a layout of 2006-01-02 15:04:05", format)
	}
	return nil
}

// Parse reads a time; a layout without a zone is UTC.
func Parse(format, s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, errors.New("no time given")
	}
	switch format {
	case "", RFC3339:
		return time.Parse(time.RFC3339, s)
	case Unix, UnixMS:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("%q is not a number", s)
		}
		if format == UnixMS {
			return time.UnixMilli(n).UTC(), nil
		}
		return time.Unix(n, 0).UTC(), nil
	}
	return time.Parse(format, s)
}

// Format writes a time in UTC.
func Format(format string, t time.Time) string {
	t = t.UTC()
	switch format {
	case "", RFC3339:
		return t.Format(time.RFC3339)
	case Unix:
		return strconv.FormatInt(t.Unix(), 10)
	case UnixMS:
		return strconv.FormatInt(t.UnixMilli(), 10)
	}
	return t.Format(format)
}
