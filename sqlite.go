package main

/*
#cgo LDFLAGS: -lsqlite3
#include <sqlite3.h>
#include <stdlib.h>
#include <sys/mman.h>
static int bind_text(sqlite3_stmt *s, int i, const char *v) {
    return sqlite3_bind_text(s, i, v, -1, SQLITE_TRANSIENT);
}
static int enable_memory_accounting(void) {
    return sqlite3_config(SQLITE_CONFIG_MEMSTATUS, 1);
}
static int supports_images(void) {
#ifdef SQLITE_DESERIALIZE_FREEONCLOSE
    return 1;
#else
    return 0;
#endif
}
static int deserialize_image(sqlite3 *db, void *data, sqlite3_int64 size) {
#ifdef SQLITE_DESERIALIZE_FREEONCLOSE
    return sqlite3_deserialize(db, "main", data, size, size,
        SQLITE_DESERIALIZE_READONLY);
#else
    return SQLITE_NOTFOUND;
#endif
}
static void *allocate_image(size_t size) {
    void *p = mmap(NULL, size, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANON, -1, 0);
    return p == MAP_FAILED ? NULL : p;
}
static void free_image(void *p, size_t size) { munmap(p, size); }
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"
	"unsafe"
)

// One connection, one owner at a time. No Go pointer crosses the SQLite API.
type database struct {
	mu        sync.Mutex
	p         *C.sqlite3
	path      string
	image     unsafe.Pointer
	imageSize int64
	books     map[string]libraryBook
}

// Configure from main, before any connection opens. Avoid native library calls
// from package variable initializers on macOS.
var memoryAccountingEnabled bool

func enableSQLiteMemoryAccounting() error {
	if memoryAccountingEnabled {
		return nil
	}
	if C.enable_memory_accounting() != C.SQLITE_OK {
		return errors.New("SQLite memory accounting could not be enabled")
	}
	memoryAccountingEnabled = true
	return nil
}

// The hard limit covers SQLite allocations, including an in-memory database.
// Go allocations and filesystem caches require separate operating-system headroom.
func (d *database) sqliteHeapLimit(bytes int64) error {
	if !memoryAccountingEnabled {
		return errors.New("SQLite memory accounting could not be enabled")
	}
	rows, err := d.rows(fmt.Sprintf("PRAGMA hard_heap_limit=%d", bytes))
	if err != nil {
		return err
	}
	if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0] == "0" {
		return errors.New("this SQLite runtime does not support hard heap limits")
	}
	return nil
}

// SQLite's backup API copies a consistent snapshot. An unfinished backup rolls
// back its destination transaction and never reports a completed snapshot.
func (d *database) snapshot(ctx context.Context, path string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	target, err := openDatabase(path, false)
	if err != nil {
		return err
	}
	defer target.close()
	return copyDatabase(ctx, target, d)
}

func (d *database) load(ctx context.Context, source *database) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	source.mu.Lock()
	defer source.mu.Unlock()
	if C.supports_images() == 0 || source.path == ":memory:" {
		return copyDatabase(ctx, d, source)
	}
	// Hold a SQLite read transaction while reading its rollback-mode file.
	// Bulk reads avoid millions of small NFS requests from the backup reader.
	if err := source.exec("BEGIN"); err != nil {
		return err
	}
	defer source.exec("ROLLBACK")
	if _, err := source.rows("SELECT rowid FROM documents LIMIT 1"); err != nil {
		return err
	}
	file, err := os.Open(source.path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if !info.Mode().IsRegular() || size < 512 || size > 16*1024*1024*1024 {
		return errors.New("metadata image size is invalid")
	}
	limits, err := d.rows("PRAGMA hard_heap_limit")
	if err != nil || len(limits) != 1 {
		return errors.New("metadata image requires an enforceable memory ceiling")
	}
	ceiling, err := strconv.ParseInt(limits[0][0], 10, 64)
	if err != nil || ceiling-size < 32*1024*1024 {
		return errors.New("metadata image exceeds the configured memory ceiling")
	}
	// SQLite limits single allocations to less than two GiB. Reserve the image
	// separately and lower SQLite's global ceiling by that exact byte count.
	if err := d.sqliteHeapLimit(ceiling - size); err != nil {
		return err
	}
	buffer := C.allocate_image(C.size_t(size))
	if buffer == nil {
		return errors.New("metadata image exceeds the available SQLite heap")
	}
	owned := false
	defer func() {
		if !owned {
			C.free_image(buffer, C.size_t(size))
		}
	}()
	for offset := int64(0); offset < size; {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := min(int64(8*1024*1024), size-offset)
		chunk := unsafe.Slice((*byte)(unsafe.Add(buffer, offset)), int(n))
		if _, err := io.ReadFull(file, chunk); err != nil {
			return errors.New("metadata image read did not complete")
		}
		offset += n
	}
	if C.deserialize_image(d.p, buffer, C.sqlite3_int64(size)) != C.SQLITE_OK {
		return d.err()
	}
	owned = true
	d.image = buffer
	d.imageSize = size
	return nil
}

func copyDatabase(ctx context.Context, target, source *database) error {
	name := C.CString("main")
	defer C.free(unsafe.Pointer(name))
	b := C.sqlite3_backup_init(target.p, name, source.p, name)
	if b == nil {
		return target.err()
	}
	defer C.sqlite3_backup_finish(b)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rc := C.sqlite3_backup_step(b, 8192)
		if rc == C.SQLITE_DONE {
			return nil
		}
		if rc == C.SQLITE_BUSY || rc == C.SQLITE_LOCKED {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		} else if rc != C.SQLITE_OK {
			return target.err()
		}
	}
}

func openDatabase(path string, create bool) (*database, error) {
	if path == "" {
		return nil, errors.New("an explicit database path is required")
	}
	name := C.CString(path)
	defer C.free(unsafe.Pointer(name))
	d := &database{path: path}
	flags := C.int(C.SQLITE_OPEN_READWRITE | C.SQLITE_OPEN_FULLMUTEX)
	if create {
		flags |= C.SQLITE_OPEN_CREATE
	}
	if C.sqlite3_open_v2(name, &d.p, flags, nil) != C.SQLITE_OK {
		err := d.err()
		d.close()
		return nil, err
	}
	C.sqlite3_busy_timeout(d.p, 5000)
	// DELETE journaling avoids WAL shared memory on a mounted NAS. The operator
	// must still provide reliable filesystem locks, preferably NAS-local storage.
	if err := d.exec("PRAGMA foreign_keys=ON; PRAGMA journal_mode=DELETE; PRAGMA synchronous=FULL; PRAGMA temp_store=MEMORY; PRAGMA cache_size=-32768"); err != nil {
		d.close()
		return nil, err
	}
	return d, nil
}

func (d *database) close() {
	if d != nil && d.p != nil {
		C.sqlite3_close(d.p)
		d.p = nil
		if d.image != nil {
			C.free_image(d.image, C.size_t(d.imageSize))
			d.image = nil
		}
	}
}
func (d *database) err() error { return fmt.Errorf("sqlite: %s", C.GoString(C.sqlite3_errmsg(d.p))) }
func (d *database) exec(sql string) error {
	s := C.CString(sql)
	defer C.free(unsafe.Pointer(s))
	if C.sqlite3_exec(d.p, s, nil, nil, nil) != C.SQLITE_OK {
		return d.err()
	}
	return nil
}

func (d *database) prepare(sql string, args ...string) (*C.sqlite3_stmt, error) {
	s := C.CString(sql)
	defer C.free(unsafe.Pointer(s))
	var stmt *C.sqlite3_stmt
	if C.sqlite3_prepare_v2(d.p, s, -1, &stmt, nil) != C.SQLITE_OK {
		return nil, d.err()
	}
	for i, a := range args {
		v := C.CString(a)
		rc := C.bind_text(stmt, C.int(i+1), v)
		C.free(unsafe.Pointer(v))
		if rc != C.SQLITE_OK {
			C.sqlite3_finalize(stmt)
			return nil, d.err()
		}
	}
	return stmt, nil
}

func (d *database) run(sql string, args ...string) (int, error) {
	s, err := d.prepare(sql, args...)
	if err != nil {
		return 0, err
	}
	defer C.sqlite3_finalize(s)
	if C.sqlite3_step(s) != C.SQLITE_DONE {
		return 0, d.err()
	}
	return int(C.sqlite3_changes(d.p)), nil
}

func (d *database) rows(sql string, args ...string) ([][]string, error) {
	s, err := d.prepare(sql, args...)
	if err != nil {
		return nil, err
	}
	defer C.sqlite3_finalize(s)
	rows := make([][]string, 0)
	for {
		rc := C.sqlite3_step(s)
		if rc == C.SQLITE_DONE {
			return rows, nil
		}
		if rc != C.SQLITE_ROW {
			return nil, d.err()
		}
		row := make([]string, int(C.sqlite3_column_count(s)))
		for i := range row {
			row[i] = C.GoString((*C.char)(unsafe.Pointer(C.sqlite3_column_text(s, C.int(i)))))
		}
		rows = append(rows, row)
	}
}
