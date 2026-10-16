package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgreSQL owns catalog metadata. Small local session and usage stores retain
// their existing SQLite contracts. Every catalog query names its immutable generation.
type postgresCatalog struct {
	pool     *pgxpool.Pool
	schema   string
	id       string
	baseline string
	tx       pgx.Tx
	ticks    atomic.Uint64
	scanRows atomic.Uint64
}

func postgresEnabled() bool { return os.Getenv("RESEARCH_POSTGRES_HOST") != "" }
func postgresGenerationSchema(id string) (string, error) {
	if id != "original" && !deskID.MatchString(id) {
		return "", errors.New("invalid PostgreSQL catalog generation")
	}
	return "research_g_" + id, nil
}
func postgresPool(ctx context.Context) (*pgxpool.Pool, error) {
	host := os.Getenv("RESEARCH_POSTGRES_HOST")
	db := os.Getenv("RESEARCH_POSTGRES_DATABASE")
	if host == "" || strings.ContainsAny(host, "/:?#@ ") || !regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`).MatchString(db) {
		return nil, errors.New("PostgreSQL needs a fixed host and database")
	}
	user, e := os.ReadFile(os.Getenv("RESEARCH_POSTGRES_USERNAME_FILE"))
	if e != nil {
		return nil, errors.New("PostgreSQL username is unavailable")
	}
	password, e := os.ReadFile(os.Getenv("RESEARCH_POSTGRES_PASSWORD_FILE"))
	if e != nil {
		return nil, errors.New("PostgreSQL credential is unavailable")
	}
	u := url.URL{Scheme: "postgres", User: url.User(strings.TrimSpace(string(user))), Host: net.JoinHostPort(host, "5432"), Path: "/" + db}
	q := u.Query()
	q.Set("sslmode", "verify-full")
	q.Set("sslrootcert", os.Getenv("RESEARCH_POSTGRES_CA_FILE"))
	u.RawQuery = q.Encode()
	c, e := pgxpool.ParseConfig(u.String())
	if e != nil {
		return nil, errors.New("PostgreSQL TLS configuration is invalid")
	}
	c.ConnConfig.Password = strings.TrimSpace(string(password))
	c.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	c.ConnConfig.RuntimeParams["application_name"] = "research-assistant"
	c.ConnConfig.ConnectTimeout = 10 * time.Second
	c.MaxConns = 4
	c.MinConns = 0
	c.MaxConnLifetime = 30 * time.Minute
	c.MaxConnIdleTime = 5 * time.Minute
	if c.ConnConfig.TLSConfig == nil || c.ConnConfig.TLSConfig.InsecureSkipVerify {
		return nil, errors.New("PostgreSQL requires verified TLS")
	}
	c.ConnConfig.TLSConfig.MinVersion = tls.VersionTLS12
	pool, e := pgxpool.NewWithConfig(ctx, c)
	if e != nil {
		return nil, errors.New("PostgreSQL pool is unavailable")
	}
	if e = pool.Ping(ctx); e != nil {
		pool.Close()
		return nil, errors.New("PostgreSQL connection did not pass TLS authentication")
	}
	return pool, nil
}
func postgresPathID(path string) (string, bool) {
	root := os.Getenv("RESEARCH_INDEX_ROOT")
	if root == "" {
		return "", false
	}
	var a indexActive
	if readIndexJSON(filepath.Join(root, "active.json"), &a) == nil {
		if path == a.Current.Catalog {
			return a.Current.ID, true
		}
		if a.Previous != nil && path == a.Previous.Catalog {
			return a.Previous.ID, true
		}
	}
	dir := filepath.Dir(path)
	id := filepath.Base(dir)
	if filepath.Base(path) == "catalog.sqlite" && deskID.MatchString(id) && dir == filepath.Join(root, "runs", id) {
		return id, true
	}
	return "", false
}
func openPostgresCatalog(path, id string) (*database, error) {
	return openPostgresCatalogMode(path, id, os.Getenv("RESEARCH_POSTGRES_WRITER") == "true")
}
func openPostgresCatalogMode(path, id string, mutable bool) (*database, error) {
	schema, e := postgresGenerationSchema(id)
	if e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, e := postgresPool(ctx)
	if e != nil {
		return nil, e
	}
	var exists bool
	e = pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=$1 AND table_name='documents')", schema).Scan(&exists)
	if e != nil || !exists {
		pool.Close()
		return nil, errors.New("PostgreSQL catalog has not been migrated")
	}
	var status string
	if pool.QueryRow(ctx, "SELECT status FROM research_catalog.generations WHERE id=$1", id).Scan(&status) != nil || (!mutable && status != "ready") {
		pool.Close()
		return nil, errors.New("PostgreSQL generation is not validated for reading")
	}
	// The SQLite handle holds only the small usage table for compatibility with stats.
	d, e := openSQLiteDatabase(":memory:", true)
	if e != nil {
		pool.Close()
		return nil, e
	}
	if e = d.exec("CREATE TABLE usage(day TEXT,kind TEXT,calls INTEGER,units INTEGER,PRIMARY KEY(day,kind)); PRAGMA user_version=2"); e != nil {
		d.close()
		pool.Close()
		return nil, e
	}
	d.path = path
	d.pg = &postgresCatalog{pool: pool, schema: schema, id: id}
	return d, nil
}
func (p *postgresCatalog) table(name string) string { return pgx.Identifier{p.schema, name}.Sanitize() }

// Translate only internal SQL tokens. Quoted strings remain untouched, and all
// request values stay bound parameters. FTS uses a separate native query builder.
func (p *postgresCatalog) sql(input string) (string, error) {
	if strings.Contains(input, "baseline.documents") {
		if p.baseline == "" {
			return "", errors.New("PostgreSQL baseline is not attached")
		}
		input = strings.ReplaceAll(input, "baseline.documents", pgx.Identifier{p.baseline, "documents"}.Sanitize())
	}
	input = strings.ReplaceAll(input, "sum(summary_kind='missing')", "sum((summary_kind='missing')::int)")
	input = strings.ReplaceAll(input, "rowid>?", "rowid>CAST(? AS BIGINT)")
	input = strings.ReplaceAll(input, "rowid=?", "rowid=CAST(? AS BIGINT)")
	input = strings.ReplaceAll(input, "LIMIT ?", "LIMIT CAST(? AS BIGINT)")
	var out strings.Builder
	param := 0
	tableExpected := false
	for i := 0; i < len(input); {
		c := input[i]
		if c == '\'' || c == '"' {
			quote := c
			start := i
			i++
			for i < len(input) {
				if input[i] == quote {
					i++
					if i < len(input) && input[i] == quote {
						i++
						continue
					}
					break
				}
				i++
			}
			out.WriteString(input[start:i])
			continue
		}
		if c == '?' {
			param++
			out.WriteString("$" + strconv.Itoa(param))
			i++
			continue
		}
		if c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			start := i
			for i < len(input) && (input[i] == '_' || input[i] >= 'a' && input[i] <= 'z' || input[i] >= 'A' && input[i] <= 'Z' || input[i] >= '0' && input[i] <= '9') {
				i++
			}
			token := input[start:i]
			upper := strings.ToUpper(token)
			switch upper {
			case "FROM", "JOIN", "UPDATE", "INTO", "TABLE", "TRUNCATE":
				tableExpected = true
			case "IF", "NOT", "EXISTS":
			default:
			}
			switch token {
			case "documents", "catalog_stats", "indexing_checkpoint":
				if tableExpected && (start == 0 || input[start-1] != '.') {
					out.WriteString(p.table(token))
				} else {
					out.WriteString(token)
				}
			default:
				out.WriteString(token)
			}
			if upper != "FROM" && upper != "JOIN" && upper != "UPDATE" && upper != "INTO" && upper != "TABLE" && upper != "TRUNCATE" && upper != "IF" && upper != "NOT" && upper != "EXISTS" {
				tableExpected = false
			}
			continue
		}
		out.WriteByte(c)
		i++
	}
	return out.String(), nil
}
func (p *postgresCatalog) rows(sql string, args ...string) ([][]string, error) {
	query, e := p.sql(sql)
	if e != nil {
		return nil, e
	}
	values := make([]any, len(args))
	for i := range args {
		values[i] = args[i]
	}
	timeout := 15 * time.Second
	if os.Getenv("RESEARCH_POSTGRES_WRITER") == "true" {
		timeout = 20 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var rows pgx.Rows
	if p.tx != nil {
		rows, e = p.tx.Query(ctx, query, values...)
	} else {
		rows, e = p.pool.Query(ctx, query, values...)
	}
	if e != nil {
		return nil, errors.New("PostgreSQL catalog query failed")
	}
	defer rows.Close()
	result := [][]string{}
	for rows.Next() {
		raw, e := rows.Values()
		if e != nil {
			return nil, errors.New("PostgreSQL catalog row failed")
		}
		r := make([]string, len(raw))
		for j, v := range raw {
			if v != nil {
				r[j] = fmt.Sprint(v)
			}
		}
		result = append(result, r)
	}
	if rows.Err() != nil {
		return nil, errors.New("PostgreSQL catalog scan failed")
	}
	p.ticks.Add(1)
	return result, nil
}
func (p *postgresCatalog) exec(sql string, args ...string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	switch strings.TrimSpace(sql) {
	case "BEGIN", "BEGIN IMMEDIATE":
		if p.tx != nil {
			return 0, errors.New("PostgreSQL transaction already open")
		}
		tx, e := p.pool.Begin(ctx)
		if e == nil {
			p.tx = tx
		}
		return 0, e
	case "COMMIT":
		if p.tx == nil {
			return 0, errors.New("PostgreSQL transaction is not open")
		}
		tx := p.tx
		p.tx = nil
		return 0, tx.Commit(ctx)
	case "ROLLBACK":
		if p.tx == nil {
			return 0, nil
		}
		tx := p.tx
		p.tx = nil
		return 0, tx.Rollback(ctx)
	case "DETACH DATABASE baseline":
		p.baseline = ""
		return 0, nil
	}
	if strings.HasPrefix(sql, "DETACH DATABASE baseline;") {
		p.baseline = ""
		sql = strings.TrimSpace(strings.TrimPrefix(sql, "DETACH DATABASE baseline;"))
	}
	if sql == "ATTACH DATABASE ? AS baseline" {
		if len(args) != 1 {
			return 0, errors.New("invalid PostgreSQL baseline")
		}
		id, ok := postgresPathID(args[0])
		if !ok {
			return 0, errors.New("unrecognized baseline generation")
		}
		schema, e := postgresGenerationSchema(id)
		p.baseline = schema
		return 0, e
	}
	if strings.HasPrefix(sql, "INSERT OR REPLACE INTO indexing_checkpoint") {
		sql = "INSERT INTO indexing_checkpoint(id,state) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET state=excluded.state"
	}
	query, e := p.sql(sql)
	if e != nil {
		return 0, e
	}
	v := make([]any, len(args))
	for i := range args {
		v[i] = args[i]
	}
	var n int64
	if p.tx != nil {
		tag, err := p.tx.Exec(ctx, query, v...)
		e = err
		n = tag.RowsAffected()
	} else {
		tag, err := p.pool.Exec(ctx, query, v...)
		e = err
		n = tag.RowsAffected()
	}
	if e != nil {
		return 0, errors.New("PostgreSQL catalog statement failed")
	}
	p.ticks.Add(1)
	return int(n), nil
}
func postgresCatalogSQL(sql string) bool {
	for _, name := range []string{"documents", "catalog_stats", "indexing_checkpoint"} {
		if regexp.MustCompile(`\b` + name + `\b`).MatchString(sql) {
			return true
		}
	}
	return false
}
func (p *postgresCatalog) digest(ctx context.Context) (string, error) {
	p.scanRows.Store(0)
	rows, e := p.pool.Query(ctx, "SELECT "+strings.Join(postgresCopyColumns, ",")+" FROM "+p.table("documents")+" ORDER BY rowid")
	if e != nil {
		return "", errors.New("PostgreSQL content scan failed")
	}
	defer rows.Close()
	h := sha256.New()
	encoder := json.NewEncoder(h)
	for rows.Next() {
		p.scanRows.Add(1)
		values, e := rows.Values()
		if e != nil {
			return "", errors.New("PostgreSQL content row failed")
		}
		row := make([]string, len(values))
		for i, v := range values {
			row[i] = fmt.Sprint(v)
		}
		if e = encoder.Encode(row); e != nil {
			return "", e
		}
	}
	if rows.Err() != nil {
		return "", errors.New("PostgreSQL content scan did not finish")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
