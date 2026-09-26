package postgres

import (
	"bytes"
	"context"
	"io"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// The three reads name the OIDs pg_locks holds: relation to pg_class, its
// relnamespace to pg_namespace, and database to pg_database. Every row, in the
// catalog's own order: no ORDER BY and no cap, the statement timeout the only
// bound.
const catalogClassSQL = `SELECT oid, relname::text, relnamespace, relkind::text
FROM pg_catalog.pg_class
WHERE relkind IN ('r', 'i', 'm', 'p')`

const catalogNamespaceSQL = `SELECT oid, nspname::text
FROM pg_catalog.pg_namespace`

const catalogDatabaseSQL = `SELECT oid, datname::text
FROM pg_catalog.pg_database`

// catalogBlock is one read and the block it writes. pg_class and pg_namespace
// are each database's own catalogs, so their blocks are scope=database: they
// name the connected database's objects only, and a lock in another database
// keeps its OIDs. pg_database is shared by the cluster.
type catalogBlock struct {
	source  string
	scope   string
	sql     string
	columns []string
	scan    func(pgx.Rows) ([]string, error)
}

var catalogBlocks = []catalogBlock{
	{
		source:  "pg_class",
		scope:   "database",
		sql:     catalogClassSQL,
		columns: []string{"oid", "relname", "relnamespace", "relkind"},
		scan:    scanCatalogClass,
	},
	{
		source:  "pg_namespace",
		scope:   "database",
		sql:     catalogNamespaceSQL,
		columns: []string{"oid", "nspname"},
		scan:    scanCatalogName,
	},
	{
		source:  "pg_database",
		scope:   "cluster",
		sql:     catalogDatabaseSQL,
		columns: []string{"oid", "datname"},
		scan:    scanCatalogName,
	},
}

// CatalogMap captures, once at the start, the names the server resolves
// pg_locks' OIDs against. The objects a lock can name rarely change within one
// window, so it is not resampled.
type CatalogMap struct{}

func (CatalogMap) Artifact() Artifact {
	return Artifact{
		Name:     "pg_catalog_map",
		FileName: "pg_catalog_map.txt",
		Scope:    "database",
		Schedule: Once(),

		// Three statements. Once samples at the start only, so this reaches the
		// closing tick's deadline only when the window has no length.
		SampleBudget: 3 * StatementTimeout,
	}
}

// Sample writes the three blocks every time. A read that fails is its own
// block's error=, with no rows, and the other two are still written.
func (CatalogMap) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	// One buffer, one Write: a write failing mid-block cannot leave half a sample.
	var sample bytes.Buffer

	for _, block := range catalogBlocks {
		rows, err := readCatalogBlock(ctx, q, block)

		fields := []headerField{
			{"db", s.Database},
			{"dbid", s.DBID},
			{"sample", strconv.Itoa(s.Index)},
		}

		if err != nil {
			fields = append(fields, headerField{"error", s.errorText(err)})
		}

		if err := writeBlockHeader(&sample, block.source, block.scope, fields, s.At); err != nil {
			return err
		}

		if err := writeRows(&sample, block.columns, rows); err != nil {
			return err
		}
	}

	_, err := w.Write(sample.Bytes())

	return err
}

// readCatalogBlock returns no rows at all on an error, even one partway through
// the result: a block is every row or none.
func readCatalogBlock(ctx context.Context, q RowQuerier, block catalogBlock) ([][]string, error) {
	stmtCtx, cancel := statementContext(ctx)
	defer cancel()

	rows, err := q.Query(stmtCtx, block.sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cells [][]string

	for rows.Next() {
		row, err := block.scan(rows)
		if err != nil {
			return nil, err
		}

		cells = append(cells, row)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return cells, nil
}

func scanCatalogClass(rows pgx.Rows) ([]string, error) {
	var (
		oid, namespace uint32
		name, kind     string
	)

	if err := rows.Scan(&oid, &name, &namespace, &kind); err != nil {
		return nil, err
	}

	return []string{oidText(oid), name, oidText(namespace), kind}, nil
}

// scanCatalogName reads an (oid, name) pair: pg_namespace's and pg_database's
// rows have the same shape.
func scanCatalogName(rows pgx.Rows) ([]string, error) {
	var (
		oid  uint32
		name string
	)

	if err := rows.Scan(&oid, &name); err != nil {
		return nil, err
	}

	return []string{oidText(oid), name}, nil
}

func oidText(oid uint32) string {
	return strconv.FormatUint(uint64(oid), 10)
}
