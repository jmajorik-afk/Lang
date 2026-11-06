// Command stickerpack uploads a folder of animated .tgs stickers to Telegram as
// one sticker set, taking each sticker's emoji and search keywords from a CSV
// table. @Stickers only takes them one at a time; this does a whole set per run.
//
//	go run ./tools/stickerpack -csv katakana-stickers/kana_stickers.csv -set katakana
//	go run ./tools/stickerpack -csv katakana-stickers/kana_stickers.csv -set katakana -title "Катакана" -upload
//
// Without -upload nothing is created: it reports which rows have no file, which
// files have no row, and whether every file meets Telegram's rules for animated
// stickers; with a token at hand it also checks the bot and the set name.
//
// The set is created by the bot whose TELEGRAM_TOKEN is in .env, so Telegram
// requires its short name to end in _by_<bot username> — the tool appends that.
// The owner is the first user in ALLOWED_TELEGRAM_USER_IDS (or -owner) and must
// have started the bot at least once. Re-running is safe: stickers already added
// are recorded next to the CSV and skipped, so a failed file can be fixed and
// the same command run again.
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/joho/godotenv"
)

// Telegram's limits, from https://core.telegram.org/stickers and the Bot API.
const (
	maxStickers   = 120 // regular (non-emoji) sets
	maxTGSBytes   = 64 << 10
	canvasSize    = 512
	frameRate     = 60
	maxSeconds    = 3.0
	maxEmoji      = 20
	maxKeywords   = 20
	maxKeywordLen = 64 // all keywords of one sticker together
)

type sticker struct {
	File     string // as written in the table
	Set      string
	Kana     string
	Emoji    []string
	Keywords []string
	Path     string // the matching file on disk, "" if there is none
}

func main() {
	csvPath := flag.String("csv", "", "таблица: file,set,kana,romaji,emoji,word,keywords")
	set := flag.String("set", "", "какой набор из колонки set взять, например katakana")
	name := flag.String("name", "", "короткое имя для ссылки t.me/addstickers/…, по умолчанию = -set")
	title := flag.String("title", "", "название набора, которое видно в Telegram")
	owner := flag.Int64("owner", 0, "Telegram ID владельца, по умолчанию первый из ALLOWED_TELEGRAM_USER_IDS")
	upload := flag.Bool("upload", false, "создать набор и загрузить стикеры; без флага — только проверка")
	flag.Parse()

	if *csvPath == "" || *set == "" {
		fail("нужны -csv и -set, например: -csv katakana-stickers/kana_stickers.csv -set katakana")
	}
	if *name == "" {
		*name = *set
	}
	if !validPrefix(*name) {
		fail("-name %q не подходит: латиница, цифры и _, начинается с буквы, без __ и без _ на конце", *name)
	}
	if *upload && strings.TrimSpace(*title) == "" {
		fail("для загрузки нужно название: -title \"Катакана\"")
	}
	if utf8.RuneCountInString(*title) > 64 {
		fail("название длиннее 64 символов")
	}

	all, err := readTable(*csvPath)
	if err != nil {
		fail("таблица: %v", err)
	}
	files, err := indexFiles(filepath.Dir(*csvPath))
	if err != nil {
		fail("папка: %v", err)
	}
	ready := report(all, files, *set)

	loadEnv()
	token := os.Getenv("TELEGRAM_TOKEN")
	if token == "" {
		if *upload {
			fail("нет TELEGRAM_TOKEN в .env")
		}
		fmt.Println("\nТокена нет — проверил только файлы.")
		return
	}
	if *owner == 0 {
		*owner = firstAllowedUser()
	}
	if *owner == 0 {
		fail("не знаю, чей это набор: укажи -owner или ALLOWED_TELEGRAM_USER_IDS в .env")
	}
	a := &api{token: token, http: &http.Client{Timeout: 60 * time.Second}}
	statePath := func(full string) string {
		return filepath.Join(filepath.Dir(*csvPath), ".uploaded-"+full)
	}
	if err := run(a, *owner, *name, *title, ready, statePath, *upload); err != nil {
		fail("%v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// ---------- table and folder ----------

func readTable(path string) ([]sticker, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(recs) < 2 {
		return nil, errors.New("в таблице нет строк")
	}
	col := map[string]int{}
	for i, h := range recs[0] {
		// a spreadsheet export may start with a byte-order mark
		col[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\uFEFF")))] = i
	}
	for _, need := range []string{"file", "set", "emoji"} {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("нет колонки %q", need)
		}
	}
	get := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	var out []sticker
	for _, rec := range recs[1:] {
		if get(rec, "file") == "" {
			continue
		}
		out = append(out, sticker{
			File:     get(rec, "file"),
			Set:      get(rec, "set"),
			Kana:     get(rec, "kana"),
			Emoji:    strings.Fields(get(rec, "emoji")),
			Keywords: uniq(strings.Fields(get(rec, "keywords"))),
		})
	}
	return out, nil
}

// fileKey lets the table and the folder disagree on case and on - versus _:
// the table says katakana_a.tgs, the exported file is katakana-a.tgs.
func fileKey(name string) string {
	return strings.ReplaceAll(strings.ToLower(filepath.Base(name)), "-", "_")
}

func indexFiles(dir string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".tgs") {
			files[fileKey(p)] = p
		}
		return nil
	})
	return files, err
}

func uniq(words []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range words {
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

// report prints what the dry run found and returns the stickers of the chosen
// set that have a file and pass every check, in table order.
func report(all []sticker, files map[string]string, set string) []sticker {
	inTable := map[string]bool{}
	var rows []sticker
	for _, s := range all {
		inTable[fileKey(s.File)] = true
		if strings.EqualFold(s.Set, set) {
			rows = append(rows, s)
		}
	}
	if len(rows) == 0 {
		fail("в таблице нет строк с set = %q", set)
	}

	var ready, missing []sticker
	bad := 0
	fmt.Printf("Набор «%s»: %d строк в таблице.\n\n", set, len(rows))
	for _, s := range rows {
		s.Path = files[fileKey(s.File)]
		if s.Path == "" {
			missing = append(missing, s)
			continue
		}
		problems := checkTGS(s.Path)
		problems = append(problems, checkMeta(s)...)
		if len(problems) > 0 {
			bad++
			fmt.Printf("✗ %s  %s: %s\n", s.Kana, s.File, strings.Join(problems, "; "))
			continue
		}
		ready = append(ready, s)
	}

	if len(missing) > 0 {
		var kana []string
		for _, s := range missing {
			kana = append(kana, s.Kana)
		}
		fmt.Printf("Нет файла, пропущу (%d): %s\n", len(missing), strings.Join(kana, " "))
	}
	var orphans []string
	for key, p := range files {
		if !inTable[key] {
			orphans = append(orphans, filepath.Base(p))
		}
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		fmt.Printf("Файл есть, а строки в таблице нет — без эмодзи не загрузить (%d): %s\n",
			len(orphans), strings.Join(orphans, ", "))
	}
	fmt.Printf("Проверка файлов: %d в порядке, %d с ошибками.\n", len(ready), bad)
	fmt.Printf("Готово к загрузке: %d.\n", len(ready))
	if len(ready) > maxStickers {
		fail("в одном наборе может быть не больше %d стикеров", maxStickers)
	}
	return ready
}

// checkTGS applies the rules for animated stickers that can be read off the
// file itself (https://core.telegram.org/stickers). Telegram still has the last
// word on upload — for example it also forbids expressions, which are not
// checked here.
func checkTGS(path string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return []string{err.Error()}
	}
	var problems []string
	if len(raw) > maxTGSBytes {
		problems = append(problems, fmt.Sprintf("%.1f КБ, а можно не больше 64", float64(len(raw))/1024))
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return append(problems, "это не .tgs: файл не сжат gzip")
	}
	var anim struct {
		W      float64 `json:"w"`
		H      float64 `json:"h"`
		Fr     float64 `json:"fr"`
		Ip     float64 `json:"ip"`
		Op     float64 `json:"op"`
		Layers []struct {
			Ty      int               `json:"ty"`
			Ddd     int               `json:"ddd"`
			HasMask bool              `json:"hasMask"`
			Ef      []json.RawMessage `json:"ef"`
		} `json:"layers"`
	}
	if err := json.NewDecoder(zr).Decode(&anim); err != nil {
		return append(problems, "внутри не Lottie: "+err.Error())
	}
	if anim.W != canvasSize || anim.H != canvasSize {
		problems = append(problems, fmt.Sprintf("холст %gx%g, нужен 512x512", anim.W, anim.H))
	}
	if anim.Fr != frameRate {
		problems = append(problems, fmt.Sprintf("%g кадров/с, нужно 60", anim.Fr))
	} else if d := (anim.Op - anim.Ip) / anim.Fr; d > maxSeconds+1e-9 {
		problems = append(problems, fmt.Sprintf("длится %.2f с, можно не больше 3", d))
	}
	for _, l := range anim.Layers {
		var p string
		switch {
		case l.Ty == 1:
			p = "есть слой-заливка (solid)"
		case l.Ty == 2:
			p = "есть картинка внутри"
		case l.Ty == 5:
			p = "есть текстовый слой"
		case l.Ddd == 1:
			p = "есть 3D-слой"
		case l.HasMask:
			p = "есть маска"
		case len(l.Ef) > 0:
			p = "есть эффекты слоя"
		}
		if p != "" {
			problems = append(problems, p)
			break // one layer complaint is enough to send the file back to the editor
		}
	}
	return problems
}

// checkMeta checks the table side of a sticker: emoji and search keywords.
func checkMeta(s sticker) []string {
	var problems []string
	if len(s.Emoji) == 0 {
		problems = append(problems, "нет эмодзи")
	}
	if len(s.Emoji) > maxEmoji {
		problems = append(problems, fmt.Sprintf("эмодзи больше %d", maxEmoji))
	}
	total := 0
	for _, k := range s.Keywords {
		total += utf8.RuneCountInString(k)
	}
	if len(s.Keywords) > maxKeywords || total > maxKeywordLen {
		problems = append(problems, fmt.Sprintf("ключевых слов %d (%d символов), можно до %d и до %d символов",
			len(s.Keywords), total, maxKeywords, maxKeywordLen))
	}
	return problems
}

// ---------- Telegram ----------

type api struct {
	token string
	http  *http.Client
}

type apiError struct {
	Description string
	RetryAfter  int
}

func (e *apiError) Error() string { return e.Description }

// mask keeps the token out of everything printed: a network error from
// net/http quotes the full request URL, and that URL embeds the token.
func (a *api) mask(s string) string {
	return strings.ReplaceAll(s, a.token, "<token>")
}

// call posts one Bot API request as multipart form data; filePath, when set, is
// attached as the part named "file" (InputSticker refers to it as attach://file).
func (a *api) call(method string, params map[string]string, filePath string, out any) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range params {
		if err := w.WriteField(k, v); err != nil {
			return err
		}
	}
	if filePath != "" {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return err
		}
		part, err := w.CreateFormFile("file", filepath.Base(filePath))
		if err != nil {
			return err
		}
		if _, err := part.Write(data); err != nil {
			return err
		}
	}
	if err := w.Close(); err != nil {
		return err
	}
	url := "https://api.telegram.org/bot" + a.token + "/" + method
	var resp *http.Response
	var err error
	if len(params) == 0 && filePath == "" {
		resp, err = a.http.Get(url) // Telegram answers 400 to an empty multipart form
	} else {
		resp, err = a.http.Post(url, w.FormDataContentType(), &body)
	}
	if err != nil {
		return errors.New(a.mask(err.Error()))
	}
	defer resp.Body.Close()
	var r struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("%s: непонятный ответ Telegram (HTTP %d)", method, resp.StatusCode)
	}
	if !r.OK {
		return &apiError{Description: a.mask(r.Description), RetryAfter: r.Parameters.RetryAfter}
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

// callPatiently waits out Telegram's flood control instead of failing on it.
func (a *api) callPatiently(method string, params map[string]string, filePath string) error {
	for attempt := 1; ; attempt++ {
		err := a.call(method, params, filePath, nil)
		var ae *apiError
		if errors.As(err, &ae) && ae.RetryAfter > 0 && attempt < 5 {
			fmt.Printf("  Telegram просит подождать %d с…\n", ae.RetryAfter)
			time.Sleep(time.Duration(ae.RetryAfter+1) * time.Second)
			continue
		}
		return err
	}
}

type inputSticker struct {
	Sticker   string   `json:"sticker"`
	Format    string   `json:"format"`
	EmojiList []string `json:"emoji_list"`
	Keywords  []string `json:"keywords,omitempty"`
}

func run(a *api, owner int64, name, title string, ready []sticker, statePath func(string) string, upload bool) error {
	var me struct {
		Username string `json:"username"`
	}
	if err := a.call("getMe", nil, "", &me); err != nil {
		return fmt.Errorf("бот не отвечает — проверь TELEGRAM_TOKEN в .env: %v", err)
	}
	full := name + "_by_" + me.Username
	if len(full) > 64 {
		return fmt.Errorf("имя %q длиннее 64 символов, укороти -name", full)
	}

	var existing struct {
		Title    string            `json:"title"`
		Stickers []json.RawMessage `json:"stickers"`
	}
	exists := true
	if err := a.call("getStickerSet", map[string]string{"name": full}, "", &existing); err != nil {
		var ae *apiError
		if !errors.As(err, &ae) || !strings.Contains(ae.Description, "STICKERSET_INVALID") {
			return fmt.Errorf("не смог проверить набор %s: %v", full, err)
		}
		exists = false
	}

	state := statePath(full)
	done := readState(state)
	if !exists && len(done) > 0 {
		done = map[string]bool{} // the set was deleted since the last run: start over
		os.Remove(state)
	}
	var todo []sticker
	for _, s := range ready {
		if !done[fileKey(s.File)] {
			todo = append(todo, s)
		}
	}

	link := "https://t.me/addstickers/" + full
	fmt.Printf("\nБот @%s, ссылка на набор: %s\n", me.Username, link)
	if exists {
		fmt.Printf("Набор уже есть («%s», %d стикеров), добавлю недостающие: %d.\n",
			existing.Title, len(existing.Stickers), len(todo))
	} else {
		fmt.Printf("Набора ещё нет, создам с %d стикерами.\n", len(todo))
	}
	if len(existing.Stickers)+len(todo) > maxStickers {
		return fmt.Errorf("получится больше %d стикеров в наборе", maxStickers)
	}
	if !upload {
		fmt.Println("Это была проверка. Чтобы загрузить, добавь -upload и -title.")
		return nil
	}
	if len(todo) == 0 {
		fmt.Println("Загружать нечего — всё уже в наборе.")
		return nil
	}

	fmt.Println()
	added, failed := 0, 0
	user := strconv.FormatInt(owner, 10)
	for _, s := range todo {
		input, err := json.Marshal(inputSticker{
			Sticker: "attach://file", Format: "animated", EmojiList: s.Emoji, Keywords: s.Keywords,
		})
		if err != nil {
			return err
		}
		if !exists {
			// the set is born with its first sticker; every later one is added
			// separately, so one bad file costs one sticker, not the whole set
			err = a.callPatiently("createNewStickerSet", map[string]string{
				"user_id": user, "name": full, "title": title, "stickers": "[" + string(input) + "]",
			}, s.Path)
			exists = err == nil
		} else {
			err = a.callPatiently("addStickerToSet", map[string]string{
				"user_id": user, "name": full, "sticker": string(input),
			}, s.Path)
		}
		if err != nil {
			failed++
			fmt.Printf("  ✗ %s  %s: %v%s\n", s.Kana, s.File, err, hint(err))
			continue
		}
		appendState(state, fileKey(s.File))
		added++
		fmt.Printf("  ✓ %s  %s %s\n", s.Kana, s.File, strings.Join(s.Emoji, ""))
		time.Sleep(300 * time.Millisecond)
	}
	fmt.Printf("\nДобавлено %d, не получилось %d.\n%s\n", added, failed, link)
	if failed > 0 {
		fmt.Println("Поправь то, что не прошло, и запусти ту же команду ещё раз — загруженные пропустятся.")
	}
	return nil
}

// hint turns the Telegram errors a first run is likely to hit into a next step.
func hint(err error) string {
	d := err.Error()
	switch {
	case strings.Contains(d, "PEER_ID_INVALID"), strings.Contains(d, "user not found"):
		return " — владелец должен хотя бы раз написать боту /start"
	case strings.Contains(d, "EMOJI"):
		return " — Telegram не принял эмодзи, поменяй его в таблице"
	case strings.Contains(d, "TGS"), strings.Contains(d, "STICKER_FILE"):
		return " — Telegram не принял сам файл, пересохрани его в редакторе"
	}
	return ""
}

func readState(path string) map[string]bool {
	done := map[string]bool{}
	data, err := os.ReadFile(path)
	if err != nil {
		return done
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			done[line] = true
		}
	}
	return done
}

func appendState(path, key string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  не смог записать %s: %v\n", path, err)
		return
	}
	defer f.Close()
	fmt.Fprintln(f, key)
}

// ---------- settings ----------

var prefixRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// validPrefix checks the part of the set name before "_by_<bot>": Telegram
// allows letters, digits and underscores, starting with a letter, never two
// underscores in a row — so no trailing one either, as "_by_" follows.
func validPrefix(s string) bool {
	return prefixRe.MatchString(s) && !strings.Contains(s, "__") && !strings.HasSuffix(s, "_")
}

// loadEnv finds the bot's .env in the current directory or above it, so the
// tool works from the repo root and from the sticker folder alike.
func loadEnv() {
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	for i := 0; i < 5; i++ {
		if godotenv.Load(filepath.Join(dir, ".env")) == nil {
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}

func firstAllowedUser() int64 {
	for _, s := range strings.Split(os.Getenv("ALLOWED_TELEGRAM_USER_IDS"), ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			return id
		}
	}
	return 0
}
