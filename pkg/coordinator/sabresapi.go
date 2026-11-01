package coordinator

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/auth"
	"github.com/giongto35/cloud-game/v3/pkg/config"
	"github.com/giongto35/cloud-game/v3/pkg/network/httpx"
	"github.com/giongto35/cloud-game/v3/pkg/store"
)

// Sabres: the rhythm game. Songs are shared by every account; the browser analyses the audio
// and sends the generated charts (note maps) with the upload.

const (
	maxChartBytes     = 3 << 20
	maxSabresTitleLen = 120
)

var sabresAudio = map[string]string{
	"mp3": "audio/mpeg", "m4a": "audio/mp4", "aac": "audio/aac", "ogg": "audio/ogg", "oga": "audio/ogg",
	"opus": "audio/ogg", "wav": "audio/wav", "flac": "audio/flac", "webm": "audio/webm",
}

var sabresDifficulties = map[string]bool{"facil": true, "normal": true, "dificil": true, "expert": true}

var sabresGrades = map[string]bool{"SS": true, "S": true, "A": true, "B": true, "C": true, "D": true, "E": true, "F": true}

// SabresOptions is the configuration of the module.
type SabresOptions struct {
	Dir      string
	MaxBytes int64
}

func newSabresOptions(c config.Sabres) *SabresOptions {
	if !c.Enabled {
		return nil
	}
	max := int64(c.MaxUploadMB)
	if max <= 0 {
		max = 40
	}
	return &SabresOptions{Dir: c.Dir, MaxBytes: max << 20}
}

func (a *API) sabresRoutes(mux *httpx.Mux) {
	mux.HandleFunc("GET /api/sabres/songs", a.authed(a.sabresSongs))
	mux.HandleFunc("PUT /api/sabres/songs", a.rawUpload(a.authed(a.sabresPutSong)))
	mux.HandleFunc("PUT /api/sabres/songs/{id}/chart", a.mutating(a.authed(a.sabresPutChart)))
	mux.HandleFunc("GET /api/sabres/songs/{id}/chart", a.authed(a.sabresGetChart))
	mux.HandleFunc("GET /api/sabres/songs/{id}/audio", a.authed(a.sabresAudio))
	mux.HandleFunc("DELETE /api/sabres/songs/{id}", a.mutating(a.authed(a.sabresDeleteSong)))
	mux.HandleFunc("POST /api/sabres/scores", a.mutating(a.authed(a.sabresPostScore)))
	mux.HandleFunc("GET /api/sabres/ranking", a.authed(a.sabresRanking))
	mux.HandleFunc("GET /api/sabres/me", a.authed(a.sabresMe))
}

func (a *API) sabresReady(w http.ResponseWriter) bool {
	if a.sabres == nil {
		writeErr(w, &auth.Error{Status: 404, Code: "sabres_disabled", Message: "O jogo de sabres está desativado neste servidor."})
		return false
	}
	return true
}

func sabresId(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

type sabresSongView struct {
	Id         int64  `json:"id"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	DurationMs int64  `json:"duration_ms"`
	Bpm        int    `json:"bpm"`
	Size       int64  `json:"size"`
	HasChart   bool   `json:"has_chart"`
	Uploader   string `json:"uploader"`
	Mine       bool   `json:"mine"`
	CreatedAt  int64  `json:"created_at"`
	Plays      int    `json:"plays"`
	BestScore  int64  `json:"best_score"`
	BestGrade  string `json:"best_grade"`
	BestAcc    int    `json:"best_accuracy"`
	LastPlayed int64  `json:"last_played"`
	TopScore   int64  `json:"top_score"`
	TopName    string `json:"top_name"`
}

func (a *API) sabresSongs(w http.ResponseWriter, r *http.Request, acc *auth.Account, _ string) {
	if !a.sabresReady(w) {
		return
	}
	ctx := r.Context()
	songs, err := a.store.SabresSongs(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	stats, err := a.store.SabresUserStats(ctx, acc.Id)
	if err != nil {
		writeErr(w, err)
		return
	}
	tops, err := a.store.SabresTops(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]sabresSongView, 0, len(songs))
	for _, s := range songs {
		v := sabresSongView{Id: s.Id, Title: s.Title, Artist: s.Artist, DurationMs: s.DurationMs, Bpm: s.Bpm, Size: s.Size,
			HasChart: s.HasChart, Uploader: s.Uploader, Mine: s.UploadedBy != nil && *s.UploadedBy == acc.Id, CreatedAt: s.CreatedAt.Unix()}
		if st := stats[s.Id]; st != nil {
			v.Plays, v.BestScore, v.BestGrade, v.BestAcc, v.LastPlayed = st.Plays, st.BestScore, st.BestGrade, st.BestAcc, st.LastPlayed.Unix()
		}
		if t, ok := tops[s.Id]; ok {
			v.TopScore, v.TopName = t.Score, t.Name
		}
		out = append(out, v)
	}
	writeJson(w, http.StatusOK, map[string]any{"songs": out, "max_upload_mb": a.sabres.MaxBytes >> 20})
}

func randomName() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (a *API) sabresPutSong(w http.ResponseWriter, r *http.Request, acc *auth.Account, _ string) {
	if !a.sabresReady(w) {
		return
	}
	q := r.URL.Query()
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(q.Get("name")), "."))
	mime, ok := sabresAudio[ext]
	if !ok {
		writeErr(w, &auth.Error{Status: 400, Code: "bad_type", Message: "Formato não suportado. Envie MP3, M4A, OGG, WAV ou FLAC."})
		return
	}
	title := strings.TrimSpace(q.Get("title"))
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(q.Get("name")), filepath.Ext(q.Get("name")))
	}
	title = truncateRunes(title, maxSabresTitleLen)
	artist := truncateRunes(strings.TrimSpace(q.Get("artist")), maxSabresTitleLen)
	duration, _ := strconv.ParseInt(q.Get("duration_ms"), 10, 64)
	bpm, _ := strconv.Atoi(q.Get("bpm"))
	if duration < 0 || duration > 4*3600*1000 {
		duration = 0
	}
	if bpm < 0 || bpm > 400 {
		bpm = 0
	}

	if err := os.MkdirAll(a.sabres.Dir, 0o750); err != nil {
		a.log.Error().Err(err).Msg("sabres: create dir")
		writeErr(w, err)
		return
	}
	file := randomName() + "." + ext
	dst := filepath.Join(a.sabres.Dir, file)
	tmp := dst + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o640)
	if err != nil {
		writeErr(w, err)
		return
	}
	n, err := io.Copy(f, http.MaxBytesReader(w, r.Body, a.sabres.MaxBytes))
	cerr := f.Close()
	if err != nil || cerr != nil || n == 0 {
		_ = os.Remove(tmp)
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, &auth.Error{Status: 413, Code: "too_large", Message: "A música é grande demais (máximo " + strconv.FormatInt(a.sabres.MaxBytes>>20, 10) + " MB)."})
			return
		}
		writeErr(w, &auth.Error{Status: 400, Code: "bad_upload", Message: "O envio foi interrompido. Tente novamente."})
		return
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		writeErr(w, err)
		return
	}
	id, err := a.store.AddSabresSong(r.Context(), &store.SabresSong{Title: title, Artist: artist, DurationMs: duration, Bpm: bpm,
		File: file, Size: n, Mime: mime, UploadedBy: &acc.Id})
	if err != nil {
		_ = os.Remove(dst)
		writeErr(w, err)
		return
	}
	a.auth.Audit(r.Context(), acc, "sabres.song.add", title)
	writeJson(w, http.StatusOK, map[string]any{"id": id, "title": title})
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func (a *API) sabresPutChart(w http.ResponseWriter, r *http.Request, acc *auth.Account, _ string) {
	if !a.sabresReady(w) {
		return
	}
	id, ok := sabresId(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	song, err := a.store.SabresSong(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, &auth.Error{Status: 404, Code: "not_found", Message: "Música não encontrada."})
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	// the chart is generated by the uploader's browser; anyone may fill a missing one
	if song.HasChart && !(song.UploadedBy != nil && *song.UploadedBy == acc.Id) && !acc.Can(auth.PermAdminView) {
		writeErr(w, auth.ErrForbidden)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChartBytes))
	if err != nil || !json.Valid(body) {
		writeErr(w, &auth.Error{Status: 400, Code: "bad_request", Message: "Mapa de notas inválido."})
		return
	}
	var head struct {
		Bpm float64 `json:"bpm"`
	}
	_ = json.Unmarshal(body, &head)
	if err := a.store.SetSabresChart(r.Context(), id, string(body), int(head.Bpm+0.5)); err != nil {
		writeErr(w, err)
		return
	}
	writeJson(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *API) sabresGetChart(w http.ResponseWriter, r *http.Request, _ *auth.Account, _ string) {
	if !a.sabresReady(w) {
		return
	}
	id, ok := sabresId(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	chart, err := a.store.SabresChart(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && chart == "") {
		writeErr(w, &auth.Error{Status: 404, Code: "no_chart", Message: "Esta música ainda não tem mapa de notas."})
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-cache")
	_, _ = io.WriteString(w, chart)
}

func (a *API) sabresAudio(w http.ResponseWriter, r *http.Request, _ *auth.Account, _ string) {
	if !a.sabresReady(w) {
		return
	}
	id, ok := sabresId(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	song, err := a.store.SabresSong(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(a.sabres.Dir, filepath.Base(song.File)))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", song.Mime)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, song.File, song.CreatedAt, f)
}

func (a *API) sabresDeleteSong(w http.ResponseWriter, r *http.Request, acc *auth.Account, _ string) {
	if !a.sabresReady(w) {
		return
	}
	id, ok := sabresId(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	song, err := a.store.SabresSong(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, &auth.Error{Status: 404, Code: "not_found", Message: "Música não encontrada."})
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	if !(song.UploadedBy != nil && *song.UploadedBy == acc.Id) && !acc.Can(auth.PermAdminView) {
		writeErr(w, auth.ErrForbidden)
		return
	}
	if err := a.store.DeleteSabresSong(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	_ = os.Remove(filepath.Join(a.sabres.Dir, filepath.Base(song.File)))
	a.auth.Audit(r.Context(), acc, "sabres.song.delete", song.Title)
	writeJson(w, http.StatusOK, map[string]any{"ok": true})
}

type sabresScoreIn struct {
	SongId     int64  `json:"song_id"`
	Difficulty string `json:"difficulty"`
	Score      int64  `json:"score"`
	Accuracy   int    `json:"accuracy"`
	MaxCombo   int    `json:"max_combo"`
	Grade      string `json:"grade"`
	Hits       int    `json:"hits"`
	Misses     int    `json:"misses"`
}

func (a *API) sabresPostScore(w http.ResponseWriter, r *http.Request, acc *auth.Account, _ string) {
	if !a.sabresReady(w) {
		return
	}
	var in sabresScoreIn
	if err := readJson(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	bad := !sabresDifficulties[in.Difficulty] || !sabresGrades[in.Grade] || in.Score < 0 || in.Score > 100_000_000 ||
		in.Accuracy < 0 || in.Accuracy > 1000 || in.MaxCombo < 0 || in.MaxCombo > 100000 ||
		in.Hits < 0 || in.Hits > 100000 || in.Misses < 0 || in.Misses > 100000
	if bad {
		writeErr(w, &auth.Error{Status: 400, Code: "bad_request", Message: "Pontuação inválida."})
		return
	}
	if _, err := a.store.SabresSong(r.Context(), in.SongId); err != nil {
		writeErr(w, &auth.Error{Status: 404, Code: "not_found", Message: "Música não encontrada."})
		return
	}
	// position in the ranking of that song and difficulty, before this run is counted
	before, err := a.store.SabresRanking(r.Context(), in.SongId, in.Difficulty, 1000)
	if err != nil {
		writeErr(w, err)
		return
	}
	var prevBest int64
	for _, s := range before {
		if s.UserId == acc.Id {
			prevBest = s.Score
		}
	}
	if _, err := a.store.AddSabresScore(r.Context(), &store.SabresScore{SongId: in.SongId, UserId: acc.Id, Difficulty: in.Difficulty,
		Score: in.Score, Accuracy: in.Accuracy, MaxCombo: in.MaxCombo, Grade: in.Grade, Hits: in.Hits, Misses: in.Misses}); err != nil {
		writeErr(w, err)
		return
	}
	rank := 1
	for _, s := range before {
		if s.UserId != acc.Id && s.Score > in.Score {
			rank++
		}
	}
	writeJson(w, http.StatusOK, map[string]any{"rank": rank, "players": len(before) + boolInt(prevBest == 0),
		"record": in.Score > prevBest, "previous_best": prevBest})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

type sabresScoreView struct {
	Id         int64  `json:"id"`
	SongId     int64  `json:"song_id"`
	SongTitle  string `json:"song_title"`
	UserId     int64  `json:"user_id"`
	Name       string `json:"name"`
	Difficulty string `json:"difficulty"`
	Score      int64  `json:"score"`
	Accuracy   int    `json:"accuracy"`
	MaxCombo   int    `json:"max_combo"`
	Grade      string `json:"grade"`
	Hits       int    `json:"hits"`
	Misses     int    `json:"misses"`
	PlayedAt   int64  `json:"played_at"`
}

func scoreViews(list []*store.SabresScore) []sabresScoreView {
	out := make([]sabresScoreView, 0, len(list))
	for _, s := range list {
		out = append(out, sabresScoreView{Id: s.Id, SongId: s.SongId, SongTitle: s.SongTitle, UserId: s.UserId, Name: s.UserName,
			Difficulty: s.Difficulty, Score: s.Score, Accuracy: s.Accuracy, MaxCombo: s.MaxCombo, Grade: s.Grade,
			Hits: s.Hits, Misses: s.Misses, PlayedAt: s.PlayedAt.Unix()})
	}
	return out
}

// sabresRanking returns the ranking of one song (?song=) or the overall ranking when no song is given.
func (a *API) sabresRanking(w http.ResponseWriter, r *http.Request, _ *auth.Account, _ string) {
	if !a.sabresReady(w) {
		return
	}
	q := r.URL.Query()
	if q.Get("song") == "" {
		list, err := a.store.SabresOverall(r.Context(), 50)
		if err != nil {
			writeErr(w, err)
			return
		}
		type row struct {
			UserId int64  `json:"user_id"`
			Name   string `json:"name"`
			Total  int64  `json:"total"`
			Songs  int    `json:"songs"`
		}
		out := make([]row, 0, len(list))
		for _, p := range list {
			out = append(out, row{p.UserId, p.Name, p.Total, p.Songs})
		}
		writeJson(w, http.StatusOK, map[string]any{"players": out})
		return
	}
	id, err := strconv.ParseInt(q.Get("song"), 10, 64)
	diff := q.Get("difficulty")
	if err != nil || (diff != "" && !sabresDifficulties[diff]) {
		writeErr(w, &auth.Error{Status: 400, Code: "bad_request", Message: "Requisição inválida."})
		return
	}
	list, err := a.store.SabresRanking(r.Context(), id, diff, 50)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJson(w, http.StatusOK, map[string]any{"scores": scoreViews(list)})
}

// sabresMe returns the play history of the account.
func (a *API) sabresMe(w http.ResponseWriter, r *http.Request, acc *auth.Account, _ string) {
	if !a.sabresReady(w) {
		return
	}
	list, err := a.store.SabresHistory(r.Context(), acc.Id, 100)
	if err != nil {
		writeErr(w, err)
		return
	}
	stats, err := a.store.SabresUserStats(r.Context(), acc.Id)
	if err != nil {
		writeErr(w, err)
		return
	}
	var plays int
	var total int64
	for _, st := range stats {
		plays += st.Plays
		total += st.BestScore
	}
	writeJson(w, http.StatusOK, map[string]any{"history": scoreViews(list), "songs_played": len(stats), "plays": plays,
		"total_best": total, "now": time.Now().Unix()})
}
