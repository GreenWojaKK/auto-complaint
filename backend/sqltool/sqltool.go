// Package sqltool은 분석 모델이 쓴 SELECT 한 문장을 읽기 전용 거래 안에서 실행한다.
// 접속 계정(dllio_read)이 읽기 전용이고, 준비문(prepared statement)으로 실행해 여러 문장을 한 번에 보낼 수 없다.
package sqltool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Result의 Rows는 열 순서를 지킨 객체 목록이다. Total은 결과 전체 줄 수, Rows는 앞의 keep줄까지다.
type Result struct {
	Columns   []string          `json:"columns"`
	Rows      []json.RawMessage `json:"rows"`
	Total     int               `json:"total"`
	Truncated bool              `json:"truncated"`
}

func Run(ctx context.Context, db *pgxpool.Pool, query string, keep int) (*Result, error) {
	q := strings.TrimRight(strings.TrimSpace(query), "; \n\t")
	if q == "" {
		return nil, fmt.Errorf("SQL 문장이 비어 있다")
	}
	tx, err := db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := &Result{Columns: []string{}, Rows: []json.RawMessage{}}
	seen := map[string]int{}
	for _, f := range rows.FieldDescriptions() {
		name := f.Name
		if seen[name]++; seen[name] > 1 {
			name = fmt.Sprintf("%s_%d", name, seen[name])
		}
		res.Columns = append(res.Columns, name)
	}
	for rows.Next() {
		res.Total++
		if res.Total > keep {
			continue
		}
		values, err := rows.Values()
		if err != nil {
			return nil, err
		}
		var b bytes.Buffer
		b.WriteByte('{')
		for i, v := range values {
			if i > 0 {
				b.WriteByte(',')
			}
			k, _ := json.Marshal(res.Columns[i])
			b.Write(k)
			b.WriteByte(':')
			raw, err := json.Marshal(plain(v))
			if err != nil {
				return nil, err
			}
			b.Write(raw)
		}
		b.WriteByte('}')
		res.Rows = append(res.Rows, b.Bytes())
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	res.Truncated = res.Total > keep
	return res, nil
}

// plain은 PostgreSQL 값을 JSON으로 쓸 수 있는 값으로 바꾼다.
func plain(v any) any {
	switch x := v.(type) {
	case pgtype.Numeric:
		if !x.Valid {
			return nil
		}
		f, err := x.Float64Value()
		if err != nil || !f.Valid {
			return nil
		}
		return f.Float64
	case time.Time:
		if x.Hour() == 0 && x.Minute() == 0 && x.Second() == 0 && x.Nanosecond() == 0 {
			return x.Format("2006-01-02")
		}
		return x.Format(time.RFC3339)
	case pgtype.Interval:
		return fmt.Sprintf("%d months %d days %d microseconds", x.Months, x.Days, x.Microseconds)
	case []byte:
		return string(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = plain(e)
		}
		return out
	case map[string]any:
		return x
	}
	return v
}
