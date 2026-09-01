package store

// Message search.
//
// Text is matched as a substring of a folded copy of the message rather than
// through a word index. That is a deliberate choice: a word index has to decide
// where words end, and no single tokenizer makes that decision correctly for
// Spanish and Chinese at once, while a substring means the same thing in every
// language the client is translated into. It also matches the middle of a word,
// which is what somebody hunting for half-remembered wording actually wants.
//
// What it costs is a scan, and what bounds that scan is the filtering the
// caller has already done: a search only ever runs over the channels its author
// may read, and usually over fewer.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/aural-chat/aural-server/internal/protocol"
)

// backfillBatch is how many rows one pass of the backfill folds. It is bounded
// so that opening a database with a long history does not hold one enormous
// transaction open.
const backfillBatch = 500

// backfillSearchText folds every message that has never been folded.
//
// It exists because search_text arrived after the messages did. A row written
// before the column existed carries NULL, and the partial index the migration
// creates holds exactly those rows, so on a database that is already folded
// this costs one empty index scan.
func (s *Store) backfillSearchText(ctx context.Context) error {
	for {
		rows, err := s.db.QueryContext(ctx,
			`SELECT id, content FROM messages WHERE search_text IS NULL LIMIT ?`, backfillBatch)
		if err != nil {
			return fmt.Errorf("store: read unfolded messages: %w", err)
		}

		type folded struct {
			id   int64
			text string
		}
		batch := make([]folded, 0, backfillBatch)
		for rows.Next() {
			var id int64
			var content string
			if err := rows.Scan(&id, &content); err != nil {
				rows.Close()
				return fmt.Errorf("store: read unfolded messages: %w", err)
			}
			batch = append(batch, folded{id: id, text: foldForSearch(content)})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("store: read unfolded messages: %w", err)
		}
		if len(batch) == 0 {
			return nil
		}

		err = s.tx(ctx, func(tx *sql.Tx) error {
			for _, row := range batch {
				if _, err := tx.ExecContext(ctx,
					`UPDATE messages SET search_text = ? WHERE id = ?`, row.text, row.id); err != nil {
					return fmt.Errorf("store: fold message %d: %w", row.id, err)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
}

// SearchFilter is one search, already reduced to what the query needs.
//
// Every field narrows the result and they are combined with AND; entries within
// one field are alternatives. ChannelIDs is not optional: the gateway fills it
// with the channels the caller may read, so a search can never reach past them.
type SearchFilter struct {
	// Terms are folded substrings, all of which must appear in the message.
	Terms      []string
	ChannelIDs []int64
	AuthorIDs  []int64
	// Has names kinds of content the message must carry, using the protocol's
	// Has constants.
	Has []string
	// After is inclusive and Before exclusive, both in Unix seconds.
	After  int64
	Before int64
	Sort   string
	Limit  int
	Offset int
}

// SearchMessages returns one page of matches and how many there were in all.
func (s *Store) SearchMessages(ctx context.Context, f SearchFilter) ([]Message, int, error) {
	if len(f.ChannelIDs) == 0 {
		return nil, 0, nil
	}
	where, args := f.where()

	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages m WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: count search matches: %w", err)
	}
	if total == 0 || f.Offset >= total {
		return nil, total, nil
	}

	order, orderArgs := f.orderBy()
	page := append(append([]any{}, args...), orderArgs...)
	page = append(page, f.Limit, f.Offset)

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+messageColumns+messageFrom+` WHERE `+where+order+` LIMIT ? OFFSET ?`, page...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: search messages: %w", err)
	}
	hits, err := scanMessages(rows, f.Limit)
	if err != nil {
		return nil, 0, err
	}
	return hits, total, nil
}

// where builds the filter shared by the count and the page, so the two can
// never disagree about what matched.
func (f SearchFilter) where() (string, []any) {
	clauses := []string{`m.channel_id IN (` + placeholders(len(f.ChannelIDs)) + `)`}
	args := idArgs(f.ChannelIDs)

	if len(f.AuthorIDs) > 0 {
		clauses = append(clauses, `m.user_id IN (`+placeholders(len(f.AuthorIDs))+`)`)
		args = append(args, idArgs(f.AuthorIDs)...)
	}
	for _, term := range f.Terms {
		clauses = append(clauses, `m.search_text LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(term)+"%")
	}
	if f.After > 0 {
		clauses = append(clauses, `m.created_at >= ?`)
		args = append(args, f.After)
	}
	if f.Before > 0 {
		clauses = append(clauses, `m.created_at < ?`)
		args = append(args, f.Before)
	}
	for _, has := range f.Has {
		switch has {
		case protocol.HasLink:
			// A link is what the client turns into an embed, and every one it
			// recognises starts the same way.
			clauses = append(clauses, `(m.search_text LIKE '%http://%' OR m.search_text LIKE '%https://%')`)
		case protocol.HasFile:
			clauses = append(clauses, attachmentExists(""))
		case protocol.HasImage:
			clauses = append(clauses, attachmentExists("image/"))
		case protocol.HasVideo:
			clauses = append(clauses, attachmentExists("video/"))
		case protocol.HasSound:
			clauses = append(clauses, attachmentExists("audio/"))
		}
	}
	return strings.Join(clauses, " AND "), args
}

// attachmentExists asks whether a message carries a file, optionally of one
// media type. The prefix is a constant of this package and never user input.
func attachmentExists(typePrefix string) string {
	clause := `EXISTS (SELECT 1 FROM attachments a WHERE a.message_id = m.id`
	if typePrefix != "" {
		clause += ` AND a.content_type LIKE '` + typePrefix + `%'`
	}
	return clause + `)`
}

// orderBy builds the sort, along with the arguments its expressions need.
//
// Relevance has no index behind it and no corpus statistics to draw on, so it
// is stated plainly rather than dressed up as a score: a message where the whole
// query appears as written outranks one where the words merely all appear, a
// message that repeats them outranks one that mentions them once, and recency
// breaks the remaining ties. Without terms to weigh there is nothing to rank,
// and it falls back to the newest.
func (f SearchFilter) orderBy() (string, []any) {
	switch f.Sort {
	case protocol.SortOldest:
		return ` ORDER BY m.id ASC`, nil
	case protocol.SortRelevance:
		if len(f.Terms) == 0 {
			break
		}
		phrase := strings.Join(f.Terms, " ")
		args := []any{"%" + escapeLike(phrase) + "%"}
		occurrences := make([]string, 0, len(f.Terms))
		for _, term := range f.Terms {
			// How many times a term occurs: the length the message loses when
			// every copy of the term is cut out of it, over the term's length.
			occurrences = append(occurrences,
				fmt.Sprintf(`(LENGTH(m.search_text) - LENGTH(REPLACE(m.search_text, ?, ''))) / %d`,
					utf8.RuneCountInString(term)))
			args = append(args, term)
		}
		return ` ORDER BY (CASE WHEN m.search_text LIKE ? ESCAPE '\' THEN 1 ELSE 0 END) DESC, (` +
			strings.Join(occurrences, " + ") + `) DESC, m.id DESC`, args
	}
	return ` ORDER BY m.id DESC`, nil
}

// escapeLike neutralises the two wildcards a LIKE pattern gives meaning to, so
// a search for "100%" looks for that and not for "100" followed by anything.
func escapeLike(term string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(term)
}

// SearchTerms splits free text into the substrings every match must contain.
//
// Whitespace separates terms and double quotes hold a phrase together, which is
// the convention every search box has taught people to expect. Terms are folded
// the same way message text is, so the comparison is like for like.
func SearchTerms(query string, max int) []string {
	var (
		terms   []string
		current strings.Builder
		quoted  bool
	)
	flush := func() {
		term := foldForSearch(strings.TrimSpace(current.String()))
		current.Reset()
		if term != "" && len(terms) < max {
			terms = append(terms, term)
		}
	}

	for _, r := range query {
		switch {
		case r == '"':
			// A closing quote ends the phrase; an opening one starts it, and
			// whatever was being typed before it is a term of its own.
			flush()
			quoted = !quoted
		case !quoted && (r == ' ' || r == '\t' || r == '\n' || r == '\r'):
			flush()
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return terms
}

// Neighbours are the messages either side of one search hit in its own channel.
// Either end is nil at the edge of a channel's history.
type Neighbours struct {
	Before *int64
	After  *int64
}

// NeighbourIDs finds the message immediately before and after each of a page of
// hits, in one query rather than two per hit.
//
// A hit is read with the line before it because a line of chat rarely means
// anything alone: what makes a result recognisable is what it was answering.
func (s *Store) NeighbourIDs(ctx context.Context, ids []int64) (map[int64]Neighbours, error) {
	out := make(map[int64]Neighbours, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT hit.id,
		       (SELECT MAX(b.id) FROM messages b
		         WHERE b.channel_id = hit.channel_id AND b.id < hit.id),
		       (SELECT MIN(a.id) FROM messages a
		         WHERE a.channel_id = hit.channel_id AND a.id > hit.id)
		  FROM messages hit
		 WHERE hit.id IN (`+placeholders(len(ids))+`)`, idArgs(ids)...)
	if err != nil {
		return nil, fmt.Errorf("store: read hit neighbours: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var neighbours Neighbours
		if err := rows.Scan(&id, &neighbours.Before, &neighbours.After); err != nil {
			return nil, fmt.Errorf("store: read hit neighbours: %w", err)
		}
		out[id] = neighbours
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read hit neighbours: %w", err)
	}
	return out, nil
}
