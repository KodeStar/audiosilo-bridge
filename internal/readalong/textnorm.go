package readalong

import (
	"slices"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// The text side's tokens and sentences: a port of the spike's textnorm.py
// (.readalong-spike/tools/align/textnorm.py). Offsets are RUNE offsets into the
// section's walked text (Python's string indices); epubcfi.Walk.CFI takes them.

// Token is one normalised word of the text and the rune span it came from. A
// number spelled out is several tokens sharing its span.
type Token struct {
	Norm       string
	Start, End int
}

// isWordRune is Python's [^\W_] for a str pattern: a letter or a number.
func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }

// isPySpace is Python's str whitespace (\s, str.isspace): Go's set plus the four
// information separators.
func isPySpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

func isApostrophe(r rune) bool { return r == '\'' || r == '’' }

// Tokens is tokens_with_offsets: TOKEN_RE `[^\W_]+(?:['’][^\W_]+)*` over text, each
// match normalised (NormToken) or, when all ASCII digits, spelled out.
func Tokens(text []rune) []Token {
	var out []Token
	for i := 0; i < len(text); {
		if !isWordRune(text[i]) {
			i++
			continue
		}
		start := i
		for i < len(text) && isWordRune(text[i]) {
			i++
		}
		for i+1 < len(text) && isApostrophe(text[i]) && isWordRune(text[i+1]) {
			i++
			for i < len(text) && isWordRune(text[i]) {
				i++
			}
		}
		raw := string(text[start:i])
		if allDigits(raw) {
			for _, w := range NumberWords(raw) {
				out = append(out, Token{Norm: w, Start: start, End: i})
			}
			continue
		}
		if n := NormToken(raw); n != "" {
			out = append(out, Token{Norm: n, Start: start, End: i})
		}
	}
	return out
}

// allDigits reports whether s is ASCII digits only (Python's isdigit, for the
// digits a book prints; another script's digits stay a word).
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// NormToken is norm_token: NFKD, combining marks dropped, lower case, apostrophes
// removed.
func NormToken(s string) string {
	var b strings.Builder
	for _, r := range norm.NFKD.String(s) {
		if norm.NFKD.PropertiesString(string(r)).CCC() != 0 || isApostrophe(r) {
			continue
		}
		b.WriteString(strings.ToLower(string(r)))
	}
	return b.String()
}

var (
	ones = strings.Fields("zero one two three four five six seven eight nine ten eleven twelve thirteen " +
		"fourteen fifteen sixteen seventeen eighteen nineteen")
	tens = strings.Fields("_ _ twenty thirty forty fifty sixty seventy eighty ninety")
)

func under100(n int) []string {
	if n < 20 {
		return []string{ones[n]}
	}
	if n%10 == 0 {
		return []string{tens[n/10]}
	}
	return []string{tens[n/10], ones[n%10]}
}

func cardinal(n int) []string {
	switch {
	case n < 100:
		return under100(n)
	case n < 1000:
		out := []string{ones[n/100], "hundred"}
		if r := n % 100; r != 0 {
			out = append(append(out, "and"), under100(r)...)
		}
		return out
	case n < 1_000_000:
		out := append(cardinal(n/1000), "thousand")
		if r := n % 1000; r != 0 {
			out = append(out, cardinal(r)...)
		}
		return out
	}
	return []string{strconv.Itoa(n)}
}

// NumberWords is number_words: digits as they are read aloud. A four-digit year
// from 1100 to 1999 (not a whole hundred) is read in pairs ("eighteen oh five");
// a million or more stays its digits.
func NumberWords(digits string) []string {
	trimmed := strings.TrimLeft(digits, "0")
	if trimmed == "" {
		return []string{"zero"}
	}
	if len(trimmed) > 7 {
		return []string{trimmed}
	}
	n, _ := strconv.Atoi(trimmed)
	if len(digits) == 4 && n >= 1100 && n <= 1999 && n%100 != 0 {
		a, b := n/100, n%100
		if b < 10 {
			return append(under100(a), "oh", ones[b])
		}
		return append(under100(a), under100(b)...)
	}
	return cardinal(n)
}

// abbreviations end no sentence (textnorm.py's ABBREV).
var abbreviations = map[string]bool{}

func init() {
	for _, w := range strings.Fields("mr mrs ms dr st mt jr sr messrs mme mlle capt col gen lt rev prof hon no vs etc " +
		"esq sgt maj gov sen rep fig vol ch pp ft") {
		abbreviations[w] = true
	}
}

const (
	sentenceEnders  = ".!?…"
	sentenceClosers = "\"”’')]»"
	sentenceOpeners = "\"“‘'([«"
)

// SplitSentences is split_sentences: block breaks are hard boundaries; inside a
// block a sentence ends after .!?… (and closing quotes) followed by whitespace when
// the next word starts upper case or with a digit (opening quotes skipped), unless
// a single "." follows a known abbreviation or an initial. Returns the rune spans
// trimmed of whitespace that hold a word character.
func SplitSentences(text []rune, breaks []int) [][2]int {
	n := len(text)
	cuts := map[int]bool{0: true, n: true}
	for _, b := range breaks {
		if b > 0 && b < n {
			cuts[b] = true
		}
	}
	for i := 0; i < n; {
		if !strings.ContainsRune(sentenceEnders, text[i]) {
			i++
			continue
		}
		start := i
		for i < n && strings.ContainsRune(sentenceEnders, text[i]) {
			i++
		}
		ender := string(text[start:i])
		for i < n && strings.ContainsRune(sentenceClosers, text[i]) {
			i++
		}
		space := i
		for i < n && isPySpace(text[i]) {
			i++
		}
		if i == space {
			continue // no whitespace: no match starting in this run
		}
		next := i
		if next >= n {
			continue
		}
		j := next
		for j < n && strings.ContainsRune(sentenceOpeners, text[j]) {
			j++
		}
		if j >= n || (!unicode.IsUpper(text[j]) && !unicode.IsDigit(text[j])) {
			continue
		}
		if ender == "." {
			k := start
			for k > 0 && isWordRune(text[k-1]) {
				k--
			}
			if k < start {
				wd := string(text[k:start])
				if abbreviations[strings.ToLower(wd)] || (start-k == 1 && unicode.IsUpper(text[k])) {
					continue
				}
			}
		}
		cuts[space] = true
	}
	sorted := make([]int, 0, len(cuts))
	for c := range cuts {
		sorted = append(sorted, c)
	}
	slices.Sort(sorted)
	var out [][2]int
	for k := 0; k+1 < len(sorted); k++ {
		a, b := sorted[k], sorted[k+1]
		for a < b && isPySpace(text[a]) {
			a++
		}
		for b > a && isPySpace(text[b-1]) {
			b--
		}
		if b <= a {
			continue
		}
		for _, r := range text[a:b] {
			if isWordRune(r) {
				out = append(out, [2]int{a, b})
				break
			}
		}
	}
	return out
}
