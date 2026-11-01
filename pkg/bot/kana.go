package bot

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"

	storage "language-learning-bot/pkg/storage"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// ---------- kana trainer (/kana) ----------
//
// Drills the two syllabaries in stages. Every level is first read (kana → romaji,
// typed) and then written (romaji → kana, four buttons, so no Japanese keyboard
// is needed); a stage is passed once 90% of its items are known. Levels: 1 the
// basic kana, 2 dakuten, 3 small ゃゅょ, 4 small っ and ー inside short words, 5
// katakana-only syllables for foreign words. Words from the user's kana table
// join in once every kana in them is known. Progress is kept per item and
// direction, so it carries across modes: hiragana learned alone counts in the
// mixed mode too. Wrong options are drawn from look-alike kana and from what the
// user has actually confused before.

const (
	modeKana      = "kana"
	kanaRoundSize = 10
	kanaPassShare = 0.9
)

// Levels 1-3 in hiragana; katakana is derived. «:» separates the readings, «|»
// the accepted spellings, the usual one first.
var kanaSyllables = []struct {
	level int
	list  string
}{
	{1, "あ:a い:i う:u え:e お:o か:ka き:ki く:ku け:ke こ:ko さ:sa し:shi|si す:su せ:se そ:so " +
		"た:ta ち:chi|ti つ:tsu|tu て:te と:to な:na に:ni ぬ:nu ね:ne の:no は:ha ひ:hi ふ:fu|hu へ:he ほ:ho " +
		"ま:ma み:mi む:mu め:me も:mo や:ya ゆ:yu よ:yo ら:ra り:ri る:ru れ:re ろ:ro わ:wa を:wo|o ん:n|nn"},
	{2, "が:ga ぎ:gi ぐ:gu げ:ge ご:go ざ:za じ:ji|zi ず:zu ぜ:ze ぞ:zo だ:da ぢ:ji|di づ:zu|du で:de ど:do " +
		"ば:ba び:bi ぶ:bu べ:be ぼ:bo ぱ:pa ぴ:pi ぷ:pu ぺ:pe ぽ:po"},
	{3, "きゃ:kya きゅ:kyu きょ:kyo しゃ:sha|sya しゅ:shu|syu しょ:sho|syo ちゃ:cha|tya|cya ちゅ:chu|tyu|cyu " +
		"ちょ:cho|tyo|cyo にゃ:nya にゅ:nyu にょ:nyo ひゃ:hya ひゅ:hyu ひょ:hyo みゃ:mya みゅ:myu みょ:myo " +
		"りゃ:rya りゅ:ryu りょ:ryo ぎゃ:gya ぎゅ:gyu ぎょ:gyo じゃ:ja|zya|jya じゅ:ju|zyu|jyu じょ:jo|zyo|jyo " +
		"びゃ:bya びゅ:byu びょ:byo ぴゃ:pya ぴゅ:pyu ぴょ:pyo"},
}

// level 5: katakana-only syllables for foreign words
const kataExtended = "ファ:fa フィ:fi フェ:fe フォ:fo ティ:ti ディ:di トゥ:tu ドゥ:du デュ:dyu ウィ:wi ウェ:we " +
	"ウォ:wo シェ:she ジェ:je チェ:che ヴァ:va ヴィ:vi ヴ:vu ヴェ:ve ヴォ:vo ツァ:tsa"

// level 4: short words where small っ and ー make the difference
const (
	hiraSokuon = "きって きっぷ ちょっと いっぱい はっぱ らっぱ ざっし せっけん がっこう まって みっつ よっつ"
	kataSokuon = "ベッド クッキー サッカー コーヒー ケーキ スープ ノート ボール ゲーム ビール カップ チーズ メール ロボット"
)

// the words of the user's kana table: each joins the drill once all its kana are known
const (
	hiraWords = "あめ いぬ うみ えび おにぎり かさ きのこ くるま けむし こども さかな しか すし せっけん そり " +
		"たまご ちず つき てがみ とり なす にんじん ぬいぐるみ ねこ のこぎり はな ふね へび ほし まど みみ " +
		"むぎ もも やま ゆき よる らっぱ りんご るす れっしゃ ろうそく わに がっこう ぎゅうにゅう ぐんて " +
		"げんかん ごはん ざっし じてんしゃ ずつう ぜに ぞう だんご はなぢ つづく でんわ どんぐり ばら " +
		"びょういん ぶた べんとう ぼうし ぱちぱち ぴかぴか ぷくぷく ぺこぺこ ぽかぽか"
	kataWords = "アイス イヤホン ウイスキー エレベーター オレンジ カメラ キウイ クッキー ケーキ コーヒー サッカー " +
		"シャツ スプーン セーター ソファ タクシー チーズ ツリー テレビ トマト ナイフ ニンジャ ヌードル ネクタイ " +
		"ノート ハンバーガー ヒーロー フォーク ヘリコプター ホテル マイク ミュージック ムービー メロン モンスター " +
		"ヤシ ユニコーン ヨット ライオン リボン ルーレット レモン ロボット ワイン ガソリン ギター グラス ゲーム " +
		"ゴリラ ザリガニ ジーンズ ズッキーニ ゼロ ゾンビ ダイヤ デザート ドーナツ バナナ ビール ブーツ ベッド " +
		"ボール パン ピザ プレゼント ペンギン ポテト"
)

// kanaLookalikes are groups of kana that get mixed up; wrong options are drawn
// from them first, so a right answer means telling the shapes apart.
var kanaLookalikes = []string{
	"あおめぬ", "ぬめねわれ", "るろ", "いりこ", "うらつ", "さちき", "はほけ", "まもほ", "しつ", "くへ",
	"たなに", "ゆよ", "そん", "をおち",
	"シツンソ", "ノソンリ", "クタケ", "ウワフヲラ", "コユロエ", "チテナ", "ヌスメナ", "マムアヤ",
	"ハヘ", "セヒ", "ミシニ", "キサ", "ネホオ", "レル",
}

// kanaVoicing groups the same shape with its ゛ and ゜ marks.
const kanaVoicing = "かが きぎ くぐ けげ こご さざ しじ すず せぜ そぞ ただ ちぢ つづ てで とど はばぱ ひびぴ ふぶぷ へべぺ ほぼぽ"

type kanaItem struct {
	Kana    string   // as shown: ぬ, ヌ, きゃ, コーヒー; a pair item is ぬヌ
	Romaji  []string // accepted readings, the usual one first
	Script  byte     // 'h' hiragana, 'k' katakana, 'p' a hiragana–katakana pair
	Level   int      // 1-5; 0 for a word from the user's kana table
	Word    bool     // a whole word: compared after romaji normalisation
	NoWrite bool     // ぢ/づ read exactly like じ/ず: never asked from the reading
	Hira    string   // pair items: the hiragana half
	Kata    string   // pair items: the katakana half
}

var (
	kanaItems      []*kanaItem
	kanaByKana     = map[string]*kanaItem{}
	syllableRomaji = map[string]string{}          // hiragana syllable → usual romaji, to read whole words
	kanaByReading  = map[byte]map[string]string{} // script → romaji → kana, to name a confusion
)

func init() {
	add := func(it *kanaItem) {
		if _, dup := kanaByKana[it.Kana]; dup {
			return
		}
		kanaItems = append(kanaItems, it)
		kanaByKana[it.Kana] = it
		if it.Script != 'p' && !it.Word {
			if kanaByReading[it.Script] == nil {
				kanaByReading[it.Script] = map[string]string{}
			}
			for _, r := range it.Romaji {
				if _, taken := kanaByReading[it.Script][r]; !taken {
					kanaByReading[it.Script][r] = it.Kana
				}
			}
		}
	}
	for _, lv := range kanaSyllables {
		for _, f := range strings.Fields(lv.list) {
			hira, rom, _ := strings.Cut(f, ":")
			romaji, kata := strings.Split(rom, "|"), toKatakana(hira)
			noWrite := hira == "ぢ" || hira == "づ"
			syllableRomaji[hira] = romaji[0]
			add(&kanaItem{Kana: hira, Romaji: romaji, Script: 'h', Level: lv.level, NoWrite: noWrite})
			add(&kanaItem{Kana: kata, Romaji: romaji, Script: 'k', Level: lv.level, NoWrite: noWrite})
			if !noWrite {
				add(&kanaItem{Kana: hira + kata, Romaji: romaji, Script: 'p', Level: lv.level, Hira: hira, Kata: kata})
			}
		}
	}
	for _, f := range strings.Fields(kataExtended) {
		kata, rom, _ := strings.Cut(f, ":")
		romaji := strings.Split(rom, "|")
		syllableRomaji[toHiragana(kata)] = romaji[0]
		add(&kanaItem{Kana: kata, Romaji: romaji, Script: 'k', Level: 5})
	}
	for _, w := range []struct {
		list   string
		script byte
		level  int
	}{{hiraSokuon, 'h', 4}, {kataSokuon, 'k', 4}, {hiraWords, 'h', 0}, {kataWords, 'k', 0}} {
		for _, word := range strings.Fields(w.list) {
			r, ok := kanaToRomaji(word)
			if !ok {
				log.Printf("kana: cannot read %q", word)
				continue
			}
			add(&kanaItem{Kana: word, Romaji: []string{r}, Script: w.script, Level: w.level, Word: true})
		}
	}
}

// ---------- reading kana ----------

func toKatakana(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 0x3041 && r <= 0x3096 {
			return r + 0x60
		}
		return r
	}, s)
}

func toHiragana(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 0x30A1 && r <= 0x30F6 {
			return r - 0x60
		}
		return r
	}, s)
}

// smallKana merge with the kana before them into one syllable: きゃ, ファ.
const smallKana = "ゃゅょぁぃぅぇぉゎ"

// kanaUnits splits a word into syllables as it is read: きって → き っ て,
// ミュージック → ミュ ー ジ ッ ク.
func kanaUnits(word string) []string {
	rs := []rune(word)
	var out []string
	for i := 0; i < len(rs); i++ {
		u := string(rs[i])
		if i+1 < len(rs) && strings.Contains(smallKana, toHiragana(string(rs[i+1]))) {
			u += string(rs[i+1])
			i++
		}
		out = append(out, u)
	}
	return out
}

// kanaToRomaji reads a word in either script as Hepburn romaji, writing a long
// vowel twice: コーヒー → koohii, がっこう → gakkou. Reports false if the word
// holds anything it cannot read.
func kanaToRomaji(word string) (string, bool) {
	units := kanaUnits(toHiragana(word))
	var sb strings.Builder
	double := false
	for i, u := range units {
		switch u {
		case "っ":
			double = true
			continue
		case "ー":
			s := sb.String()
			if s == "" {
				return "", false
			}
			sb.WriteByte(s[len(s)-1])
			continue
		}
		r, ok := syllableRomaji[u]
		if !ok {
			return "", false
		}
		if u == "ん" && i+1 < len(units) {
			if next := syllableRomaji[units[i+1]]; next != "" && strings.ContainsRune("aiueoy", rune(next[0])) {
				r = "n'"
			}
		}
		if double {
			if strings.HasPrefix(r, "ch") {
				sb.WriteByte('t')
			} else {
				sb.WriteByte(r[0])
			}
			double = false
		}
		sb.WriteString(r)
	}
	return sb.String(), sb.Len() > 0 && !double
}

var (
	romajiMarks = strings.NewReplacer("ā", "aa", "ī", "ii", "ū", "uu", "ē", "ee", "ō", "oo",
		"â", "aa", "î", "ii", "û", "uu", "ê", "ee", "ô", "oo", "'", "", "’", "", "-", "", " ", "")
	romajiGlides    = strings.NewReplacer("sy", "sh", "ty", "ch", "cy", "ch", "zy", "j", "jy", "j")
	romajiKunrei    = strings.NewReplacer("si", "shi", "ti", "chi", "tu", "tsu", "zi", "ji")
	romajiLong      = strings.NewReplacer("ou", "oo", "mb", "nb", "mp", "np", "cch", "tch")
	romajiHuRe      = regexp.MustCompile(`(^|[^sc])hu`)
	romajiDoubleNRe = regexp.MustCompile(`nn([^aiueoy]|$)`)
)

// normalizeRomaji brings a romanised word to one spelling, so the usual ways of
// writing it compare equal: macrons or doubled vowels, ou or oo, Kunrei si/ti/tu
// or Hepburn shi/chi/tsu, m or n before b and p.
func normalizeRomaji(s string) string {
	s = romajiMarks.Replace(strings.ToLower(strings.TrimSpace(s)))
	s = romajiKunrei.Replace(romajiGlides.Replace(s))
	s = romajiHuRe.ReplaceAllString(s, "${1}fu")
	s = romajiDoubleNRe.ReplaceAllString(s, "n$1")
	return romajiLong.Replace(s)
}

// kanaReadingOK checks a typed reading: a syllable against its accepted
// spellings exactly (ティ is "ti", but "chi" would be チ), a word after
// normalisation.
func kanaReadingOK(it *kanaItem, answer string) bool {
	a := strings.ToLower(strings.Trim(strings.TrimSpace(answer), ".,!?«»\"'"))
	for _, r := range it.Romaji {
		if a == r || (it.Word && normalizeRomaji(a) == normalizeRomaji(r)) {
			return true
		}
	}
	return false
}

func sameReading(a, b *kanaItem) bool {
	for _, x := range a.Romaji {
		for _, y := range b.Romaji {
			if x == y || (a.Word && b.Word && normalizeRomaji(x) == normalizeRomaji(y)) {
				return true
			}
		}
	}
	return false
}

// ---------- wrong options ----------

// kanaLookalikesOf lists kana easily mistaken for this one: the look-alike group
// and the same shape with other marks; for a syllable like きゃ also the other
// small kana and the look-alikes of its first kana.
func kanaLookalikesOf(kana string) []string {
	rs := []rune(kana)
	var out []string
	if len(rs) == 1 {
		for _, g := range kanaLookalikes { // look-alikes are listed per script
			if strings.Contains(g, kana) {
				for _, r := range g {
					if string(r) != kana {
						out = append(out, string(r))
					}
				}
			}
		}
		// the same shape with other marks — the families are written in
		// hiragana, katakana in means katakana out
		h := toHiragana(kana)
		for _, fam := range strings.Fields(kanaVoicing) {
			if !strings.Contains(fam, h) {
				continue
			}
			for _, r := range fam {
				k := string(r)
				if h != kana {
					k = toKatakana(k)
				}
				if k != kana {
					out = append(out, k)
				}
			}
		}
		return out
	}
	base, small := string(rs[0]), string(rs[1:])
	for _, set := range []string{"ゃゅょ", "ぁぃぅぇぉ"} {
		if strings.Contains(set, toHiragana(small)) {
			for _, s := range set {
				k := string(s)
				if toHiragana(small) != small {
					k = toKatakana(k)
				}
				if k != small {
					out = append(out, base+k)
				}
			}
		}
	}
	for _, b := range kanaLookalikesOf(base) {
		out = append(out, b+small)
	}
	return out
}

// kanaWordVariants are misspellings of a level-4 word that differ exactly where
// the level is about: っ dropped or written full size, ー dropped or added.
func kanaWordVariants(word string) []string {
	rs := []rune(word)
	var out []string
	for i, r := range rs {
		rest := string(rs[i+1:])
		switch r {
		case 'っ', 'ッ':
			out = append(out, string(rs[:i])+rest, string(rs[:i])+string(r+1)+rest) // きて, きつて
		case 'ー':
			out = append(out, string(rs[:i])+rest)
		}
	}
	if units := kanaUnits(word); toHiragana(word) != word && len(units) > 1 && units[1] != "ー" && units[1] != "ッ" {
		out = append(out, units[0]+"ー"+strings.Join(units[1:], ""))
	}
	return out
}

// kanaWriteOptions are four buttons for romaji → kana: the right one, then what
// the user mistook it for before, look-alikes, and others of the same level and
// script — never one that reads the same.
func kanaWriteOptions(target *kanaItem, confused string) ([]string, int) {
	taken := map[string]bool{target.Kana: true}
	var picks []string
	addItem := func(k string) {
		it := kanaByKana[k]
		if len(picks) == 3 || taken[k] || it == nil || it.Script != target.Script || it.Word != target.Word ||
			it.Level > target.Level || it.Level == 0 || sameReading(it, target) {
			return
		}
		taken[k] = true
		picks = append(picks, k)
	}
	addItem(confused)
	var cands []string
	if target.Word {
		for _, v := range shuffled(kanaWordVariants(target.Kana)) {
			if len(picks) < 3 && !taken[v] {
				taken[v] = true
				picks = append(picks, v)
			}
		}
	} else {
		cands = shuffled(kanaLookalikesOf(target.Kana))
	}
	var same, lower []string
	for _, it := range kanaItems {
		if it.Script == target.Script && it.Word == target.Word {
			if it.Level == target.Level {
				same = append(same, it.Kana)
			} else if it.Level < target.Level {
				lower = append(lower, it.Kana)
			}
		}
	}
	for _, k := range append(append(cands, shuffled(same)...), shuffled(lower)...) {
		addItem(k)
	}
	opts := shuffled(append(picks, target.Kana))
	for i, o := range opts {
		if o == target.Kana {
			return opts, i
		}
	}
	return opts, 0
}

// kanaPairOptions are four hiragana–katakana pairs for «nu → ぬ ヌ»: the right
// one, one with the wrong hiragana, one with the wrong katakana, and the pair of
// a look-alike sound — so only knowing both halves gets it right.
func kanaPairOptions(it *kanaItem) ([]string, int) {
	hOpts, hRight := kanaWriteOptions(kanaByKana[it.Hira], "")
	kOpts, kRight := kanaWriteOptions(kanaByKana[it.Kata], "")
	var hAlt, kAlt []string
	for i, o := range hOpts {
		if i != hRight {
			hAlt = append(hAlt, o)
		}
	}
	for i, o := range kOpts {
		if i != kRight {
			kAlt = append(kAlt, o)
		}
	}
	right := it.Hira + " " + it.Kata
	opts := []string{right}
	if len(hAlt) >= 2 && len(kAlt) >= 1 {
		opts = append(opts, hAlt[0]+" "+it.Kata, it.Hira+" "+kAlt[0], hAlt[1]+" "+toKatakana(hAlt[1]))
	}
	opts = shuffled(opts)
	for i, o := range opts {
		if o == right {
			return opts, i
		}
	}
	return opts, 0
}

func shuffled(s []string) []string {
	out := append([]string(nil), s...)
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// ---------- stages and progress ----------

type kanaStage struct {
	Level int
	Dir   string // read, write or pair
}

type kanaMode struct {
	Title   string
	Scripts string // which items take part: h, k, hk or p
	Stages  []kanaStage
}

func readThenWrite(levels int) []kanaStage {
	var out []kanaStage
	for l := 1; l <= levels; l++ {
		out = append(out, kanaStage{l, "read"}, kanaStage{l, "write"})
	}
	return out
}

var kanaModes = map[string]kanaMode{
	"hiragana": {"Хирагана", "h", readThenWrite(4)},
	"katakana": {"Катакана", "k", readThenWrite(5)},
	"mixed":    {"Вся кана", "hk", readThenWrite(5)},
	"pairs":    {"Пары あ↔ア", "p", []kanaStage{{1, "pair"}, {2, "pair"}, {3, "pair"}}},
}

var kanaLevelNames = [...]string{"", "основные знаки", "знаки с ゛ и ゜", "слоги с маленькими ゃ ゅ ょ",
	"маленькая っ и ー в словах", "особые слоги катаканы: ファ, ティ…"}

var kanaDirNames = map[string]string{"read": "знак → чтение", "write": "чтение → знак", "pair": "хирагана ↔ катакана"}

func kanaKey(dir, kana string) string { return dir + "|" + kana }

// kanaKnown: two right answers in a row, or never missed at all — someone who
// already knows a kana shouldn't have to prove it twice.
func kanaKnown(s storage.KanaStat) bool {
	return s.Streak >= 2 || (s.Seen > 0 && s.Correct == s.Seen)
}

func stageItems(m kanaMode, s kanaStage) []*kanaItem {
	var out []*kanaItem
	for _, it := range kanaItems {
		if it.Level == s.Level && strings.IndexByte(m.Scripts, it.Script) >= 0 && !(s.Dir == "write" && it.NoWrite) {
			out = append(out, it)
		}
	}
	return out
}

func stageKnown(m kanaMode, s kanaStage, prog map[string]storage.KanaStat) (known, total int) {
	items := stageItems(m, s)
	for _, it := range items {
		if kanaKnown(prog[kanaKey(s.Dir, it.Kana)]) {
			known++
		}
	}
	return known, len(items)
}

func stagePassed(m kanaMode, s kanaStage, prog map[string]storage.KanaStat) bool {
	known, total := stageKnown(m, s, prog)
	return float64(known) >= kanaPassShare*float64(total)
}

// advanceKanaStage moves past every stage already passed — also ones passed in
// another mode, since progress is shared.
func advanceKanaStage(m kanaMode, stage int, prog map[string]storage.KanaStat) int {
	for stage < len(m.Stages) && stagePassed(m, m.Stages[stage], prog) {
		stage++
	}
	return stage
}

// kanaWordUnlocked: every kana of the word is known when read, and for a word
// with っ or ー the level that teaches them is passed.
func kanaWordUnlocked(w *kanaItem, prog map[string]storage.KanaStat) bool {
	for _, u := range kanaUnits(w.Kana) {
		if u == "っ" || u == "ッ" || u == "ー" {
			own := map[byte]string{'h': "hiragana", 'k': "katakana"}[w.Script]
			if !stagePassed(kanaModes[own], kanaStage{4, "read"}, prog) {
				return false
			}
			continue
		}
		if kanaByKana[u] == nil || !kanaKnown(prog[kanaKey("read", u)]) {
			return false
		}
	}
	return true
}

func unlockedKanaWords(m kanaMode, prog map[string]storage.KanaStat) []*kanaItem {
	var out []*kanaItem
	for _, it := range kanaItems {
		if it.Level == 0 && strings.IndexByte(m.Scripts, it.Script) >= 0 && kanaWordUnlocked(it, prog) {
			out = append(out, it)
		}
	}
	return out
}

// pickKanaItem favours what isn't known yet: new items three times as often as
// known ones, items being learned six times. The last question is not repeated.
func pickKanaItem(pool []*kanaItem, dir string, prog map[string]storage.KanaStat, last string) *kanaItem {
	weights := make([]int, len(pool))
	total := 0
	for i, it := range pool {
		if it.Kana == last && len(pool) > 1 {
			continue
		}
		s := prog[kanaKey(dir, it.Kana)]
		switch {
		case s.Seen == 0:
			weights[i] = 3
		case !kanaKnown(s):
			weights[i] = 6
		default:
			weights[i] = 1
		}
		total += weights[i]
	}
	if total == 0 {
		return nil
	}
	n := rand.IntN(total)
	for i, w := range weights {
		if n < w {
			return pool[i]
		}
		n -= w
	}
	return nil
}

// nextKanaQuestion mostly drills the current stage, now and then reviews an
// earlier one or reads an unlocked word; once everything is passed, all of it
// comes mixed.
func nextKanaQuestion(m kanaMode, stage int, prog map[string]storage.KanaStat, last string) (*kanaItem, string) {
	if words := unlockedKanaWords(m, prog); len(words) > 0 && rand.Float64() < 0.15 {
		if it := pickKanaItem(words, "read", prog, last); it != nil {
			return it, "read"
		}
	}
	s := stage
	switch {
	case stage >= len(m.Stages):
		s = rand.IntN(len(m.Stages))
	case stage > 0 && rand.Float64() < 0.25:
		s = rand.IntN(stage)
	}
	st := m.Stages[s]
	if it := pickKanaItem(stageItems(m, st), st.Dir, prog, last); it != nil {
		return it, st.Dir
	}
	return nil, ""
}

// ---------- the round ----------

// kanaState is the open question, kept as JSON in user_state.task_text.
type kanaState struct {
	Mode  string   `json:"m"`
	Stage int      `json:"s"`
	Item  string   `json:"i"`
	Dir   string   `json:"d"`
	Ask   string   `json:"a,omitempty"` // pair questions: h2k, k2h or r2p
	Opts  []string `json:"o,omitempty"`
	Right int      `json:"r"`
	Asked int      `json:"n"` // questions answered this round
	OK    int      `json:"k"` // of them right
	Msg   int      `json:"g"` // the question's message, to take its buttons away once answered
}

func kanaModeKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Хирагана", "kana_mode#hiragana"),
			tgbotapi.NewInlineKeyboardButtonData("Катакана", "kana_mode#katakana"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Пары あ↔ア", "kana_mode#pairs"),
			tgbotapi.NewInlineKeyboardButtonData("Вся кана", "kana_mode#mixed"),
		),
	)
}

// kanaQuestionKeyboard — the options two by two, then «Не знаю» and «Закончить».
// The question number in each button keeps a tap on an old question harmless.
func kanaQuestionKeyboard(ks kanaState) tgbotapi.InlineKeyboardMarkup {
	var rows [][]tgbotapi.InlineKeyboardButton
	for i := 0; i < len(ks.Opts); i += 2 {
		row := tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(ks.Opts[i], fmt.Sprintf("kana_pick#%d#%d", ks.Asked, i)))
		if i+1 < len(ks.Opts) {
			row = append(row, tgbotapi.NewInlineKeyboardButtonData(ks.Opts[i+1], fmt.Sprintf("kana_pick#%d#%d", ks.Asked, i+1)))
		}
		rows = append(rows, row)
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("Не знаю", fmt.Sprintf("kana_skip#%d", ks.Asked)),
		tgbotapi.NewInlineKeyboardButtonData("Закончить", "exit"),
	))
	return tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func kanaRoundKeyboard(mode string) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("Ещё 10", "kana_more#"+mode),
		tgbotapi.NewInlineKeyboardButtonData("Закончить", "exit"),
	))
}

// kanaStageLine is «уровень 1 — основные знаки · знак → чтение · выучено 12 из 46».
func kanaStageLine(m kanaMode, stage int, prog map[string]storage.KanaStat) string {
	if stage >= len(m.Stages) {
		return "Все уровни пройдены — повторяем всё вразнобой."
	}
	s := m.Stages[stage]
	known, total := stageKnown(m, s, prog)
	return fmt.Sprintf("Уровень %d — %s\n%s · выучено %d из %d", s.Level, kanaLevelNames[s.Level],
		kanaDirNames[s.Dir], known, total)
}

// startKana is /kana: choose what to train.
func startKana(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64, userID int) {
	if reminderPending(bot, db, chatID, userID) {
		return
	}
	sendKb(bot, chatID, "Тренировка каны. Что тренируем?", kanaModeKeyboard())
}

func startKanaRound(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64, userID int, mode string) {
	m, ok := kanaModes[mode]
	if !ok || reminderPending(bot, db, chatID, userID) {
		return
	}
	prog, err := storage.KanaProgress(db, userID)
	if err != nil {
		log.Printf("kana progress: %v", err)
		send(bot, chatID, "Не получилось загрузить прогресс, попробуй ещё раз.")
		return
	}
	stage := advanceKanaStage(m, storage.GetKanaStage(db, userID, mode), prog)
	storage.SetKanaStage(db, userID, mode, stage)
	hint := "Знак → чтение: пиши латиницей. Чтение → знак: выбирай кнопкой."
	if mode == "pairs" {
		hint = "Выбирай ответ кнопкой."
	}
	askKana(bot, db, chatID, userID, kanaState{Mode: mode, Stage: stage}, prog,
		m.Title+"\n"+kanaStageLine(m, stage, prog)+"\n"+hint+"\n\n")
}

// askKana picks the next question, sends it and keeps it in the user's state.
func askKana(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64, userID int, ks kanaState,
	prog map[string]storage.KanaStat, prefix string) {
	m := kanaModes[ks.Mode]
	it, dir := nextKanaQuestion(m, ks.Stage, prog, ks.Item)
	if it == nil {
		storage.ClearMode(db, userID)
		send(bot, chatID, prefix+"Не нашёл, что спросить. /kana — начать заново.")
		return
	}
	q := composeKanaQuestion(&ks, it, dir, prog[kanaKey(dir, it.Kana)].Confused)
	ks.Msg = sendKbID(bot, chatID, prefix+q, kanaQuestionKeyboard(ks))
	data, _ := json.Marshal(ks)
	storage.SetState(db, userID, modeKana, ks.Mode, string(data), 0)
}

func composeKanaQuestion(ks *kanaState, it *kanaItem, dir, confused string) string {
	ks.Item, ks.Dir, ks.Ask, ks.Opts, ks.Right = it.Kana, dir, "", nil, 0
	switch dir {
	case "read":
		if it.Word {
			return "Как читается слово: " + it.Kana
		}
		return "Как читается: " + it.Kana
	case "write":
		ks.Opts, ks.Right = kanaWriteOptions(it, confused)
		hint := ""
		if ks.Mode == "mixed" {
			hint = map[byte]string{'h': " хираганой", 'k': " катаканой"}[it.Script]
		}
		return "Как пишется «" + it.Romaji[0] + "»" + hint + "?"
	}
	switch rand.IntN(3) {
	case 0:
		ks.Ask = "h2k"
		ks.Opts, ks.Right = kanaWriteOptions(kanaByKana[it.Kata], confused)
		return it.Hira + " — а катаканой?"
	case 1:
		ks.Ask = "k2h"
		ks.Opts, ks.Right = kanaWriteOptions(kanaByKana[it.Hira], confused)
		return it.Kata + " — а хираганой?"
	}
	ks.Ask = "r2p"
	ks.Opts, ks.Right = kanaPairOptions(it)
	return "«" + it.Romaji[0] + "» — хираганой и катаканой?"
}

// kanaVerdict is the line shown after an answer: the right answer always, and
// what the wrong one actually was, so a confusion turns into a contrast.
func kanaVerdict(ks kanaState, ok, skipped bool, answer string) string {
	it := kanaByKana[ks.Item]
	mark := "✗ "
	switch {
	case ok:
		mark = "✓ "
	case skipped:
		mark = ""
	}
	other := func(k string) string {
		if o := kanaByKana[k]; o != nil && !ok && !skipped && k != "" {
			return fmt.Sprintf(" А %s — это «%s».", k, o.Romaji[0])
		}
		return ""
	}
	switch {
	case ks.Dir == "read":
		line := mark + it.Kana + " — " + it.Romaji[0] + "."
		if k := kanaByReading[it.Script][strings.ToLower(strings.TrimSpace(answer))]; k != "" && !ok && !skipped && !it.Word {
			line += fmt.Sprintf(" А «%s» — это %s.", strings.ToLower(strings.TrimSpace(answer)), k)
		}
		return line
	case ks.Dir == "write":
		return mark + "«" + it.Romaji[0] + "» — это " + it.Kana + "." + other(answer)
	case ks.Ask == "h2k":
		return mark + it.Hira + " — это " + it.Kata + " (" + it.Romaji[0] + ")." + other(answer)
	case ks.Ask == "k2h":
		return mark + it.Kata + " — это " + it.Hira + " (" + it.Romaji[0] + ")." + other(answer)
	}
	return mark + "«" + it.Romaji[0] + "» — это " + it.Hira + " " + it.Kata + "."
}

// handleKanaText takes a typed answer: the reading for a read question, or a
// kana that matches one of the buttons for anyone with a Japanese keyboard.
func handleKanaText(bot *tgbotapi.BotAPI, db *sql.DB, message *tgbotapi.Message, st storage.State) {
	chatID, userID := message.Chat.ID, int(message.From.ID)
	var ks kanaState
	if json.Unmarshal([]byte(st.TaskText), &ks) != nil || kanaByKana[ks.Item] == nil {
		storage.ClearMode(db, userID)
		send(bot, chatID, "Тренировка сбилась. /kana — начать заново.")
		return
	}
	text := strings.TrimSpace(message.Text)
	if ks.Dir == "read" {
		switch {
		case containsCyrillic(text):
			send(bot, chatID, "Сейчас тренировка каны: напиши чтение латиницей или нажми «Закончить».")
			return
		case containsJapanese(text):
			send(bot, chatID, "Напиши, как это читается, латиницей: например, nu.")
			return
		}
		removeKeyboard(bot, chatID, ks.Msg)
		it := kanaByKana[ks.Item]
		ok := kanaReadingOK(it, text)
		confused := ""
		if !ok && !it.Word {
			confused = kanaByReading[it.Script][strings.ToLower(text)]
		}
		finishKanaAnswer(bot, db, chatID, userID, ks, ok, confused, kanaVerdict(ks, ok, false, text))
		return
	}
	for i, o := range ks.Opts {
		if text == o || text == strings.ReplaceAll(o, " ", "") {
			removeKeyboard(bot, chatID, ks.Msg)
			finishKanaAnswer(bot, db, chatID, userID, ks, i == ks.Right, kanaConfusion(ks, o),
				kanaVerdict(ks, i == ks.Right, false, o))
			return
		}
	}
	send(bot, chatID, "Выбери ответ кнопкой под вопросом.")
}

// handleKanaButton takes «kana_pick#<question>#<option>» and «kana_skip#<question>».
func handleKanaButton(bot *tgbotapi.BotAPI, db *sql.DB, cq *tgbotapi.CallbackQuery, data string) {
	chatID, userID := cq.Message.Chat.ID, int(cq.From.ID)
	st := storage.GetState(db, userID)
	var ks kanaState
	if st.Mode != modeKana || json.Unmarshal([]byte(st.TaskText), &ks) != nil || kanaByKana[ks.Item] == nil {
		send(bot, chatID, "Это уже неактуально. /kana — начать заново.")
		return
	}
	parts := strings.Split(data, "#")
	if len(parts) < 2 || parts[1] != strconv.Itoa(ks.Asked) {
		return // a tap on an old question
	}
	removeKeyboard(bot, chatID, cq.Message.MessageID)
	if parts[0] == "kana_skip" {
		finishKanaAnswer(bot, db, chatID, userID, ks, false, "", kanaVerdict(ks, false, true, ""))
		return
	}
	i, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil || i < 0 || i >= len(ks.Opts) {
		return
	}
	ok := i == ks.Right
	finishKanaAnswer(bot, db, chatID, userID, ks, ok, kanaConfusion(ks, ks.Opts[i]), kanaVerdict(ks, ok, false, ks.Opts[i]))
}

// kanaConfusion is what to remember a wrong pick as: the chosen kana, unless it
// was a whole pair or a made-up spelling of a word.
func kanaConfusion(ks kanaState, picked string) string {
	if ks.Ask == "r2p" || kanaByKana[picked] == nil {
		return ""
	}
	return picked
}

// finishKanaAnswer records the answer, announces a passed stage, and either asks
// the next question or closes the round with a score.
func finishKanaAnswer(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64, userID int, ks kanaState,
	ok bool, confused, verdictLine string) {
	if ok {
		confused = ""
	}
	if err := storage.RecordKana(db, userID, ks.Item, ks.Dir, ok, confused); err != nil {
		log.Printf("kana record: %v", err)
	}
	ks.Asked++
	if ok {
		ks.OK++
	}
	m := kanaModes[ks.Mode]
	prog, err := storage.KanaProgress(db, userID)
	if err != nil {
		log.Printf("kana progress: %v", err)
	}
	msg := verdictLine
	if next := advanceKanaStage(m, ks.Stage, prog); next != ks.Stage {
		storage.SetKanaStage(db, userID, ks.Mode, next)
		ks.Stage = next
		msg += "\n\nУровень пройден! Дальше:\n" + kanaStageLine(m, next, prog)
	}
	if ks.Asked >= kanaRoundSize {
		storage.ClearMode(db, userID)
		sendKb(bot, chatID, fmt.Sprintf("%s\n\nРаунд окончен: %d из %d.\n%s", msg, ks.OK, ks.Asked,
			kanaStageLine(m, ks.Stage, prog)), kanaRoundKeyboard(ks.Mode))
		return
	}
	askKana(bot, db, chatID, userID, ks, prog, msg+"\n\n")
}

func sendKbID(bot *tgbotapi.BotAPI, chatID int64, text string, kb tgbotapi.InlineKeyboardMarkup) int {
	m := tgbotapi.NewMessage(chatID, tidy(text))
	m.ReplyMarkup = kb
	sent, err := bot.Send(m)
	if err != nil {
		log.Printf("send error: %v", err)
		return 0
	}
	return sent.MessageID
}

func removeKeyboard(bot *tgbotapi.BotAPI, chatID int64, messageID int) {
	if messageID != 0 {
		bot.Send(tgbotapi.NewEditMessageReplyMarkup(chatID, messageID, tgbotapi.NewInlineKeyboardMarkup()))
	}
}
