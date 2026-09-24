package query

import (
	"context"

	"github.com/hao-ji-xing/agentory/internal/index"
)

// SQLResult is the outcome of a read-only query.
type SQLResult struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Truncated bool     `json:"truncated"` // more rows than maxRows existed
}

// RunSQL executes q with writes disabled (PRAGMA query_only) and returns
// at most maxRows rows (0 = unlimited). ctx bounds the running time.
func RunSQL(ctx context.Context, db *index.DB, q string, maxRows int) (*SQLResult, error) {
	// The index uses a single connection, so the pragma applies to q.
	if _, err := db.ExecContext(ctx, `PRAGMA query_only = 1`); err != nil {
		return nil, err
	}
	defer db.Exec(`PRAGMA query_only = 0`)
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	res := &SQLResult{Columns: cols, Rows: [][]any{}}
	for rows.Next() {
		if maxRows > 0 && len(res.Rows) == maxRows {
			res.Truncated = true
			break
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		res.Rows = append(res.Rows, vals)
	}
	return res, rows.Err()
}
