package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"
)

// Sabres: the rhythm game. Songs (audio kept on disk, chart and metadata here) are shared by
// every account; each finished run is a score row used for rankings and play history.

type SabresSong struct {
	Id         int64
	Title      string
	Artist     string
	DurationMs int64
	Bpm        int
	File       string
	Size       int64
	Mime       string
	HasChart   bool
	UploadedBy *int64
	Uploader   string
	CreatedAt  time.Time
}

type SabresScore struct {
	Id         int64
	SongId     int64
	SongTitle  string
	UserId     int64
	UserName   string
	Difficulty string
	Score      int64
	Accuracy   int // per mille
	MaxCombo   int
	Grade      string
	Hits       int
	Misses     int
	PlayedAt   time.Time
}

// SabresSongStat is what one account did on a song.
type SabresSongStat struct {
	SongId     int64
	Plays      int
	BestScore  int64
	BestGrade  string
	BestAcc    int
	LastPlayed time.Time
}

// SabresTop is the best score of a song.
type SabresTop struct {
	Score int64
	Name  string
}

// SabresPlayer is a line of the overall ranking.
type SabresPlayer struct {
	UserId int64
	Name   string
	Total  int64
	Songs  int
}

const songCols = `s.id, s.title, s.artist, s.duration_ms, s.bpm, s.file, s.size, s.mime,
	(CASE WHEN s.chart <> '' THEN 1 ELSE 0 END), s.uploaded_by, COALESCE(u.name, ''), s.created_at`

func scanSong(r scanner) (*SabresSong, error) {
	var s SabresSong
	var by sql.NullInt64
	var created int64
	var hasChart int
	if err := r.Scan(&s.Id, &s.Title, &s.Artist, &s.DurationMs, &s.Bpm, &s.File, &s.Size, &s.Mime,
		&hasChart, &by, &s.Uploader, &created); err != nil {
		return nil, err
	}
	s.HasChart = hasChart != 0
	s.UploadedBy = fromNullInt(by)
	s.CreatedAt = fromUnix(created)
	return &s, nil
}

func (s *Store) AddSabresSong(ctx context.Context, song *SabresSong) (int64, error) {
	return insertID(ctx, s.db, s.db.pg,
		`INSERT INTO sabres_songs(title, artist, duration_ms, bpm, file, size, mime, chart, uploaded_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?)`,
		song.Title, song.Artist, song.DurationMs, song.Bpm, song.File, song.Size, song.Mime, song.UploadedBy, unix(time.Now()))
}

func (s *Store) SabresSong(ctx context.Context, id int64) (*SabresSong, error) {
	song, err := scanSong(s.db.QueryRowContext(ctx,
		`SELECT `+songCols+` FROM sabres_songs s LEFT JOIN users u ON u.id = s.uploaded_by WHERE s.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return song, err
}

func (s *Store) SabresSongs(ctx context.Context) ([]*SabresSong, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+songCols+` FROM sabres_songs s LEFT JOIN users u ON u.id = s.uploaded_by ORDER BY s.created_at DESC, s.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*SabresSong
	for rows.Next() {
		song, err := scanSong(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, song)
	}
	return list, rows.Err()
}

func (s *Store) SabresChart(ctx context.Context, id int64) (string, error) {
	var chart string
	err := s.db.QueryRowContext(ctx, `SELECT chart FROM sabres_songs WHERE id = ?`, id).Scan(&chart)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return chart, err
}

func (s *Store) SetSabresChart(ctx context.Context, id int64, chart string, bpm int) error {
	res, err := s.db.ExecContext(ctx, `UPDATE sabres_songs SET chart = ?, bpm = ? WHERE id = ?`, chart, bpm, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteSabresSong(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sabres_songs WHERE id = ?`, id)
	return err
}

func (s *Store) AddSabresScore(ctx context.Context, sc *SabresScore) (int64, error) {
	return insertID(ctx, s.db, s.db.pg,
		`INSERT INTO sabres_scores(song_id, user_id, difficulty, score, accuracy, max_combo, grade, hits, misses, played_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sc.SongId, sc.UserId, sc.Difficulty, sc.Score, sc.Accuracy, sc.MaxCombo, sc.Grade, sc.Hits, sc.Misses, unix(time.Now()))
}

func scanScores(rows *sql.Rows) ([]*SabresScore, error) {
	defer rows.Close()
	var list []*SabresScore
	for rows.Next() {
		var sc SabresScore
		var played int64
		if err := rows.Scan(&sc.Id, &sc.SongId, &sc.SongTitle, &sc.UserId, &sc.UserName, &sc.Difficulty, &sc.Score,
			&sc.Accuracy, &sc.MaxCombo, &sc.Grade, &sc.Hits, &sc.Misses, &played); err != nil {
			return nil, err
		}
		sc.PlayedAt = fromUnix(played)
		list = append(list, &sc)
	}
	return list, rows.Err()
}

const scoreCols = `sc.id, sc.song_id, so.title, sc.user_id, u.name, sc.difficulty, sc.score, sc.accuracy,
	sc.max_combo, sc.grade, sc.hits, sc.misses, sc.played_at`
const scoreJoin = ` FROM sabres_scores sc JOIN sabres_songs so ON so.id = sc.song_id JOIN users u ON u.id = sc.user_id`

// SabresRanking returns the best run of each player in a song (difficulty "" = any), best first.
func (s *Store) SabresRanking(ctx context.Context, songId int64, difficulty string, limit int) ([]*SabresScore, error) {
	q := `SELECT ` + scoreCols + scoreJoin + ` WHERE sc.song_id = ?`
	args := []any{songId}
	if difficulty != "" {
		q += ` AND sc.difficulty = ?`
		args = append(args, difficulty)
	}
	q += ` ORDER BY sc.score DESC, sc.played_at ASC LIMIT 1000`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	all, err := scanScores(rows)
	if err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	var best []*SabresScore
	for _, sc := range all {
		if seen[sc.UserId] {
			continue
		}
		seen[sc.UserId] = true
		best = append(best, sc)
		if len(best) >= limit {
			break
		}
	}
	return best, nil
}

// SabresHistory lists the most recent runs of an account.
func (s *Store) SabresHistory(ctx context.Context, userId int64, limit int) ([]*SabresScore, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+scoreCols+scoreJoin+` WHERE sc.user_id = ? ORDER BY sc.played_at DESC, sc.id DESC LIMIT ?`, userId, limit)
	if err != nil {
		return nil, err
	}
	return scanScores(rows)
}

// SabresUserStats summarizes, per song, what an account did.
func (s *Store) SabresUserStats(ctx context.Context, userId int64) (map[int64]*SabresSongStat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT song_id, score, accuracy, grade, played_at FROM sabres_scores WHERE user_id = ? ORDER BY played_at ASC, id ASC`, userId)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stats := map[int64]*SabresSongStat{}
	for rows.Next() {
		var id, score, played int64
		var acc int
		var grade string
		if err := rows.Scan(&id, &score, &acc, &grade, &played); err != nil {
			return nil, err
		}
		st := stats[id]
		if st == nil {
			st = &SabresSongStat{SongId: id}
			stats[id] = st
		}
		st.Plays++
		st.LastPlayed = fromUnix(played)
		if score >= st.BestScore {
			st.BestScore, st.BestGrade, st.BestAcc = score, grade, acc
		}
	}
	return stats, rows.Err()
}

// SabresTops returns the best score of every song.
func (s *Store) SabresTops(ctx context.Context) (map[int64]SabresTop, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT sc.song_id, sc.score, u.name FROM sabres_scores sc JOIN users u ON u.id = sc.user_id
		 ORDER BY sc.score DESC, sc.played_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tops := map[int64]SabresTop{}
	for rows.Next() {
		var id, score int64
		var name string
		if err := rows.Scan(&id, &score, &name); err != nil {
			return nil, err
		}
		if _, ok := tops[id]; !ok {
			tops[id] = SabresTop{Score: score, Name: name}
		}
	}
	return tops, rows.Err()
}

// SabresOverall ranks the players by the sum of their best score in each song.
func (s *Store) SabresOverall(ctx context.Context, limit int) ([]*SabresPlayer, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT sc.user_id, u.name, sc.song_id, MAX(sc.score) FROM sabres_scores sc JOIN users u ON u.id = sc.user_id
		 GROUP BY sc.user_id, u.name, sc.song_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byUser := map[int64]*SabresPlayer{}
	for rows.Next() {
		var uid, song, best int64
		var name string
		if err := rows.Scan(&uid, &name, &song, &best); err != nil {
			return nil, err
		}
		p := byUser[uid]
		if p == nil {
			p = &SabresPlayer{UserId: uid, Name: name}
			byUser[uid] = p
		}
		p.Total += best
		p.Songs++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	list := make([]*SabresPlayer, 0, len(byUser))
	for _, p := range byUser {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Total != list[j].Total {
			return list[i].Total > list[j].Total
		}
		return list[i].UserId < list[j].UserId
	})
	if len(list) > limit {
		list = list[:limit]
	}
	return list, nil
}
