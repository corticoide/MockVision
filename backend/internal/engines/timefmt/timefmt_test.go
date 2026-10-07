package timefmt

import (
	"testing"
	"time"
)

func TestFormats(t *testing.T) {
	at := time.Date(2026, 10, 7, 14, 30, 5, 0, time.UTC)
	for _, f := range []string{"", RFC3339, Unix, UnixMS, "2006_01_02_15_04_05", "20060102T150405Z"} {
		if err := Check(f); err != nil {
			t.Fatal(err)
		}
		s := Format(f, at)
		got, err := Parse(f, s)
		if err != nil || !got.Equal(at) {
			t.Fatalf("%q: %s read back as %s, %v", f, s, got, err)
		}
	}
	if Format("2006_01_02_15_04_05", at) != "2026_10_07_14_30_05" {
		t.Fatal(Format("2006_01_02_15_04_05", at))
	}
	if Check("dd/mm/yyyy") == nil {
		t.Fatal("a format without a year was accepted")
	}
	if _, err := Parse(Unix, "soon"); err == nil {
		t.Fatal("a word was read as a time")
	}
	if _, err := Parse(RFC3339, ""); err == nil {
		t.Fatal("no time was read as one")
	}
}
