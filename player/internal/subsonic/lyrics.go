// Тексты Subsonic (F3.3, GitLab #25): getLyrics (legacy artist+title →
// plain) и getLyricsBySongId (OpenSubsonic songLyrics v1: structuredLyrics
// с synced=true из LRC и synced=false из plain). Спецификация:
// https://opensubsonic.netlify.app/docs/endpoints/getlyricsbysongid/

package subsonic

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// Lyrics — legacy getLyrics: текст в chardata.
type Lyrics struct {
	Artist string `xml:"artist,attr,omitempty" json:"artist,omitempty"`
	Title  string `xml:"title,attr,omitempty" json:"title,omitempty"`
	Value  string `xml:",chardata" json:"value"`
}

// LyricsList — OpenSubsonic getLyricsBySongId.
type LyricsList struct {
	StructuredLyrics Array[StructuredLyrics] `xml:"structuredLyrics" json:"structuredLyrics"`
}

// StructuredLyrics — одна дорожка текста; synced=true → у line есть start.
type StructuredLyrics struct {
	DisplayArtist string            `xml:"displayArtist,attr,omitempty" json:"displayArtist,omitempty"`
	DisplayTitle  string            `xml:"displayTitle,attr,omitempty" json:"displayTitle,omitempty"`
	Lang          string            `xml:"lang,attr,omitempty" json:"lang,omitempty"`
	Synced        bool              `xml:"synced,attr" json:"synced"`
	Line          Array[LyricsLine] `xml:"line" json:"line"`
}

// LyricsLine — строка текста; start — мс от начала трека (synced).
type LyricsLine struct {
	Start int    `xml:"start,attr,omitempty" json:"start,omitempty"`
	Value string `xml:",chardata" json:"value"`
}

// getLyrics — legacy: artist+title → plain-текст (переводы строк).
func (rt *Router) getLyrics(r *http.Request) (*Subsonic, error) {
	if rt.Catalog == nil {
		return nil, notImplemented("getLyrics")
	}
	q := r.URL.Query()
	artist, title := q.Get("artist"), q.Get("title")
	if artist == "" || title == "" {
		return nil, NewError(ErrorMissingParameter, "required parameters artist and title are missing")
	}
	row, found, err := rt.Catalog.SubsonicLyricsForSong(artist, title)
	if err != nil {
		return nil, err
	}
	resp := rt.NewResponse()
	if !found {
		return resp, nil // пустой <lyrics/> — спецификация допускает
	}
	text := row.Plain
	if text == "" {
		text = lrcToPlain(row.Synced)
	}
	resp.Lyrics = &Lyrics{Artist: row.Artist, Title: row.Title, Value: text}
	return resp, nil
}

// getLyricsBySongId — OpenSubsonic: structuredLyrics из LRC (synced) и
// plain (не synced). Нет текста — пустой lyricsList (ok).
func (rt *Router) getLyricsBySongId(r *http.Request) (*Subsonic, error) {
	if rt.Catalog == nil {
		return nil, notImplemented("getLyricsBySongId")
	}
	id, err := parseEntityID(r, "id")
	if err != nil {
		return nil, err
	}
	row, found, err := rt.Catalog.SubsonicLyricsForTrack(id)
	if err != nil {
		return nil, err
	}
	resp := rt.NewResponse()
	if found {
		resp.LyricsList = buildLyricsList(row)
	} else {
		resp.LyricsList = &LyricsList{} // пустой список, не отсутствующее поле
	}
	return resp, nil
}

// buildLyricsList — plain/LRC → structuredLyrics-записи (lang неизвестен —
// «und», см. спецификацию).
func buildLyricsList(row LyricsRow) *LyricsList {
	list := &LyricsList{}
	base := StructuredLyrics{
		DisplayArtist: row.Artist,
		DisplayTitle:  row.Title,
		Lang:          "und",
	}
	if lines := parseLRC(row.Synced); len(lines) > 0 {
		synced := base
		synced.Synced = true
		synced.Line = lines
		list.StructuredLyrics = append(list.StructuredLyrics, synced)
	}
	if plain := strings.TrimRight(row.Plain, "\n"); plain != "" {
		text := base
		text.Line = plainLines(plain)
		list.StructuredLyrics = append(list.StructuredLyrics, text)
	}
	return list
}

// plainLines — plain-текст → строки без тайминга.
func plainLines(plain string) Array[LyricsLine] {
	out := Array[LyricsLine]{}
	for _, line := range strings.Split(plain, "\n") {
		out = append(out, LyricsLine{Value: strings.TrimRight(line, "\r")})
	}
	return out
}

// lrcLineRe — тайм-код LRC: [mm:ss], [mm:ss.cc], [mm:ss.mmm].
var lrcLineRe = regexp.MustCompile(`^\[(\d{1,3}):(\d{1,2})(?:[.:](\d{1,3}))?\]\s*(.*)$`)

// parseLRC — LRC → строки с start (мс). Строки без тайм-кода пропускаются;
// пустой результат = не LRC.
func parseLRC(lrc string) Array[LyricsLine] {
	if strings.TrimSpace(lrc) == "" {
		return nil
	}
	out := Array[LyricsLine]{}
	for _, raw := range strings.Split(lrc, "\n") {
		m := lrcLineRe.FindStringSubmatch(strings.TrimSpace(raw))
		if m == nil {
			continue // метаданные ([ar:], [offset:]...) и мусор
		}
		mins, _ := strconv.Atoi(m[1])
		secs, _ := strconv.Atoi(m[2])
		frac := 0
		if m[3] != "" {
			frac, _ = strconv.Atoi(m[3])
			for i := len(m[3]); i < 3; i++ {
				frac *= 10 // .5 → 500 мс, .50 → 500 мс
			}
		}
		out = append(out, LyricsLine{
			Start: mins*60000 + secs*1000 + frac,
			Value: m[4],
		})
	}
	if len(out) == 0 {
		return nil // не LRC
	}
	return out
}

// lrcToPlain — LRC без plain-текста → текст для legacy getLyrics.
func lrcToPlain(lrc string) string {
	lines := parseLRC(lrc)
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line.Value)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}
