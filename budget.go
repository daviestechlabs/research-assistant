package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
)

// One usage ledger per catalog directory survives new metadata snapshot files.
// Keeping writes out of the catalog also preserves its NFS read cache.
func openBudget(catalog *database) (*database, error) {
	root := filepath.Dir(catalog.path)
	if original := os.Getenv("RESEARCH_INDEX_ORIGINAL_CATALOG"); original != "" && os.Getenv("RESEARCH_INDEX_ROOT") != "" {
		root = filepath.Dir(original)
	}
	b, err := openDatabase(filepath.Join(root, "research-usage.sqlite"), true)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			b.close()
		}
	}()
	if err = b.exec("BEGIN IMMEDIATE"); err != nil {
		return nil, err
	}
	defer b.exec("ROLLBACK")
	version, err := b.rows("PRAGMA user_version")
	if err != nil {
		return nil, err
	}
	if version[0][0] != "0" && version[0][0] != "1" {
		return nil, errors.New("unsupported model budget schema")
	}
	if err = b.exec(`CREATE TABLE IF NOT EXISTS usage(day TEXT NOT NULL,kind TEXT NOT NULL,calls INTEGER NOT NULL,units INTEGER NOT NULL,PRIMARY KEY(day,kind));`); err != nil {
		return nil, err
	}
	if version[0][0] == "0" {
		catalog.mu.Lock()
		legacy, e := catalog.rows("SELECT day,kind,calls,units FROM usage")
		catalog.mu.Unlock()
		if e != nil {
			return nil, e
		}
		for _, row := range legacy {
			for _, value := range row[2:] {
				n, e := strconv.ParseInt(value, 10, 64)
				if e != nil || n < 0 {
					return nil, errors.New("invalid legacy model usage")
				}
			}
			if _, err = b.run("INSERT INTO usage(day,kind,calls,units) VALUES(?,?,?,?)", row...); err != nil {
				return nil, err
			}
		}
		if err = b.exec("PRAGMA user_version=1"); err != nil {
			return nil, err
		}
	}
	if err = b.exec("COMMIT"); err != nil {
		return nil, err
	}
	ok = true
	return b, nil
}

func (i *inference) close() {
	if i.budget != nil && i.budget != i.db {
		i.budget.close()
	}
}

func (i *inference) stats() (any, error) {
	v, err := i.db.stats()
	if err != nil {
		return nil, err
	}
	if i.budget == i.db {
		return v, nil
	}
	i.budget.mu.Lock()
	rows, err := i.budget.rows("SELECT day,kind,calls,units FROM usage WHERE day=date('now')")
	i.budget.mu.Unlock()
	if err != nil {
		return nil, err
	}
	calls, units := 0, 0
	for _, row := range rows {
		n, _ := strconv.Atoi(row[2])
		calls += n
		n, _ = strconv.Atoi(row[3])
		units += n
	}
	catalog := v.(map[string]any)
	catalog["usage_today"] = rows
	catalog["model_calls_today"] = calls
	catalog["reserved_units_today"] = units
	return v, nil
}
