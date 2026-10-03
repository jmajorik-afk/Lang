package bot

import (
	"strings"
	"testing"

	"language-learning-bot/pkg/storage"
)

func kanaCount(script byte, level int, word bool) int {
	n := 0
	for _, it := range kanaItems {
		if it.Script == script && it.Level == level && it.Word == word {
			n++
		}
	}
	return n
}

// TestKanaInventory — every syllable of both scripts is there, and every word
// of the lists could be read: a word the converter can't read is silently
// dropped at startup, so this is where a typo in the lists would show.
func TestKanaInventory(t *testing.T) {
	for _, c := range []struct {
		script byte
		level  int
		word   bool
		want   int
	}{
		{'h', 1, false, 46}, {'h', 2, false, 25}, {'h', 3, false, 33},
		{'k', 1, false, 46}, {'k', 2, false, 25}, {'k', 3, false, 33}, {'k', 5, false, 21},
		{'p', 1, false, 46}, {'p', 2, false, 23}, {'p', 3, false, 33}, // no ぢ/づ pairs
		{'h', 4, true, 12}, {'k', 4, true, 14},
	} {
		if got := kanaCount(c.script, c.level, c.word); got != c.want {
			t.Errorf("script %c level %d: %d items, want %d", c.script, c.level, got, c.want)
		}
	}
	for _, list := range []string{hiraSokuon, kataSokuon, hiraWords, kataWords} {
		for _, w := range strings.Fields(list) {
			if it := kanaByKana[w]; it == nil || !it.Word || it.Romaji[0] == "" {
				t.Errorf("word %q is missing — the converter could not read it", w)
			}
		}
	}
	for _, it := range kanaItems {
		if len(it.Romaji) == 0 || it.Romaji[0] == "" {
			t.Errorf("%q has no reading", it.Kana)
		}
	}
}

func TestKanaToRomaji(t *testing.T) {
	for word, want := range map[string]string{
		"コーヒー": "koohii", "がっこう": "gakkou", "きって": "kitte", "ちょっと": "chotto",
		"れっしゃ": "ressha", "ミュージック": "myuujikku", "ソファ": "sofa", "フォーク": "fooku",
		"ニンジャ": "ninja", "ハンバーガー": "hanbaagaa", "はなぢ": "hanaji", "つづく": "tsuzuku",
		"ぎゅうにゅう": "gyuunyuu", "びょういん": "byouin", "ズッキーニ": "zukkiini",
		"きんえん": "kin'en", "マッチ": "matchi",
	} {
		if got, ok := kanaToRomaji(word); !ok || got != want {
			t.Errorf("kanaToRomaji(%q) = %q, %v; want %q", word, got, ok, want)
		}
	}
	for _, bad := range []string{"abc", "っ", "ー", "きっ"} {
		if _, ok := kanaToRomaji(bad); ok {
			t.Errorf("kanaToRomaji(%q) should fail", bad)
		}
	}
}

func TestKanaReadingOK(t *testing.T) {
	for _, c := range []struct {
		kana, answer string
		want         bool
	}{
		{"し", "shi", true}, {"し", "si", true}, {"し", " SHI ", true}, {"し", "chi", false},
		{"ん", "n", true}, {"ん", "nn", true},
		{"ティ", "ti", true}, {"ティ", "chi", false}, // ティ is not チ
		{"コーヒー", "koohii", true}, {"コーヒー", "kōhī", true}, {"コーヒー", "kohi", false},
		{"がっこう", "gakkou", true}, {"がっこう", "gakkoo", true}, {"がっこう", "gakkō", true},
		{"がっこう", "gakko", false}, {"がっこう", "gakou", false},
		{"ざっし", "zassi", true}, // Kunrei
		{"ハンバーガー", "hambaagaa", true},
	} {
		if got := kanaReadingOK(kanaByKana[c.kana], c.answer); got != c.want {
			t.Errorf("kanaReadingOK(%s, %q) = %v, want %v", c.kana, c.answer, got, c.want)
		}
	}
}

func TestKanaLookalikes(t *testing.T) {
	has := func(list []string, k string) bool {
		for _, x := range list {
			if x == k {
				return true
			}
		}
		return false
	}
	for kana, want := range map[string][]string{
		"シ": {"ツ", "ン", "ソ"}, "ア": {"マ"}, "ぬ": {"め", "ね"}, "か": {"が"}, "パ": {"ハ", "バ"},
		"きゃ": {"きゅ", "きょ", "ぎゃ"}, "ファ": {"フィ", "フェ", "フォ"},
	} {
		got := kanaLookalikesOf(kana)
		for _, w := range want {
			if !has(got, w) {
				t.Errorf("lookalikes of %s = %v, missing %s", kana, got, w)
			}
		}
		for _, g := range got {
			if toHiragana(g) == g != (toHiragana(kana) == kana) {
				t.Errorf("lookalikes of %s include %s from the other script", kana, g)
			}
		}
	}
}

// TestKanaWriteOptions — four distinct buttons of one script, exactly one of
// them reads as asked; never an option that is also right.
func TestKanaWriteOptions(t *testing.T) {
	for _, it := range kanaItems {
		if it.Script == 'p' || it.Level == 0 || it.NoWrite {
			continue
		}
		for round := 0; round < 3; round++ {
			opts, right := kanaWriteOptions(it, "")
			if len(opts) != 4 || opts[right] != it.Kana {
				t.Fatalf("%s: options %v, right %d", it.Kana, opts, right)
			}
			seen := map[string]bool{}
			for i, o := range opts {
				if seen[o] {
					t.Fatalf("%s: duplicate option in %v", it.Kana, opts)
				}
				seen[o] = true
				if i == right {
					continue
				}
				if other := kanaByKana[o]; other != nil {
					if other.Script != it.Script || sameReading(other, it) || other.Level > it.Level {
						t.Fatalf("%s: bad option %s in %v", it.Kana, o, opts)
					}
				} else if !it.Word {
					t.Fatalf("%s: option %s is not a kana", it.Kana, o)
				}
			}
		}
	}
	// what the user mixed up before comes back as an option
	for i := 0; i < 10; i++ {
		opts, _ := kanaWriteOptions(kanaByKana["ぬ"], "め")
		if !strings.Contains(strings.Join(opts, ""), "め") {
			t.Fatalf("the remembered confusion め is missing from %v", opts)
		}
	}
}

func TestKanaWordVariants(t *testing.T) {
	for word, want := range map[string][]string{
		"きって":  {"きて", "きつて"},
		"コーヒー": {"コヒー", "コーヒ"},
		"ベッド":  {"ベド", "ベツド"},
	} {
		got := strings.Join(kanaWordVariants(word), " ")
		for _, w := range want {
			if !strings.Contains(" "+got+" ", " "+w+" ") {
				t.Errorf("variants of %s = %s, missing %s", word, got, w)
			}
		}
	}
}

// TestKanaPairOptions — only knowing both halves finds the right pair.
func TestKanaPairOptions(t *testing.T) {
	for _, it := range kanaItems {
		if it.Script != 'p' {
			continue
		}
		opts, right := kanaPairOptions(it)
		if len(opts) != 4 || opts[right] != it.Hira+" "+it.Kata {
			t.Fatalf("%s: options %v, right %d", it.Kana, opts, right)
		}
		seen := map[string]bool{}
		for _, o := range opts {
			if seen[o] {
				t.Fatalf("%s: duplicate in %v", it.Kana, opts)
			}
			seen[o] = true
		}
	}
}

func TestKanaKnown(t *testing.T) {
	for _, c := range []struct {
		s    storage.KanaStat
		want bool
	}{
		{storage.KanaStat{}, false},
		{storage.KanaStat{Seen: 1, Correct: 1, Streak: 1}, true},  // right the first time
		{storage.KanaStat{Seen: 2, Correct: 1, Streak: 1}, false}, // missed once: prove it twice
		{storage.KanaStat{Seen: 3, Correct: 2, Streak: 2}, true},
		{storage.KanaStat{Seen: 3, Correct: 2, Streak: 0}, false},
	} {
		if got := kanaKnown(c.s); got != c.want {
			t.Errorf("kanaKnown(%+v) = %v, want %v", c.s, got, c.want)
		}
	}
}

// knowAll marks every item of the stage as known, all but skip of them.
func knowAll(prog map[string]storage.KanaStat, m kanaMode, s kanaStage, skip int) {
	for i, it := range stageItems(m, s) {
		if i < skip {
			continue
		}
		prog[kanaKey(s.Dir, it.Kana)] = storage.KanaStat{Seen: 1, Correct: 1, Streak: 1}
	}
}

func TestKanaStages(t *testing.T) {
	hira := kanaModes["hiragana"]
	prog := map[string]storage.KanaStat{}
	if got := advanceKanaStage(hira, 0, prog); got != 0 {
		t.Fatalf("a newcomer starts at stage 0, got %d", got)
	}
	knowAll(prog, hira, kanaStage{1, "read"}, 5) // 41 of 46: below 90%
	if got := advanceKanaStage(hira, 0, prog); got != 0 {
		t.Errorf("41 of 46 known passed the stage")
	}
	knowAll(prog, hira, kanaStage{1, "read"}, 4) // 42 of 46
	if got := advanceKanaStage(hira, 0, prog); got != 1 {
		t.Errorf("42 of 46 known: stage %d, want 1 (reading → writing)", got)
	}
	// the mixed mode counts both scripts: hiragana alone is half of it
	if got := advanceKanaStage(kanaModes["mixed"], 0, prog); got != 0 {
		t.Errorf("mixed mode passed level 1 with no katakana known")
	}
	// a stage once reached is never taken back by a later miss
	if got := advanceKanaStage(hira, 1, map[string]storage.KanaStat{}); got != 1 {
		t.Errorf("stage went back to %d", got)
	}
}

func TestKanaWordUnlock(t *testing.T) {
	prog := map[string]storage.KanaStat{}
	known := func(k string) { prog[kanaKey("read", k)] = storage.KanaStat{Seen: 1, Correct: 1, Streak: 1} }
	inu := kanaByKana["いぬ"]
	if kanaWordUnlocked(inu, prog) {
		t.Fatal("いぬ unlocked with nothing known")
	}
	known("い")
	known("ぬ")
	if !kanaWordUnlocked(inu, prog) {
		t.Error("いぬ must unlock once い and ぬ are known")
	}
	// a word with っ waits for the level that teaches it
	for _, k := range []string{"が", "こ", "う"} {
		known(k)
	}
	gakkou := kanaByKana["がっこう"]
	if kanaWordUnlocked(gakkou, prog) {
		t.Error("がっこう unlocked before level 4 (っ and ー) was passed")
	}
	knowAll(prog, kanaModes["hiragana"], kanaStage{4, "read"}, 0)
	if !kanaWordUnlocked(gakkou, prog) {
		t.Error("がっこう must unlock after level 4")
	}
}

func TestKanaVerdict(t *testing.T) {
	read := kanaState{Item: "ぬ", Dir: "read"}
	if got := kanaVerdict(read, false, false, "mu"); !strings.Contains(got, "nu") || !strings.Contains(got, "む") {
		t.Errorf("read verdict %q must give the reading and name the kana that was read instead", got)
	}
	write := kanaState{Item: "ぬ", Dir: "write"}
	if got := kanaVerdict(write, false, false, "め"); !strings.Contains(got, "ぬ") || !strings.Contains(got, "«me»") {
		t.Errorf("write verdict %q must show ぬ and say what め reads", got)
	}
	if got := kanaVerdict(write, true, false, "ぬ"); !strings.HasPrefix(got, "✓") {
		t.Errorf("right answer verdict %q", got)
	}
}
