package bot

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// tidy runs over every text the bot sends and every dictionary line it stores,
// fixing three slips of the model deterministically, since no prompt rule
// stopped them for good:
//   - romaji that doesn't match the kana reading shown next to it —
//     ブルーベリー (buruberii) becomes (buruuberii) — with long vowels in two
//     letters, never macrons: (toiretto pēpā) becomes (toiretto peepaa);
//   - a reading after kana that merely repeats it: の(の)る → のる,
//     ブルーベリー(ぶるーべりー) → ブルーベリー;
//   - Markdown, which the chat shows as-is: ブル**ウ** → ブルウ.
func tidy(s string) string {
	return fixRomaji(dropKanaReadings(stripMarkdown(s)))
}

var (
	mdBold    = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	mdUnder   = regexp.MustCompile(`__([^_\n]+)__`)
	mdHeading = regexp.MustCompile(`(?m)^#{1,4} +`)
	mdBullet  = regexp.MustCompile(`(?m)^\* +`)
)

func stripMarkdown(s string) string {
	s = mdBold.ReplaceAllString(s, "$1")
	s = mdUnder.ReplaceAllString(s, "$1")
	s = strings.ReplaceAll(s, "```", "")
	s = mdHeading.ReplaceAllString(s, "")
	return mdBullet.ReplaceAllString(s, "• ")
}

var kanaThenReadingRe = regexp.MustCompile(`([\p{Hiragana}\p{Katakana}ー]+)[(（]([\p{Hiragana}\p{Katakana}ー]+)[)）]`)

// dropKanaReadings removes a bracketed reading written after kana when it only
// repeats that kana. Readings belong after kanji; after kana they are noise.
func dropKanaReadings(s string) string {
	return kanaThenReadingRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := kanaThenReadingRe.FindStringSubmatch(m)
		if toHiragana(sub[1]) == toHiragana(sub[2]) {
			return sub[1]
		}
		return m
	})
}

// wordWithRomajiRe is a Japanese word — kanji with their readings in brackets
// and kana — followed by romaji in brackets: 寒(さむ)い (samui), コーヒー (koohii).
// A bare kanji without its reading ends the word, so its reading is never guessed.
var wordWithRomajiRe = regexp.MustCompile(
	`((?:[\p{Han}々]+[(（][\p{Hiragana}\p{Katakana}ー]+[)）]|[\p{Hiragana}\p{Katakana}ー])+)([ \t　]*)[(（]([A-Za-zāīūēōâîûêôĀĪŪĒŌ'’ -]+)[)）]`)

var furiganaRe = regexp.MustCompile(`[\p{Han}々]+[(（]([\p{Hiragana}\p{Katakana}ー]+)[)）]`)

// fixRomaji checks the romaji of every single word against the reading shown
// with it and writes it the one agreed way — long vowels in two letters. Whole
// sentences are left alone: particles read differently from how they are
// written (は is wa), so a sentence can't be read back from its kana.
func fixRomaji(s string) string {
	locs := wordWithRomajiRe.FindAllStringSubmatchIndex(s, -1)
	if locs == nil {
		return s
	}
	var sb strings.Builder
	last := 0
	for _, l := range locs {
		start, end := l[0], l[1]
		word, gap, model := s[l[2]:l[3]], s[l[4]:l[5]], s[l[6]:l[7]]
		sb.WriteString(s[last:start])
		last = end
		fixed, ok := romajiFor(s[:start], word, model)
		if !ok {
			sb.WriteString(s[start:end])
			continue
		}
		sb.WriteString(word + gap + s[l[5]:l[6]] + fixed + s[l[7]:end])
	}
	sb.WriteString(s[last:])
	return sb.String()
}

// romajiFor decides the romaji to show after word. ok is false when the word
// is better left as it is: part of a spaced sentence, a kanji without its
// reading just before it, or kana the converter doesn't know.
func romajiFor(before, word, model string) (string, bool) {
	prev := strings.TrimRight(before, " \t　")
	if r, _ := utf8.DecodeLastRuneInString(prev); prev != "" && (containsJapanese(string(r)) || isKanji(r) ||
		strings.ContainsRune("、。・「」ー々", r)) {
		return "", false
	}
	if len(strings.Fields(model)) > 3 {
		return "", false
	}
	want, ok := kanaToRomaji(furiganaRe.ReplaceAllString(word, "$1"))
	if !ok {
		return "", false
	}
	nm, nw := normalizeRomaji(model), normalizeRomaji(want)
	switch {
	case nm == nw:
		return keepSpacing(model, want), true
	case nm == normalizeRomaji(strings.ReplaceAll(want, "ha", "wa")):
		// こんにちは is konnichiwa: a particle-like は the converter reads as ha
		return demacron(model), true
	case len(strings.Fields(model)) == 1:
		return want, true // a single word with the wrong romaji: buruberii → buruuberii
	}
	// several words that don't add up: probably a phrase with a particle, keep it
	return demacron(model), true
}

// keepSpacing writes want with the word breaks the model used, so a right
// «toiretto pēpā» becomes «toiretto peepaa» rather than «toirettopeepaa».
func keepSpacing(model, want string) string {
	segs := strings.Fields(model)
	if len(segs) < 2 || len(normalizeRomaji(want)) != len(want) {
		return want
	}
	var out []string
	pos := 0
	for i, seg := range segs {
		n := len(normalizeRomaji(seg))
		if i == len(segs)-1 {
			n = len(want) - pos
		}
		if n <= 0 || pos+n > len(want) {
			return want
		}
		out = append(out, want[pos:pos+n])
		pos += n
	}
	return strings.Join(out, " ")
}

var macrons = strings.NewReplacer("ā", "aa", "ī", "ii", "ū", "uu", "ē", "ee", "ō", "oo",
	"â", "aa", "î", "ii", "û", "uu", "ê", "ee", "ô", "oo", "Ā", "Aa", "Ī", "Ii", "Ū", "Uu", "Ē", "Ee", "Ō", "Oo")

func demacron(s string) string { return macrons.Replace(s) }

// isRomaji: Latin letters, as written for Japanese — with spaces, macrons,
// apostrophes and hyphens allowed.
func isRomaji(s string) bool {
	if strings.TrimSpace(s) == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || strings.ContainsRune(" '’-āīūēōâîûêôĀĪŪĒŌ", r)) {
			return false
		}
	}
	return true
}
