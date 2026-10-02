// Юнит-тесты LRC-парсера и lyricsList без PG (F3.3, #25).

package subsonic

import (
	"testing"
)

func TestParseLRC(t *testing.T) {
	lrc := "[ar:Artist]\n[offset:+500]\n[00:12.00]First line\n[00:13:25]colon clock\n[01:02.5]Half sec\n[00:12.250]Millis\nno timestamp\n[]empty\n"
	lines := parseLRC(lrc)
	want := []LyricsLine{
		{Start: 12000, Value: "First line"},
		{Start: 13250, Value: "colon clock"}, // [mm:ss:ff] — ff как дробь 25 → 250 мс
		{Start: 62500, Value: "Half sec"},    // .5 → 500 мс
		{Start: 12250, Value: "Millis"},
	}
	if len(lines) != len(want) {
		t.Fatalf("lines=%+v", lines)
	}
	for i, w := range want {
		if lines[i] != w {
			t.Fatalf("line %d = %+v, want %+v", i, lines[i], w)
		}
	}
	if parseLRC("") != nil || parseLRC("plain\ntext") != nil {
		t.Fatal("not LRC must yield nil")
	}
}

func TestBuildLyricsList(t *testing.T) {
	list := buildLyricsList(LyricsRow{
		Artist: "A", Title: "T",
		Plain:  "la\nla la\n",
		Synced: "[00:01.00]sync\n",
	})
	if len(list.StructuredLyrics) != 2 {
		t.Fatalf("entries=%+v", list.StructuredLyrics)
	}
	synced := list.StructuredLyrics[0]
	if !synced.Synced || synced.Lang != "und" || synced.DisplayArtist != "A" ||
		len(synced.Line) != 1 || synced.Line[0].Start != 1000 || synced.Line[0].Value != "sync" {
		t.Fatalf("synced=%+v", synced)
	}
	text := list.StructuredLyrics[1]
	if text.Synced || len(text.Line) != 2 || text.Line[0].Value != "la" {
		t.Fatalf("text=%+v", text)
	}
	// только plain — одна не-synced запись
	list = buildLyricsList(LyricsRow{Plain: "x"})
	if len(list.StructuredLyrics) != 1 || list.StructuredLyrics[0].Synced {
		t.Fatalf("plain only=%+v", list.StructuredLyrics)
	}
}

func TestLRCToPlain(t *testing.T) {
	got := lrcToPlain("[00:01.00]a\n[00:02.00]b\n")
	if got != "a\nb" {
		t.Fatalf("got %q", got)
	}
	if lrcToPlain("no lrc") != "" {
		t.Fatal("non-LRC must be empty")
	}
}
