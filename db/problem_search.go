package db

import (
	"context"
	"strings"

	"github.com/KiloProjects/kilonova"
	"github.com/KiloProjects/kilonova/sudoapi/flags"
	sq "github.com/Masterminds/squirrel"
)

// Keep matching, counting and preview access checks identical. Attachment visibility
// controls the attachment list; private controls access to statement variants.
func statementSearchQuery(filter *kilonova.ProblemFilter) sq.SelectBuilder {
	query := sq.Select().PlaceholderFormat(sq.Question).From("problem_statement_search search").
		Join("attachments attachment ON attachment.id = search.attachment_id").
		Join("problem_attachments_m2m m2m ON m2m.attachment_id = attachment.id").
		Where("m2m.problem_id = problems.id").
		Where("search.search_vector @@ websearch_to_tsquery('problem_search', ?)", strings.TrimSpace(*filter.Search))
	if filter.Language != nil {
		query = query.Where(sq.Or{
			sq.Expr("split_part(attachment.name, '-', 2) = ?", *filter.Language),
			sq.Eq{"attachment.name": "statement-" + *filter.Language + ".md"},
		})
	}
	if filter.LookingUser != nil && filter.LookingUser.IsAdmin() {
		return query
	}
	access := sq.Or{sq.Eq{"attachment.private": false}}
	if filter.LookingUser != nil {
		editor := sq.And{sq.Expr("EXISTS (SELECT 1 FROM problem_editors WHERE problem_id = problems.id AND user_id = ?)", filter.LookingUser.ID)}
		if flags.LockdownProblemEditor.Value() {
			editor = append(editor, sq.Eq{"problems.visible": false})
		}
		access = append(access, editor)
	}
	return query.Where(access)
}

// ProblemSearchPreviews reads only the returned page, not every matching statement.
// ts_headline's output is never HTML: the client renders the markers as text nodes.
func (s *DB) ProblemSearchPreviews(ctx context.Context, filter kilonova.ProblemFilter, ids []int) (map[int]*kilonova.ProblemSearchPreview, error) {
	previews := make(map[int]*kilonova.ProblemSearchPreview)
	if len(ids) == 0 || filter.Search == nil || strings.TrimSpace(*filter.Search) == "" {
		return previews, nil
	}
	headline := statementSearchQuery(&filter).
		Column(sq.Expr("ts_headline('problem_search', search.content, websearch_to_tsquery('problem_search', ?), ?) AS text", strings.TrimSpace(*filter.Search), "StartSel=, StopSel=, MaxWords=40, MinWords=15, MaxFragments=2, FragmentDelimiter=\" … \"")).
		Column("attachment.name").
		OrderByClause(sq.Expr("ts_rank_cd(search.search_vector, websearch_to_tsquery('problem_search', ?)) DESC", strings.TrimSpace(*filter.Search))).
		OrderBy("attachment.name", "search.fragment").Limit(1)
	query, args, err := sq.Select("problems.id", "preview.text", "preview.name").From("problems").
		JoinClause(sq.Expr("JOIN LATERAL (?) preview ON TRUE", headline)).
		Where(sq.Eq{"problems.id": ids}).ToSql()
	if err != nil {
		return nil, err
	}
	rows, err := s.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		var name string
		var preview kilonova.ProblemSearchPreview
		if err := rows.Scan(&id, &preview.Text, &name); err != nil {
			return nil, err
		}
		variant := strings.TrimSuffix(strings.TrimPrefix(name, "statement-"), ".md")
		preview.Language, _, _ = strings.Cut(variant, "-")
		preview.Variant = strings.Replace(variant, preview.Language, preview.Language+"-md", 1)
		previews[id] = &preview
	}
	return previews, rows.Err()
}
