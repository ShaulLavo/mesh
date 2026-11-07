package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"modernc.org/sqlite"
)

const catalogWriterEventLimit = 256
const catalogWriterConnectionLimit = 16

type catalogWriterEvent struct {
	at          time.Time
	kind        string
	connection  int
	transaction int
	table       string
	operation   int32
	result      string
	sqliteCode  int
}

type catalogWriterConnection struct {
	busyTimeout string
	foreignKeys string
	journalMode string
	synchronous string
}

type catalogWriterObserver struct {
	mu                 sync.Mutex
	started            time.Time
	writer             string
	deadline           time.Time
	transaction        int
	events             [catalogWriterEventLimit]catalogWriterEvent
	count              int
	overflow           int
	connections        [catalogWriterConnectionLimit]catalogWriterConnection
	connectionCount    int
	connectionOverflow int
}

func newCatalogWriterObserver(readyPath string) *catalogWriterObserver {
	o := &catalogWriterObserver{started: time.Now(), writer: filepath.Base(readyPath)}
	// Registration is process-global, so only the private helper may install it.
	if os.Getenv(catalogCacheHelperEnv) == "" {
		panic("catalog writer observer requires a private helper")
	}
	fixture := filepath.Join(os.Getenv(catalogCacheStateEnv), catalogDatabaseName)
	sqlite.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, dsn string) error {
		return o.connect(conn, dsn, fixture)
	})
	return o
}

func (o *catalogWriterObserver) connect(conn sqlite.ExecQuerierContext, dsn, fixture string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("catalog writer observer: parse fixture DSN: %w", err)
	}
	if u.Scheme != "file" || u.Path != filepath.ToSlash(fixture) {
		return nil
	}
	hooks, ok := conn.(sqlite.HookRegisterer)
	if !ok {
		return fmt.Errorf("catalog writer observer: SQLite connection has no supported hooks")
	}
	o.mu.Lock()
	if o.connectionCount == len(o.connections) {
		o.connectionOverflow++
		o.mu.Unlock()
		return nil
	}
	id := o.connectionCount + 1
	query := u.Query()
	o.connections[o.connectionCount] = catalogWriterConnection{
		busyTimeout: query.Get("_busy_timeout"), foreignKeys: query.Get("_foreign_keys"),
		journalMode: query.Get("_journal_mode"), synchronous: query.Get("_synchronous"),
	}
	o.connectionCount++
	o.mu.Unlock()
	o.record(catalogWriterEvent{kind: "connection-ready", connection: id})
	hooks.RegisterPreUpdateHook(func(data sqlite.SQLitePreUpdateData) {
		o.record(catalogWriterEvent{kind: "pre-update", connection: id, table: catalogWriterTable(data), operation: data.Op})
	})
	hooks.RegisterCommitHook(func() int32 {
		o.record(catalogWriterEvent{kind: "commit-hook-before-commit", connection: id})
		return 0
	})
	hooks.RegisterRollbackHook(func() {
		o.record(catalogWriterEvent{kind: "rollback-hook", connection: id})
	})
	return nil
}

func catalogWriterTable(data sqlite.SQLitePreUpdateData) string {
	if data.DatabaseName != "main" {
		return "other-database"
	}
	switch data.TableName {
	case "hosts", "sessions", "cached_session_facts":
		return data.TableName
	default:
		return "other-table"
	}
}

// Callbacks only timestamp and copy fixed-size records. No SQL, formatting or I/O
// runs here. Transaction 1 names Save's sole attempted ReconcileHost transaction;
// its BEGIN and lock acquisition time are not observed by these hooks.
func (o *catalogWriterObserver) record(event catalogWriterEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.count == len(o.events) {
		o.overflow++
		return
	}
	event.at = time.Now()
	event.transaction = o.transaction
	o.events[o.count] = event
	o.count++
}

func (o *catalogWriterObserver) beginSave(ctx context.Context) {
	o.mu.Lock()
	o.transaction = 1
	o.deadline, _ = ctx.Deadline()
	o.mu.Unlock()
	o.result("save-entry", ctx.Err())
}

func (o *catalogWriterObserver) result(kind string, err error) {
	event := catalogWriterEvent{kind: kind, result: "ok"}
	if err != nil {
		event.result = "error"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		event.result = "deadline-exceeded"
	}
	if errors.Is(err, context.Canceled) {
		event.result = "canceled"
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		event.sqliteCode = sqliteErr.Code()
	}
	o.record(event)
}

func (o *catalogWriterObserver) close(cache *SQLiteCatalogCache) {
	o.record(catalogWriterEvent{kind: "close-entry"})
	o.result("close-return", cache.Close())
}

// Flush after Save and Close return, including when the helper fails. A successful
// Save bounds release above. Close-return alone does not prove that a canceled
// transaction's asynchronous rollback has drained; this is a snapshot, not a wait.
func (o *catalogWriterObserver) flush() {
	o.mu.Lock()
	events, count, overflow := o.events, o.count, o.overflow
	connections, connectionCount, connectionOverflow := o.connections, o.connectionCount, o.connectionOverflow
	o.mu.Unlock()
	fmt.Fprintf(os.Stderr, "CATALOG-WRITER pid=%d writer=%s origin_utc=%s deadline_utc=%s events=%d capacity=%d overflow=%d connections=%d connection_overflow=%d pool_max_open=4 pool_fact=store.go-configuration runtime_pool_stats=unobserved coverage=through-close-snapshot driver_drain=unobserved\n",
		os.Getpid(), o.writer, o.started.UTC().Format(time.RFC3339Nano), o.deadline.UTC().Format(time.RFC3339Nano),
		count, len(events), overflow, connectionCount, connectionOverflow)
	for index, conn := range connections[:connectionCount] {
		fmt.Fprintf(os.Stderr, "CATALOG-WRITER pid=%d writer=%s connection=%d dsn_busy_timeout=%s dsn_foreign_keys=%s dsn_journal_mode=%s dsn_synchronous=%s\n",
			os.Getpid(), o.writer, index+1, conn.busyTimeout, conn.foreignKeys, conn.journalMode, conn.synchronous)
	}
	for index, event := range events[:count] {
		fmt.Fprintf(os.Stderr, "CATALOG-WRITER pid=%d writer=%s sequence=%d elapsed_ns=%d utc=%s connection=%d transaction=%d event=%s table=%s op=%d result=%s sqlite_code=%d\n",
			os.Getpid(), o.writer, index, event.at.Sub(o.started).Nanoseconds(), event.at.UTC().Format(time.RFC3339Nano),
			event.connection, event.transaction, event.kind, event.table, event.operation, event.result, event.sqliteCode)
	}
}
