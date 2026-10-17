package coordinator

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/api"
	"github.com/giongto35/cloud-game/v3/pkg/auth"
	"github.com/giongto35/cloud-game/v3/pkg/network/httpx"
)

// companionExts are data files of disc images (for example, the .bin tracks of a .cue sheet).
// They are not games by themselves, so the library ignores them, but the emulators need them.
var companionExts = map[string]bool{"bin": true, "img": true, "iso": true, "sub": true, "ccd": true, "mds": true, "mdf": true}

const maxGameNameLen = 200

type gameSystem struct {
	Id     string   `json:"id"`
	Folder string   `json:"folder"`
	Exts   []string `json:"exts"`
}

func (a *API) gameUploadRoutes(mux *httpx.Mux) {
	mux.HandleFunc("GET /api/admin/games/systems", a.admin(a.gameSystems))
	mux.HandleFunc("PUT /api/admin/games", a.rawUpload(a.admin(a.putGame)))
	mux.HandleFunc("POST /api/admin/games/rescan", a.mutating(a.admin(a.rescanGames)))
}

// rawUpload protects endpoints that receive a file as the raw request body (same-origin only).
func (a *API) rawUpload(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/octet-stream") {
			writeErr(w, &auth.Error{Status: 415, Code: "unsupported_media", Message: "Requisição inválida."})
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || !strings.EqualFold(u.Host, r.Host) {
				writeErr(w, &auth.Error{Status: 403, Code: "bad_origin", Message: "Origem não permitida."})
				return
			}
		}
		next(w, r)
	}
}

// systems lists the emulators known by the configuration with their library folder and file extensions.
func (a *API) systems() []gameSystem {
	var list []gameSystem
	for id, core := range a.hub.conf.Emulator.Libretro.Cores.List {
		folder := core.Folder
		if folder == "" {
			folder = id
		}
		list = append(list, gameSystem{Id: id, Folder: folder, Exts: core.Roms})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Id < list[j].Id })
	return list
}

func (a *API) gamesEnabled(w http.ResponseWriter, acc *auth.Account) bool {
	if !acc.Can(auth.PermUsersManage) {
		writeErr(w, auth.ErrForbidden)
		return false
	}
	if !a.hub.conf.Coordinator.Games.Enabled {
		writeErr(w, &auth.Error{Status: 400, Code: "games_disabled", Message: "O envio de jogos está desativado na configuração."})
		return false
	}
	return true
}

func (a *API) gameSystems(w http.ResponseWriter, _ *http.Request, acc *auth.Account, _ string) {
	if !a.gamesEnabled(w, acc) {
		return
	}
	maxMB := a.hub.conf.Coordinator.Games.MaxUploadMB
	companions := make([]string, 0, len(companionExts))
	for e := range companionExts {
		companions = append(companions, e)
	}
	sort.Strings(companions)
	writeJson(w, http.StatusOK, map[string]any{"systems": a.systems(), "companions": companions, "maxMB": maxMB})
}

// cleanGameName validates an uploaded file name: a plain name without any path parts.
func cleanGameName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxGameNameLen {
		return "", errors.New("Nome de arquivo inválido.")
	}
	if strings.HasPrefix(name, ".") || strings.ContainsAny(name, `/\:*?"<>|`) || strings.Contains(name, "..") {
		return "", errors.New("O nome do arquivo tem caracteres não permitidos.")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("O nome do arquivo tem caracteres não permitidos.")
		}
	}
	return name, nil
}

// pickSystem selects the library folder of an uploaded file by the requested system or by its extension.
func (a *API) pickSystem(ext, requested string) (gameSystem, error) {
	systems := a.systems()
	if requested != "" {
		for _, s := range systems {
			if s.Id != requested {
				continue
			}
			if companionExts[ext] {
				return s, nil
			}
			for _, e := range s.Exts {
				if e == ext {
					return s, nil
				}
			}
			return s, fmt.Errorf("O formato .%s não é aceito por esse sistema.", ext)
		}
		return gameSystem{}, errors.New("Sistema desconhecido.")
	}
	var found []gameSystem
	for _, s := range systems {
		for _, e := range s.Exts {
			if e == ext {
				found = append(found, s)
				break
			}
		}
	}
	if len(found) == 0 && companionExts[ext] {
		// the data tracks of a disc image (.bin, .sub...) belong to the CD-based system
		for _, s := range systems {
			if s.Id == "pcsx" {
				return s, nil
			}
		}
		return gameSystem{}, errors.New("Arquivos de disco (.bin, .sub...) precisam de um sistema escolhido.")
	}
	switch len(found) {
	case 0:
		return gameSystem{}, fmt.Errorf("Formato .%s não suportado.", ext)
	case 1:
		return found[0], nil
	}
	return gameSystem{}, fmt.Errorf("O formato .%s serve para mais de um sistema: escolha qual.", ext)
}

// putGame saves one uploaded file (the raw body) into the library folder of its system.
func (a *API) putGame(w http.ResponseWriter, r *http.Request, acc *auth.Account, _ string) {
	if !a.gamesEnabled(w, acc) {
		return
	}
	q := r.URL.Query()
	name, err := cleanGameName(q.Get("name"))
	if err != nil {
		writeErr(w, &auth.Error{Status: 400, Code: "bad_name", Message: err.Error()})
		return
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	sys, err := a.pickSystem(ext, q.Get("system"))
	if err != nil {
		writeErr(w, &auth.Error{Status: 400, Code: "bad_system", Message: err.Error()})
		return
	}

	base, err := filepath.Abs(a.hub.conf.Library.BasePath)
	if err != nil {
		writeErr(w, err)
		return
	}
	dir := filepath.Join(base, sys.Folder)
	dst := filepath.Join(dir, name)
	if rel, err := filepath.Rel(base, dst); err != nil || strings.HasPrefix(rel, "..") {
		writeErr(w, &auth.Error{Status: 400, Code: "bad_name", Message: "Nome de arquivo inválido."})
		return
	}
	if _, err := os.Stat(dst); err == nil && q.Get("overwrite") != "1" {
		writeErr(w, &auth.Error{Status: 409, Code: "exists", Message: "Já existe um arquivo com esse nome."})
		return
	}

	limit := int64(a.hub.conf.Coordinator.Games.MaxUploadMB) << 20
	if limit <= 0 {
		limit = 1 << 30
	}
	if r.ContentLength > limit {
		writeErr(w, &auth.Error{Status: 413, Code: "too_large", Message: fmt.Sprintf("Arquivo grande demais (máximo %d MB).", limit>>20)})
		return
	}
	// big files on slow links must not hit the server-wide read/write timeouts
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})

	if err := os.MkdirAll(dir, 0o755); err != nil {
		a.log.Error().Err(err).Str("dir", dir).Msg("game upload: can't create the folder")
		writeErr(w, &auth.Error{Status: 500, Code: "storage", Message: "Não foi possível gravar na pasta de jogos."})
		return
	}
	// a hidden temporary file: the library scan ignores it, and a failed upload leaves nothing behind
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		a.log.Error().Err(err).Str("dir", dir).Msg("game upload: can't write")
		writeErr(w, &auth.Error{Status: 500, Code: "storage", Message: "Não foi possível gravar na pasta de jogos."})
		return
	}
	tmpName := tmp.Name()
	n, err := io.Copy(tmp, http.MaxBytesReader(w, r.Body, limit))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && n == 0 {
		err = errors.New("empty")
	}
	if err == nil {
		err = os.Chmod(tmpName, 0o644)
	}
	if err == nil {
		err = os.Rename(tmpName, dst)
	}
	if err != nil {
		_ = os.Remove(tmpName)
		var mbe *http.MaxBytesError
		switch {
		case errors.As(err, &mbe):
			writeErr(w, &auth.Error{Status: 413, Code: "too_large", Message: fmt.Sprintf("Arquivo grande demais (máximo %d MB).", limit>>20)})
		case err.Error() == "empty":
			writeErr(w, &auth.Error{Status: 400, Code: "empty", Message: "O arquivo está vazio."})
		default:
			a.log.Error().Err(err).Str("file", name).Msg("game upload failed")
			writeErr(w, &auth.Error{Status: 500, Code: "storage", Message: "O envio foi interrompido. Tente novamente."})
		}
		return
	}

	a.auth.Audit(r.Context(), acc, "game.upload", sys.Id+"/"+name)
	a.hub.scheduleRescan()
	writeJson(w, http.StatusOK, map[string]any{"ok": true, "name": name, "system": sys.Id, "size": n})
}

// rescanGames tells all workers to scan the library again (after a batch of uploads).
func (a *API) rescanGames(w http.ResponseWriter, r *http.Request, acc *auth.Account, _ string) {
	if !a.gamesEnabled(w, acc) {
		return
	}
	count := a.hub.rescanLibraries()
	a.auth.Audit(r.Context(), acc, "game.rescan", "")
	writeJson(w, http.StatusOK, map[string]any{"ok": true, "workers": count})
}

var (
	rescanMu    sync.Mutex
	rescanTimer *time.Timer
)

// scheduleRescan scans the libraries a few seconds after the last upload, so new games show up
// even when the admin closes the page before the batch ends.
func (h *Hub) scheduleRescan() {
	rescanMu.Lock()
	defer rescanMu.Unlock()
	if rescanTimer != nil {
		rescanTimer.Stop()
	}
	rescanTimer = time.AfterFunc(5*time.Second, func() { h.rescanLibraries() })
}

// rescanLibraries asks every connected worker to scan its game library again.
func (h *Hub) rescanLibraries() int {
	list := h.workerList()
	for _, w := range list {
		w.Notify(api.LibRescan, nil)
	}
	return len(list)
}
