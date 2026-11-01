package db

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/KiloProjects/kilonova"
	sq "github.com/Masterminds/squirrel"
	"github.com/jackc/pgx/v5"
)

// Run against a database migrated by this build. All fixtures are rolled back.
func TestProblemStatementSearch(t *testing.T) {
	dsn := os.Getenv("KN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set KN_TEST_DATABASE_URL to a migrated PostgreSQL database")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	insert := func(query string, args ...any) int {
		t.Helper()
		var id int
		if err := tx.QueryRow(ctx, query, args...).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	token := fmt.Sprintf("searchregression%d", time.Now().UnixNano())
	newProblem := func(name string, visible bool) int {
		return insert("INSERT INTO problems(name, test_name, visible) VALUES ($1, 'search', $2) RETURNING id", name, visible)
	}
	public := newProblem("Opaque title", true)
	hidden := newProblem("Hidden title", false)
	title := newProblem(token+" title", true)
	private := newProblem("Draft statement", true)
	ids := []int{public, hidden, title, private}
	attach := func(pb int, name string, data []byte, secret bool) int {
		aid := insert("INSERT INTO attachments(name, data, private) VALUES ($1, $2, $3) RETURNING id", name, data, secret)
		exec("INSERT INTO problem_attachments_m2m(problem_id, attachment_id) VALUES ($1, $2)", pb, aid)
		return aid
	}
	body := token + " pădure traversal red fox walks through a forest. There are many nodes and edges to examine."
	publicAtt := attach(public, "statement-en.md", []byte(body), false)
	attach(public, "statement-ro.md", []byte(token+" romana"), false)
	attach(hidden, "statement-en.md", []byte(body), false)
	privateAtt := attach(private, "statement-en.md", []byte(token+" secret"), true)
	attach(title, "statement-en.pdf", []byte(token+" pdfonly"), false)
	attach(title, "statement-en-invalid.md", []byte{0xff, 0, 0xfe}, false)
	query := func(text string, lang *string, user *kilonova.UserBrief, limit, offset uint64) []int {
		t.Helper()
		filter := kilonova.ProblemFilter{IDs: ids, Search: &text, Language: lang, Look: true, LookingUser: user}
		qb := LimitOffset(sq.Select("problems.id").From("problems").Where(problemFilterQuery(&filter)).OrderBy("problems.id"), limit, offset)
		sql, args, err := qb.ToSql()
		if err != nil {
			t.Fatal(err)
		}
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			t.Fatal(err)
		}
		result, err := pgx.CollectRows(rows, pgx.RowTo[int])
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	expect := func(got, want []int) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	expect(query(token, nil, nil, 0, 0), []int{public, title})
	expect(query(token+" traversal", nil, nil, 0, 0), []int{public})
	expect(query(token+` "red fox"`, nil, nil, 0, 0), []int{public})
	expect(query(token+" padure", nil, nil, 0, 0), []int{public})
	expect(query(token+" -traversal", nil, nil, 0, 0), []int{public}) // Romanian variant has no excluded word.
	expect(query(token+" (traversal OR romana)", nil, nil, 0, 0), []int{public})
	expect(query(token, nil, nil, 1, 1), []int{title})
	expect(query("#"+fmt.Sprint(title), nil, nil, 0, 0), []int{title})
	ro, en := "ro", "en"
	expect(query(token+" traversal", &ro, nil, 0, 0), []int{})
	expect(query(token+" traversal", &en, nil, 0, 0), []int{public})
	expect(query(token+" secret", &en, nil, 0, 0), []int{})
	expect(query(token+" pdfonly", nil, nil, 0, 0), []int{})
	uid := insert("INSERT INTO users(name, email, password) VALUES ($1, $2, 'not-a-password') RETURNING id", token, token+"@example.invalid")
	exec("INSERT INTO problem_user_access(problem_id, user_id, access) VALUES ($1, $2, 'viewer')", private, uid)
	viewer := &kilonova.UserBrief{ID: uid}
	expect(query(token+" secret", nil, viewer, 0, 0), []int{})
	exec("UPDATE problem_user_access SET access = 'editor' WHERE problem_id = $1 AND user_id = $2", private, uid)
	expect(query(token+" secret", nil, viewer, 0, 0), []int{private})

	// Preview uses the same access predicate, preserves accents and emits text markers.
	text := token + " padure"
	filter := kilonova.ProblemFilter{Search: &text}
	preview := statementSearchQuery(&filter).Column("ts_headline('problem_search', content, websearch_to_tsquery('problem_search', 'padure'), 'StartSel=, StopSel=')").Limit(1)
	sql, args, err := sq.Select().Column(sq.Expr("(?)", preview)).From("problems").Where(sq.Eq{"id": public}).ToSql()
	if err != nil {
		t.Fatal(err)
	}
	var snippet string
	if err := tx.QueryRow(ctx, sql, args...).Scan(&snippet); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snippet, "\uE000pădure\uE001") {
		t.Fatalf("preview did not preserve and highlight accents: %q", snippet)
	}

	// Metadata, content edits, large files and deletes all update search immediately.
	exec("UPDATE attachments SET private = false WHERE id = $1", privateAtt)
	expect(query(token+" secret", nil, nil, 0, 0), []int{private})
	exec("UPDATE attachments SET name = 'notes.md' WHERE id = $1", privateAtt)
	expect(query(token+" secret", nil, nil, 0, 0), []int{})
	exec("UPDATE attachments SET data = $1 WHERE id = $2", []byte(strings.Repeat("padding ", 12000)+token+" lateword"), publicAtt)
	expect(query(token+" lateword", nil, nil, 0, 0), []int{public})
	expect(query(token+" traversal", nil, nil, 0, 0), []int{})
	exec("DELETE FROM attachments WHERE id = $1", publicAtt)
	expect(query(token+" lateword", nil, nil, 0, 0), []int{})
}
