// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/delinoio/mono/servers/accounts-api/internal/db"
)

// interleavingQuerier is a test-only db.Querier that forces the interleaving
// of a concurrent commit with a read. It runs the first statement that it
// receives to completion and buffers its whole result, then calls
// afterFirst, which commits a change on another connection, and only then
// returns the buffered result. Every later statement runs normally and so
// sees the commit.
//
// A read that uses one statement is consistent: the statement ran before the
// commit and returns the state before it. A read that uses several
// statements pairs the result before the commit with later results, which
// is the inconsistency of a read without one snapshot.
type interleavingQuerier struct {
	inner      db.Querier
	afterFirst func()
	done       bool
}

func (q *interleavingQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return q.inner.Exec(ctx, sql, args...)
}

func (q *interleavingQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := q.inner.Query(ctx, sql, args...)
	if err != nil || q.done {
		return rows, err
	}
	q.done = true
	buffered, err := bufferRows(rows)
	if err != nil {
		return nil, err
	}
	q.afterFirst()
	return buffered, nil
}

func (q *interleavingQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	rows, err := q.Query(ctx, sql, args...)
	return &bufferedRow{rows: rows, err: err}
}

// bufferedRow is the pgx.Row of a buffered result.
type bufferedRow struct {
	rows pgx.Rows
	err  error
}

func (r *bufferedRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	defer r.rows.Close()
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return err
		}
		return pgx.ErrNoRows
	}
	return r.rows.Scan(dest...)
}

// bufferedRows is a fully read result of a statement.
type bufferedRows struct {
	fields  []pgconn.FieldDescription
	tag     pgconn.CommandTag
	raw     [][][]byte
	next    int
	typeMap *pgtype.Map
}

func bufferRows(rows pgx.Rows) (*bufferedRows, error) {
	defer rows.Close()
	b := &bufferedRows{typeMap: pgtype.NewMap()}
	b.fields = slices.Clone(rows.FieldDescriptions())
	for rows.Next() {
		var row [][]byte
		for _, v := range rows.RawValues() {
			// The driver reuses its buffer, so copy the bytes; a NULL stays nil.
			if v == nil {
				row = append(row, nil)
				continue
			}
			row = append(row, slices.Clone(v))
		}
		b.raw = append(b.raw, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	b.tag = rows.CommandTag()
	return b, nil
}

func (b *bufferedRows) Close()                                       {}
func (b *bufferedRows) Err() error                                   { return nil }
func (b *bufferedRows) CommandTag() pgconn.CommandTag                { return b.tag }
func (b *bufferedRows) FieldDescriptions() []pgconn.FieldDescription { return b.fields }
func (b *bufferedRows) Conn() *pgx.Conn                              { return nil }

func (b *bufferedRows) Next() bool {
	if b.next >= len(b.raw) {
		return false
	}
	b.next++
	return true
}

func (b *bufferedRows) RawValues() [][]byte { return b.raw[b.next-1] }

func (b *bufferedRows) Scan(dest ...any) error {
	row := b.raw[b.next-1]
	for i, d := range dest {
		f := b.fields[i]
		if err := b.typeMap.Scan(f.DataTypeOID, f.Format, row[i], d); err != nil {
			return err
		}
	}
	return nil
}

func (b *bufferedRows) Values() ([]any, error) {
	row := b.raw[b.next-1]
	out := make([]any, len(row))
	for i, v := range row {
		f := b.fields[i]
		if v == nil {
			continue
		}
		t, ok := b.typeMap.TypeForOID(f.DataTypeOID)
		if !ok {
			return nil, fmt.Errorf("unknown type OID %d", f.DataTypeOID)
		}
		var err error
		if out[i], err = t.Codec.DecodeValue(b.typeMap, f.DataTypeOID, f.Format, v); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (b *bufferedRows) TypeMap() *pgtype.Map { return b.typeMap }
