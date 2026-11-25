package clients

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var ErrQuery = errors.New("invalid client list query")

type ListQuery struct {
	ID        int64
	Name      string
	Search    string
	Disabled  *bool
	Hidden    *bool
	Withdrawn *bool
	After     int64
	Limit     int
}

type Page struct {
	Clients []Client
	NextID  int64
}

// List returns metadata only, in stable ascending ID order. Name matching is a
// literal substring, not a SQL pattern or a second full-text search system.
func (r *Repository) List(ctx context.Context, query ListQuery) (Page, error) {
	if query.Limit == 0 {
		query.Limit = 50
	}
	if query.Limit < 1 || query.Limit > 100 || query.ID < 0 || query.After < 0 || !utf8.ValidString(query.Name) || utf8.RuneCountInString(query.Name) > 80 || !utf8.ValidString(query.Search) || utf8.RuneCountInString(query.Search) > 80 {
		return Page{}, ErrQuery
	}
	conditions := []string{"id > ?"}
	args := []any{query.After}
	if query.ID != 0 {
		conditions = append(conditions, "id = ?")
		args = append(args, query.ID)
	}
	if query.Name != "" {
		conditions = append(conditions, "instr(display_name, ?) > 0")
		args = append(args, query.Name)
	}
	search := strings.TrimSpace(query.Search)
	if search != "" {
		// 正整数同时匹配席位 ID、GitHub ID、昵称字面子串与实例 server_id；非正整数匹配昵称与 server_id。
		id, err := strconv.ParseInt(search, 10, 64)
		if err == nil && id > 0 && strconv.FormatInt(id, 10) == search {
			conditions = append(conditions, "(id = ? OR github_id = ? OR instr(display_name, ?) > 0 OR instr(lower(server_id), lower(?)) > 0)")
			args = append(args, id, id, search, search)
		} else {
			conditions = append(conditions, "(instr(display_name, ?) > 0 OR instr(lower(server_id), lower(?)) > 0)")
			args = append(args, search, search)
		}
	}
	if query.Disabled != nil {
		conditions = append(conditions, "disabled = ?")
		args = append(args, *query.Disabled)
	}
	if query.Hidden != nil {
		conditions = append(conditions, "ranking_hidden = ?")
		args = append(args, *query.Hidden)
	}
	if query.Withdrawn != nil {
		conditions = append(conditions, "withdrawn = ?")
		args = append(args, *query.Withdrawn)
	}
	args = append(args, query.Limit+1)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := r.reader.QueryContext(ctx, selectClient+" WHERE "+strings.Join(conditions, " AND ")+" ORDER BY id LIMIT ?", args...)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	page := Page{Clients: make([]Client, 0, query.Limit)}
	for rows.Next() {
		client, err := scanClient(rows)
		if err != nil {
			return Page{}, err
		}
		page.Clients = append(page.Clients, client)
	}
	if err := rows.Err(); err != nil {
		return Page{}, err
	}
	if len(page.Clients) > query.Limit {
		page.Clients = page.Clients[:query.Limit]
		page.NextID = page.Clients[len(page.Clients)-1].ID
	}
	return page, nil
}
