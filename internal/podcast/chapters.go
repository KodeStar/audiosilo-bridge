package podcast

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

type ChapterFile struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	DurationMS int64  `json:"duration_ms"`
}
type Chapter struct {
	Title    string `json:"title"`
	StartMS  int64  `json:"start_ms"`
	LengthMS int64  `json:"length_ms"`
}
type ChapterList struct {
	Version     int           `json:"version"`
	Generator   string        `json:"generator"`
	GeneratedAt string        `json:"generated_at"`
	From        string        `json:"from"`
	Files       []ChapterFile `json:"files"`
	Chapters    []Chapter     `json:"chapters"`
}
type chap struct {
	id, title  string
	start, end int64
}

// ReadCHAP reads only a bounded ID3 tag. Bad optional chapters never reject audio.
func ReadCHAP(r io.Reader, episode string, f Fetch) *ChapterList {
	var head [10]byte
	if _, err := io.ReadFull(r, head[:]); err != nil || string(head[:3]) != "ID3" || head[3] < 3 || head[3] > 4 || head[5]&0x0f != 0 {
		return nil
	}
	n, ok := syncsafe(head[6:10])
	if !ok || n > 16<<20 {
		return nil
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil
	}
	version := int(head[3])
	globalUnsync := head[5]&0x80 != 0
	if globalUnsync && version == 3 {
		b = deunsync(b)
	}
	if head[5]&0x40 != 0 {
		if len(b) < 4 {
			return nil
		}
		size := int(binary.BigEndian.Uint32(b[:4])) + 4
		if version == 4 {
			var valid bool
			size, valid = syncsafe(b[:4])
			if !valid {
				return nil
			}
		}
		if size < 4 || size > len(b) {
			return nil
		}
		b = b[size:]
	}
	var chapters []chap
	valid := frames(b, version, func(id string, flags uint16, body []byte) bool {
		if id != "CHAP" {
			return true
		}
		if len(chapters) >= 2000 {
			return false
		}
		if version == 4 && (globalUnsync || flags&2 != 0) {
			body = deunsync(body)
		}
		if version == 4 && flags&1 != 0 {
			if len(body) < 4 {
				return false
			}
			body = body[4:]
		}
		end := bytes.IndexByte(body, 0)
		if end < 1 || end > 255 || len(body) < end+17 {
			return false
		}
		c := chap{id: string(body[:end]), start: int64(binary.BigEndian.Uint32(body[end+1:])), end: int64(binary.BigEndian.Uint32(body[end+5:]))}
		if !frames(body[end+17:], version, func(id string, flags uint16, text []byte) bool {
			if id == "TIT2" {
				if version == 4 && flags&2 != 0 {
					text = deunsync(text)
				}
				c.title = chapterText(text)
			}
			return true
		}) {
			return false
		}
		chapters = append(chapters, c)
		return true
	})
	if !valid || len(chapters) < 2 {
		return nil
	}
	slices.SortFunc(chapters, func(a, b chap) int {
		if a.start < b.start {
			return -1
		}
		if a.start > b.start {
			return 1
		}
		return 0
	})
	out := &ChapterList{Version: 1, Generator: "audiosilo-bridge", GeneratedAt: f.FetchedAt, From: "id3_chap", Files: []ChapterFile{{path.Base(episode), f.Bytes, f.DurationMS}}, Chapters: []Chapter{}}
	seen := map[string]bool{}
	for i, c := range chapters {
		next := f.DurationMS
		if i+1 < len(chapters) {
			next = chapters[i+1].start
		}
		if c.end == 0xffffffff {
			c.end = next
		}
		if seen[c.id] || i == 0 && c.start != 0 || i > 0 && c.start <= chapters[i-1].start || c.start < 0 || c.end <= c.start || c.end > f.DurationMS || abs(c.end-next) > 1000 {
			return nil
		}
		seen[c.id] = true
		if c.title == "" {
			c.title = fmt.Sprintf("Chapter %d", i+1)
		}
		out.Chapters = append(out.Chapters, Chapter{c.title, c.start, c.end - c.start})
	}
	return out
}
func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
func syncsafe(b []byte) (int, bool) {
	if len(b) != 4 {
		return 0, false
	}
	n := 0
	for _, v := range b {
		if v&128 != 0 {
			return 0, false
		}
		n = n<<7 | int(v)
	}
	return n, true
}
func deunsync(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		out = append(out, b[i])
		if b[i] == 255 && i+1 < len(b) && b[i+1] == 0 {
			i++
		}
	}
	return out
}
func frames(b []byte, v int, visit func(string, uint16, []byte) bool) bool {
	for len(b) > 0 {
		if b[0] == 0 {
			return true
		}
		if len(b) < 10 {
			return false
		}
		id := string(b[:4])
		for _, c := range b[:4] {
			if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
				return false
			}
		}
		size := int(binary.BigEndian.Uint32(b[4:8]))
		if v == 4 {
			var ok bool
			size, ok = syncsafe(b[4:8])
			if !ok {
				return false
			}
		}
		if size > len(b)-10 {
			return false
		}
		flags := binary.BigEndian.Uint16(b[8:10])
		body := b[10 : 10+size]
		skip := v == 3 && flags&0x00e0 != 0 || v == 4 && flags&0x004c != 0
		if !skip && !visit(id, flags, body) {
			return false
		}
		b = b[10+size:]
	}
	return true
}
func chapterText(b []byte) string {
	if len(b) > 1025 {
		b = b[:1025]
		for len(b) > 1 && !utf8.RuneStart(b[len(b)-1]) {
			b = b[:len(b)-1]
		}
	}
	if len(b) < 1 {
		return ""
	}
	var s string
	switch b[0] {
	case 0:
		for _, c := range b[1:] {
			s += string(rune(c))
		}
	case 3:
		if !utf8.Valid(b[1:]) {
			return ""
		}
		s = string(b[1:])
	case 1, 2:
		b0 := b[0]
		b = b[1:]
		var order binary.ByteOrder = binary.BigEndian
		if b0 == 1 {
			if len(b) < 2 {
				return ""
			}
			if b[0] == 255 && b[1] == 254 {
				order = binary.LittleEndian
			} else if b[0] != 254 || b[1] != 255 {
				return ""
			}
			b = b[2:]
		}
		if len(b)%2 != 0 {
			return ""
		}
		u := make([]uint16, 0, len(b)/2)
		for len(b) > 0 {
			u = append(u, order.Uint16(b[:2]))
			b = b[2:]
		}
		s = string(utf16.Decode(u))
	default:
		return ""
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == 0xfffd || r == 0x061c || r == 0x200e || r == 0x200f || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 {
			return -1
		}
		return r
	}, s)
	return Clip(strings.TrimSpace(s), 200)
}

func ValidChapterList(c *ChapterList, episode string, f *Fetch) bool {
	if c == nil || !f.Valid() || c.Version != 1 || c.Generator != "audiosilo-bridge" || c.From != "id3_chap" || c.GeneratedAt != f.FetchedAt || len(c.Files) != 1 || c.Files[0].Path != path.Base(episode) || c.Files[0].Size != f.Bytes || c.Files[0].DurationMS != f.DurationMS || len(c.Chapters) < 2 || len(c.Chapters) > 2000 {
		return false
	}
	for i, ch := range c.Chapters {
		if ch.StartMS < 0 || ch.LengthMS <= 0 || ch.StartMS > f.DurationMS || ch.LengthMS > f.DurationMS-ch.StartMS || ch.Title == "" || len(ch.Title) > 200 || !utf8.ValidString(ch.Title) {
			return false
		}
		if i == 0 && ch.StartMS != 0 {
			return false
		}
		if i > 0 {
			prev := c.Chapters[i-1]
			if ch.StartMS <= prev.StartMS || abs(prev.StartMS+prev.LengthMS-ch.StartMS) > 1000 {
				return false
			}
		}
	}
	last := c.Chapters[len(c.Chapters)-1]
	return abs(f.DurationMS-last.StartMS-last.LengthMS) <= 1000
}
func ReadChapters(root *os.Root, episode string, f *Fetch) *ChapterList {
	rd, err := OpenRegular(root, AttachmentPath(episode, ".chapters.json"))
	if err != nil {
		return nil
	}
	defer rd.Close()
	var c ChapterList
	if json.NewDecoder(io.LimitReader(rd, 4<<20)).Decode(&c) != nil || !ValidChapterList(&c, episode, f) {
		return nil
	}
	return &c
}
