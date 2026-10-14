package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// Runs only against a real PostgreSQL:
//
//	BELKO_TEST_POSTGRES_URL=postgres://belko:senha@localhost:5432/belko_test?sslmode=disable go test ./pkg/store
func TestPostgres(t *testing.T) {
	url := os.Getenv("BELKO_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("BELKO_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	s, err := Open(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	// a second Open must be a no-op (migrations are idempotent)
	s2, err := Open(url)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_ = s2.Close()

	suffix := time.Now().Format("150405.000000")
	email := "Admin-" + suffix + "@Example.com"

	u := &User{Email: email, Name: "Admin", PasswordHash: "h", Role: RoleAdmin, Status: StatusApproved}
	if err := s.CreateUser(ctx, u); err != nil || u.Id == 0 {
		t.Fatalf("create user: id=%d err=%v", u.Id, err)
	}
	// e-mails are case-insensitive, on lookup and on uniqueness
	got, err := s.UserByEmail(ctx, "ADMIN-"+suffix+"@example.com")
	if err != nil || got.Id != u.Id {
		t.Fatalf("lookup by e-mail with other case: %v", err)
	}
	dup := &User{Email: "admin-" + suffix + "@example.com", Name: "x", PasswordHash: "h", Role: RoleUser, Status: StatusPending}
	if err := s.CreateUser(ctx, dup); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate e-mail: got %v, want ErrDuplicate", err)
	}
	if _, err := s.UserByEmail(ctx, "nobody-"+suffix+"@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing user: got %v", err)
	}

	// time-based columns hold unix seconds (BIGINT)
	if err := s.TouchLogin(ctx, u.Id); err != nil {
		t.Fatal(err)
	}
	got, _ = s.UserById(ctx, u.Id)
	if got.LastLoginAt == nil || time.Since(*got.LastLoginAt) > time.Minute {
		t.Fatalf("last login: %v", got.LastLoginAt)
	}

	// sessions + cascade
	now := time.Now()
	if err := s.CreateSession(ctx, Session{TokenHash: "tok-" + suffix, UserId: u.Id, CreatedAt: now,
		ExpiresAt: now.Add(time.Hour), LastSeenAt: now, Ip: "1.2.3.4", UserAgent: "go-test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionByHash(ctx, "tok-"+suffix); err != nil {
		t.Fatal(err)
	}

	// upserts, audit join and aggregates
	if err := s.SetPreferences(ctx, u.Id, `{"a":1}`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPreferences(ctx, u.Id, `{"a":2}`); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Preferences(ctx, u.Id); p != `{"a":2}` {
		t.Fatalf("preferences: %s", p)
	}
	if err := s.Audit(ctx, &u.Id, "test.audit", &u.Id, "d"); err != nil {
		t.Fatal(err)
	}
	log, err := s.AuditLog(ctx, 10)
	if err != nil || len(log) == 0 || log[0].Action != "test.audit" || log[0].ActorName != "Admin" {
		t.Fatalf("audit log: %+v %v", log, err)
	}
	if err := s.SetCover(ctx, Cover{Key: "k-" + suffix, System: "nes", Title: "t", Status: CoverFound}); err != nil {
		t.Fatal(err)
	}
	if f, _, err := s.CoverStats(ctx); err != nil || f < 1 {
		t.Fatalf("cover stats: %d %v", f, err)
	}
	if err := s.SetGameMeta(ctx, GameMeta{Key: "m-" + suffix, System: "nes", Title: "t", Genre: "g", Status: MetaFound}); err != nil {
		t.Fatal(err)
	}
	if f, _, err := s.GameMetaStats(ctx); err != nil || f < 1 {
		t.Fatalf("meta stats: %d %v", f, err)
	}
	if err := s.SetAvatar(ctx, u.Id, []byte{0x89, 'P', 'N', 'G', 0, 1, 2}); err != nil {
		t.Fatal(err)
	}
	if b, _, err := s.Avatar(ctx, u.Id); err != nil || len(b) != 7 {
		t.Fatalf("avatar: %v %v", b, err)
	}

	// tournaments: RETURNING id, bracket in a transaction, duplicate player
	tr := &Tournament{Name: "T", Game: "g", System: "nes", Rule: "r", Status: "open", CreatedBy: &u.Id}
	if err := s.CreateTournament(ctx, tr); err != nil || tr.Id == 0 {
		t.Fatalf("create tournament: %v", err)
	}
	if err := s.AddTournamentPlayer(ctx, tr.Id, u.Id); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTournamentPlayer(ctx, tr.Id, u.Id); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate player: got %v", err)
	}
	ms := []*Match{{Round: 1, Position: 0, P1: &u.Id, Status: "ready"}, {Round: 2, Position: 0, Status: "waiting"}}
	if err := s.CreateBracket(ctx, tr.Id, ms); err != nil || ms[0].Id == 0 || ms[1].Id == ms[0].Id {
		t.Fatalf("bracket: %v %+v", err, ms)
	}
	ms[0].Status = "running"
	if err := s.UpdateMatch(ctx, ms[0]); err != nil {
		t.Fatal(err)
	}
	if open, err := s.OpenMatches(ctx); err != nil || len(open) == 0 {
		t.Fatalf("open matches: %v", err)
	}
	rp := &ReplayInfo{MatchId: &ms[0].Id, Game: "g", Frames: 10, Size: 20, File: "f"}
	if err := s.CreateReplay(ctx, rp); err != nil || rp.Id == 0 {
		t.Fatalf("replay: %v", err)
	}

	// deleting the user cascades to sessions and keeps the audit row
	if _, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, u.Id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionByHash(ctx, "tok-"+suffix); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session should be gone: %v", err)
	}
}
