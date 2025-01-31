// SPDX-FileCopyrightText: 2014-2022 SAP SE
//
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"bufio"
	"context"
	"crypto/tls"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SAP/go-hdb/driver/dial"
	p "github.com/SAP/go-hdb/driver/internal/protocol"
	"github.com/SAP/go-hdb/driver/internal/protocol/scanner"
	"github.com/SAP/go-hdb/driver/sqltrace"
	"github.com/SAP/go-hdb/driver/unicode/cesu8"
	"golang.org/x/text/transform"
)

// Transaction isolation levels supported by hdb.
const (
	LevelReadCommitted  = "READ COMMITTED"
	LevelRepeatableRead = "REPEATABLE READ"
	LevelSerializable   = "SERIALIZABLE"
)

// Access modes supported by hdb.
const (
	modeReadOnly  = "READ ONLY"
	modeReadWrite = "READ WRITE"
)

// map sql isolation level to hdb isolation level.
var isolationLevel = map[driver.IsolationLevel]string{
	driver.IsolationLevel(sql.LevelDefault):        LevelReadCommitted,
	driver.IsolationLevel(sql.LevelReadCommitted):  LevelReadCommitted,
	driver.IsolationLevel(sql.LevelRepeatableRead): LevelRepeatableRead,
	driver.IsolationLevel(sql.LevelSerializable):   LevelSerializable,
}

// map sql read only flag to hdb access mode.
var readOnly = map[bool]string{
	true:  modeReadOnly,
	false: modeReadWrite,
}

// ErrUnsupportedIsolationLevel is the error raised if a transaction is started with a not supported isolation level.
var ErrUnsupportedIsolationLevel = errors.New("unsupported isolation level")

// ErrNestedTransaction is the error raised if a transaction is created within a transaction as this is not supported by hdb.
var ErrNestedTransaction = errors.New("nested transactions are not supported")

// ErrNestedQuery is the error raised if a sql statement is executed before an "active" statement is closed.
// Example: execute sql statement before rows of previous select statement are closed.
var ErrNestedQuery = errors.New("nested sql queries are not supported")

// queries
const (
	dummyQuery        = "select 1 from dummy"
	setIsolationLevel = "set transaction isolation level"
	setAccessMode     = "set transaction"
	setDefaultSchema  = "set schema"
)

// bulk statement
const (
	bulk = "b$"
)

var (
	flushTok   = new(struct{})
	noFlushTok = new(struct{})
)

var (
	// NoFlush is to be used as parameter in bulk statements to delay execution.
	NoFlush = sql.Named(bulk, &noFlushTok)
	// Flush can be used as optional parameter in bulk statements but is not required to trigger execution.
	Flush = sql.Named(bulk, &flushTok)
)

const (
	maxNumTraceArg = 20
)

var (
	// register as var to execute even before init() funcs are called
	_ = p.RegisterScanType(p.DtDecimal, reflect.TypeOf((*Decimal)(nil)).Elem())
	_ = p.RegisterScanType(p.DtLob, reflect.TypeOf((*Lob)(nil)).Elem())
)

// dbConn wraps the database tcp connection. It sets timeouts and handles driver ErrBadConn behavior.
type dbConn struct {
	// atomic access - alignment
	cancelled int32
	metrics   *metrics
	conn      net.Conn
	timeout   time.Duration
	lastError error // error bad connection
	closed    bool
}

func (c *dbConn) deadline() (deadline time.Time) {
	if c.timeout == 0 {
		return
	}
	return time.Now().Add(c.timeout)
}

var (
	errCancelled = errors.New("db connection is canceled")
	errClosed    = errors.New("db connection is closed")
)

func (c *dbConn) cancel() {
	atomic.StoreInt32(&c.cancelled, 1)
	c.lastError = errCancelled
}

func (c *dbConn) close() error {
	c.closed = true
	c.lastError = errClosed
	return c.conn.Close()
}

// Read implements the io.Reader interface.
func (c *dbConn) Read(b []byte) (n int, err error) {
	// check if killed
	if atomic.LoadInt32(&c.cancelled) == 1 {
		return 0, driver.ErrBadConn
	}
	var start time.Time
	//set timeout
	if err = c.conn.SetReadDeadline(c.deadline()); err != nil {
		goto retError
	}
	start = time.Now()
	n, err = c.conn.Read(b)
	c.metrics.addTimeValue(timeRead, time.Since(start).Nanoseconds())
	c.metrics.addCounterValue(counterBytesRead, uint64(n))
	if err == nil {
		return
	}
retError:
	dlog.Printf("Connection read error local address %s remote address %s: %s", c.conn.LocalAddr(), c.conn.RemoteAddr(), err)
	c.lastError = err
	return n, driver.ErrBadConn
}

// Write implements the io.Writer interface.
func (c *dbConn) Write(b []byte) (n int, err error) {
	// check if killed
	if atomic.LoadInt32(&c.cancelled) == 1 {
		return 0, driver.ErrBadConn
	}
	var start time.Time
	//set timeout
	if err = c.conn.SetWriteDeadline(c.deadline()); err != nil {
		goto retError
	}
	start = time.Now()
	n, err = c.conn.Write(b)
	c.metrics.addTimeValue(timeWrite, time.Since(start).Nanoseconds())
	c.metrics.addCounterValue(counterBytesWritten, uint64(n))
	if err == nil {
		return
	}
retError:
	dlog.Printf("Connection write error local address %s remote address %s: %s", c.conn.LocalAddr(), c.conn.RemoteAddr(), err)
	c.lastError = err
	return n, driver.ErrBadConn
}

const (
	lrNestedQuery = 1
)

type connLock struct {
	// 64 bit alignment
	lockReason int64 // atomic access

	mu     sync.Mutex // tryLock mutex
	connMu sync.Mutex // connection mutex
}

func (l *connLock) tryLock(lockReason int64) error {
	l.mu.Lock()
	if atomic.LoadInt64(&l.lockReason) == lrNestedQuery {
		l.mu.Unlock()
		return ErrNestedQuery
	}
	l.connMu.Lock()
	atomic.StoreInt64(&l.lockReason, lockReason)
	l.mu.Unlock()
	return nil
}

func (l *connLock) lock() { l.connMu.Lock() }

func (l *connLock) unlock() {
	atomic.StoreInt64(&l.lockReason, 0)
	l.connMu.Unlock()
}

// check if conn implements all required interfaces
var (
	_ driver.Conn               = (*conn)(nil)
	_ driver.ConnPrepareContext = (*conn)(nil)
	_ driver.Pinger             = (*conn)(nil)
	_ driver.ConnBeginTx        = (*conn)(nil)
	_ driver.ExecerContext      = (*conn)(nil)
	_ driver.QueryerContext     = (*conn)(nil)
	_ driver.NamedValueChecker  = (*conn)(nil)
	_ driver.SessionResetter    = (*conn)(nil)
	_ driver.Validator          = (*conn)(nil)
	_ Conn                      = (*conn)(nil) // go-hdb enhancements
)

// connHook is a hook for testing.
var connHook func(c *conn, op int)

// connection hook operations
const (
	choNone = iota
	choStmtExec
)

// Conn enhances a connection with go-hdb specific connection functions.
type Conn interface {
	HDBVersion() *Version
	DatabaseName() string
	DBConnectInfo(ctx context.Context, databaseName string) (*DBConnectInfo, error)
}

// Conn is the implementation of the database/sql/driver Conn interface.
type conn struct {
	metrics *metrics
	// Holding connection lock in QueryResultSet (see rows.onClose)
	/*
		As long as a session is in query mode no other sql statement must be executed.
		Example:
		- pinger is active
		- select with blob fields is executed
		- scan is hitting the database again (blob streaming)
		- if in between a ping gets executed (ping selects db) hdb raises error
		  "SQL Error 1033 - error while parsing protocol: invalid lob locator id (piecewise lob reading)"
	*/
	connLock

	dbConn  *dbConn
	scanner *scanner.Scanner
	closed  chan struct{}

	inTx bool // in transaction

	lastError error // last error

	trace bool // call sqlTrace.On() only once

	//Attrs *connAttrs // as a dedicated instance (clone) is used for every session we can access the attributes directly.

	sessionID int64

	// after go.17 support: delete serverOptions and define it again direcly here
	serverOptions connectOptions
	hdbVersion    *Version

	pr *p.Reader
	pw *p.Writer

	bulkSize     int
	lobChunkSize int
	fetchSize    int
	legacy       bool
	cesu8Decoder func() transform.Transformer
	cesu8Encoder func() transform.Transformer
}

func newConn(ctx context.Context, metrics *metrics, attrs *connAttrs, auth *p.Auth) (driver.Conn, error) {
	// lock attributes
	attrs.mu.RLock()
	defer attrs.mu.RUnlock()

	netConn, err := attrs._dialer.DialContext(ctx, attrs._host, dial.DialerOptions{Timeout: attrs._timeout, TCPKeepAlive: attrs._tcpKeepAlive})
	if err != nil {
		return nil, err
	}

	// is TLS connection requested?
	if attrs._tlsConfig != nil {
		netConn = tls.Client(netConn, attrs._tlsConfig.Clone())
	}

	dbConn := &dbConn{metrics: metrics, conn: netConn, timeout: attrs._timeout}
	// buffer connection
	rw := bufio.NewReadWriter(bufio.NewReaderSize(dbConn, attrs._bufferSize), bufio.NewWriterSize(dbConn, attrs._bufferSize))

	c := &conn{
		metrics:      metrics,
		dbConn:       dbConn,
		scanner:      &scanner.Scanner{},
		closed:       make(chan struct{}),
		trace:        sqltrace.On(),
		bulkSize:     attrs._bulkSize,
		lobChunkSize: attrs._lobChunkSize,
		fetchSize:    attrs._fetchSize,
		legacy:       attrs._legacy,
		cesu8Decoder: attrs._cesu8Decoder,
		cesu8Encoder: attrs._cesu8Encoder,
	}
	//c.Attrs = connAttrs // TODO rework

	c.pw = p.NewWriter(rw.Writer, attrs._cesu8Encoder, cloneStringStringMap(attrs._sessionVariables)) // write upstream
	if err := c.pw.WriteProlog(); err != nil {
		return nil, err
	}

	c.pr = p.NewReader(false, rw.Reader, attrs._cesu8Decoder) // read downstream
	if err := c.pr.ReadProlog(); err != nil {
		return nil, err
	}

	c.sessionID = defaultSessionID

	if c.sessionID, c.serverOptions, err = c._authenticate(auth, attrs._applicationName, attrs._dfv, attrs._locale); err != nil {
		return nil, err
	}

	if c.sessionID <= 0 {
		return nil, fmt.Errorf("invalid session id %d", c.sessionID)
	}

	c.hdbVersion = parseVersion(c.serverOptions[p.CoFullVersionString].(string))

	if attrs._defaultSchema != "" {
		if _, err := c.ExecContext(ctx, strings.Join([]string{setDefaultSchema, Identifier(attrs._defaultSchema).String()}, " "), nil); err != nil {
			return nil, err
		}
	}

	if attrs._pingInterval != 0 {
		go c.pinger(attrs._pingInterval, c.closed)
	}

	c.metrics.addGaugeValue(gaugeConn, 1) // increment open connections.

	return c, nil
}

func (c *conn) isBad() bool {
	switch {

	case c.dbConn.lastError != nil:
		return true

	case c.lastError != nil:
		// if last error was not a hdb error the connection is most probably not useable any more, e.g.
		// interrupted read / write on connection.
		if _, ok := c.lastError.(Error); !ok {
			return true
		}
	}
	return false
}

func (c *conn) pinger(d time.Duration, done <-chan struct{}) {
	ticker := time.NewTicker(d)
	defer ticker.Stop()

	ctx := context.Background()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			c.Ping(ctx)
		}
	}
}

// Ping implements the driver.Pinger interface.
func (c *conn) Ping(ctx context.Context) (err error) {
	if err := c.tryLock(0); err != nil {
		return err
	}
	defer c.unlock()

	if c.isBad() {
		return driver.ErrBadConn
	}

	if c.trace {
		defer traceSQL(time.Now(), dummyQuery, nil)
	}

	done := make(chan struct{})
	go func() {
		_, err = c._queryDirect(dummyQuery, !c.inTx)
		close(done)
	}()

	select {
	case <-ctx.Done():
		c.dbConn.cancel()
		return ctx.Err()
	case <-done:
		c.lastError = err
		return err
	}
}

// ResetSession implements the driver.SessionResetter interface.
func (c *conn) ResetSession(ctx context.Context) error {
	c.lock()
	defer c.unlock()

	stdQueryResultCache.cleanup(c)

	if c.isBad() {
		return driver.ErrBadConn
	}
	return nil
}

// IsValid implements the driver.Validator interface.
func (c *conn) IsValid() bool {
	c.lock()
	defer c.unlock()

	return !c.isBad()
}

// PrepareContext implements the driver.ConnPrepareContext interface.
func (c *conn) PrepareContext(ctx context.Context, query string) (stmt driver.Stmt, err error) {
	if err := c.tryLock(0); err != nil {
		return nil, err
	}
	defer c.unlock()

	if c.isBad() {
		return nil, driver.ErrBadConn
	}

	if c.trace {
		defer traceSQL(time.Now(), query, nil)
	}

	done := make(chan struct{})
	func() {
		var (
			qd *queryDescr
			pr *prepareResult
		)

		if qd, err = newQueryDescr(query, c.scanner); err != nil {
			goto done
		}

		if pr, err = c._prepare(qd.query); err != nil {
			goto done
		}
		if err = pr.check(qd); err != nil {
			goto done
		}

		if pr.isProcedureCall() {
			stmt = newCallStmt(c, qd.query, pr)
		} else {
			stmt = newStmt(c, qd.query, qd.isBulk, c.bulkSize, pr) //take latest connector bulk size
		}

	done:
		close(done)
	}()

	select {
	case <-ctx.Done():
		c.dbConn.cancel()
		return nil, ctx.Err()
	case <-done:
		c.metrics.addGaugeValue(gaugeStmt, 1) // increment number of statements.
		c.lastError = err
		return stmt, err
	}
}

// Close implements the driver.Conn interface.
func (c *conn) Close() error {
	c.lock()
	defer c.unlock()

	c.metrics.addGaugeValue(gaugeConn, -1) // decrement open connections.
	close(c.closed)                        // signal connection close

	// cleanup query cache
	stdQueryResultCache.cleanup(c)

	// if isBad do not disconnect
	if !c.isBad() {
		c._disconnect() // ignore error
	}
	return c.dbConn.close()
}

// BeginTx implements the driver.ConnBeginTx interface.
func (c *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (tx driver.Tx, err error) {
	if err := c.tryLock(0); err != nil {
		return nil, err
	}
	defer c.unlock()

	if c.isBad() {
		return nil, driver.ErrBadConn
	}

	if c.inTx {
		return nil, ErrNestedTransaction
	}

	level, ok := isolationLevel[opts.Isolation]
	if !ok {
		return nil, ErrUnsupportedIsolationLevel
	}

	done := make(chan struct{})
	go func() {
		// set isolation level
		query := strings.Join([]string{setIsolationLevel, level}, " ")
		if _, err = c._execDirect(query, !c.inTx); err != nil {
			goto done
		}
		// set access mode
		query = strings.Join([]string{setAccessMode, readOnly[opts.ReadOnly]}, " ")
		if _, err = c._execDirect(query, !c.inTx); err != nil {
			goto done
		}
		c.inTx = true
		tx = newTx(c)
	done:
		close(done)
	}()

	select {
	case <-ctx.Done():
		c.dbConn.cancel()
		return nil, ctx.Err()
	case <-done:
		c.metrics.addGaugeValue(gaugeTx, 1) // increment number of transactions.
		c.lastError = err
		return tx, err
	}
}

// QueryContext implements the driver.QueryerContext interface.
func (c *conn) QueryContext(ctx context.Context, query string, nvargs []driver.NamedValue) (rows driver.Rows, err error) {
	if err := c.tryLock(lrNestedQuery); err != nil {
		return nil, err
	}
	hasRowsCloser := false
	defer func() {
		// unlock connection if rows will not do it
		if !hasRowsCloser {
			c.unlock()
		}
	}()

	if c.isBad() {
		return nil, driver.ErrBadConn
	}

	if len(nvargs) != 0 {
		return nil, driver.ErrSkip //fast path not possible (prepare needed)
	}

	qd, err := newQueryDescr(query, c.scanner)
	if err != nil {
		return nil, err
	}
	switch qd.kind {
	case qkCall:
		// direct execution of call procedure
		// - returns no parameter metadata (sps 82) but only field values
		// --> let's take the 'prepare way' for stored procedures
		return nil, driver.ErrSkip
	case qkID:
		// query call table result
		rows, ok := stdQueryResultCache.Get(qd.id)
		if !ok {
			return nil, fmt.Errorf("invalid result set id %s", query)
		}
		if onCloser, ok := rows.(onCloser); ok {
			onCloser.setOnClose(c.unlock)
			hasRowsCloser = true
		}
		return rows, nil
	}

	if c.trace {
		defer traceSQL(time.Now(), query, nvargs)
	}

	done := make(chan struct{})
	go func() {
		rows, err = c._queryDirect(query, !c.inTx)
		close(done)
	}()

	select {
	case <-ctx.Done():
		c.dbConn.cancel()
		return nil, ctx.Err()
	case <-done:
		if onCloser, ok := rows.(onCloser); ok {
			onCloser.setOnClose(c.unlock)
			hasRowsCloser = true
		}
		c.lastError = err
		return rows, err
	}
}

// ExecContext implements the driver.ExecerContext interface.
func (c *conn) ExecContext(ctx context.Context, query string, nvargs []driver.NamedValue) (r driver.Result, err error) {
	if err := c.tryLock(0); err != nil {
		return nil, err
	}
	defer c.unlock()

	if c.isBad() {
		return nil, driver.ErrBadConn
	}

	if len(nvargs) != 0 {
		return nil, driver.ErrSkip //fast path not possible (prepare needed)
	}

	if c.trace {
		defer traceSQL(time.Now(), query, nvargs)
	}

	done := make(chan struct{})
	go func() {
		/*
			handle call procedure (qd.Kind() == p.QkCall) without parameters here as well
		*/
		var qd *queryDescr

		if qd, err = newQueryDescr(query, c.scanner); err != nil {
			goto done
		}
		r, err = c._execDirect(qd.query, !c.inTx)
	done:
		close(done)
	}()

	select {
	case <-ctx.Done():
		c.dbConn.cancel()
		return nil, ctx.Err()
	case <-done:
		c.lastError = err
		return r, err
	}
}

// CheckNamedValue implements the NamedValueChecker interface.
func (c *conn) CheckNamedValue(nv *driver.NamedValue) error {
	// - called by sql driver for ExecContext and QueryContext
	// - no check needs to be performed as ExecContext and QueryContext provided
	//   with parameters will force the 'prepare way' (driver.ErrSkip)
	// - Anyway, CheckNamedValue must be implemented to avoid default sql driver checks
	//   which would fail for custom arg types like Lob
	return nil
}

// Conn Raw access methods

// HDBVersion implements the Conn interface.
func (c *conn) HDBVersion() *Version { return c.hdbVersion }

// DatabaseName implements the Conn interface.
func (c *conn) DatabaseName() string { return c._databaseName() }

// DBConnectInfo implements the Conn interface.
func (c *conn) DBConnectInfo(ctx context.Context, databaseName string) (ci *DBConnectInfo, err error) {
	if err := c.tryLock(0); err != nil {
		return nil, err
	}
	defer c.unlock()

	if c.isBad() {
		return nil, driver.ErrBadConn
	}

	done := make(chan struct{})
	go func() {
		ci, err = c._dbConnectInfo(databaseName)
		close(done)
	}()

	select {
	case <-ctx.Done():
		c.dbConn.cancel()
		return nil, ctx.Err()
	case <-done:
		c.lastError = err
		return ci, err
	}
}

func traceSQL(start time.Time, query string, nvargs []driver.NamedValue) {
	ms := time.Since(start).Milliseconds()
	switch {
	case len(nvargs) == 0:
		sqltrace.Tracef("%s duration %dms", query, ms)
	case len(nvargs) > maxNumTraceArg:
		sqltrace.Tracef("%s args(limited to %d) %v duration %dms", query, maxNumTraceArg, nvargs[:maxNumTraceArg], ms)
	default:
		sqltrace.Tracef("%s args %v duration %dms", query, nvargs, ms)
	}
}

func (c *conn) addTimeValue(start time.Time, k int) {
	c.metrics.addTimeValue(k, time.Since(start).Nanoseconds())
}

//transaction

// check if tx implements all required interfaces
var (
	_ driver.Tx = (*tx)(nil)
)

type tx struct {
	conn   *conn
	closed bool
}

func newTx(conn *conn) *tx { return &tx{conn: conn} }

func (t *tx) Commit() error   { return t.close(false) }
func (t *tx) Rollback() error { return t.close(true) }

func (t *tx) close(rollback bool) (err error) {
	c := t.conn

	c.lock()
	defer c.unlock()

	if t.closed {
		return nil
	}
	t.closed = true

	c.inTx = false

	c.metrics.addGaugeValue(gaugeTx, -1) // decrement number of transactions.

	if c.isBad() {
		return driver.ErrBadConn
	}

	if rollback {
		err = c._rollback()
	} else {
		err = c._commit()
	}
	return
}

/*
statements

nvargs // TODO handling of nvargs when real named args are supported (v1.0.0)
. check support (v1.0.0)
  . call (most probably as HANA does support parameter names)
  . query input parameters (most probably not, as HANA does not support them)
  . exec input parameters (could be done (map to table field name) but is it worth the effort?
*/

// check if statements implements all required interfaces
var (
	_ driver.Stmt              = (*stmt)(nil)
	_ driver.StmtExecContext   = (*stmt)(nil)
	_ driver.StmtQueryContext  = (*stmt)(nil)
	_ driver.NamedValueChecker = (*stmt)(nil)

	_ driver.Stmt              = (*callStmt)(nil)
	_ driver.StmtExecContext   = (*callStmt)(nil)
	_ driver.StmtQueryContext  = (*callStmt)(nil)
	_ driver.NamedValueChecker = (*callStmt)(nil)
)

type stmt struct {
	conn              *conn
	query             string
	pr                *prepareResult
	bulk, flush, many bool
	bulkSize, numBulk int
	nvargs            []driver.NamedValue // bulk or many
}

func newStmt(conn *conn, query string, bulk bool, bulkSize int, pr *prepareResult) *stmt {
	return &stmt{conn: conn, query: query, pr: pr, bulk: bulk, bulkSize: bulkSize}
}

type callStmt struct {
	conn  *conn
	query string
	pr    *prepareResult
}

func newCallStmt(conn *conn, query string, pr *prepareResult) *callStmt {
	return &callStmt{conn: conn, query: query, pr: pr}
}

/*
NumInput differs dependent on statement (check is done in QueryContext and ExecContext):
- #args == #param (only in params):    query, exec, exec bulk (non control query)
- #args == #param (in and out params): exec call
- #args == 0:                          exec bulk (control query)
- #args == #input param:               query call
*/
func (s *stmt) NumInput() int     { return -1 }
func (s *callStmt) NumInput() int { return -1 }

// stmt methods

/*
reset args
- keep slice to avoid additional allocations but
- free elements (GC)
*/
func (s *stmt) resetArgs() {
	for i := 0; i < len(s.nvargs); i++ {
		s.nvargs[i].Value = nil
	}
	s.nvargs = s.nvargs[:0]
}

func (s *stmt) Close() error {
	c := s.conn

	c.lock()
	defer c.unlock()

	s.conn.metrics.addGaugeValue(gaugeStmt, -1) // decrement number of statements.

	if c.isBad() {
		return driver.ErrBadConn
	}

	if s.nvargs != nil {
		if len(s.nvargs) != 0 { // log always //TODO: Fatal?
			dlog.Printf("close: %s - not flushed records: %d)", s.query, len(s.nvargs)/s.pr.numField())
		}
		s.nvargs = nil
	}

	return c._dropStatementID(s.pr.stmtID)
}

func (s *stmt) QueryContext(ctx context.Context, nvargs []driver.NamedValue) (rows driver.Rows, err error) {
	c := s.conn

	if err := c.tryLock(lrNestedQuery); err != nil {
		return nil, err
	}
	hasRowsCloser := false
	defer func() {
		// unlock connection if rows will not do it
		if !hasRowsCloser {
			c.unlock()
		}
	}()

	if c.isBad() {
		return nil, driver.ErrBadConn
	}

	if len(nvargs) != s.pr.numField() { // all fields needs to be input fields
		return nil, fmt.Errorf("invalid number of arguments %d - %d expected", len(nvargs), s.pr.numField())
	}

	if c.trace {
		defer traceSQL(time.Now(), s.query, nvargs)
	}

	done := make(chan struct{})
	go func() {
		rows, err = c._query(s.pr, nvargs, !c.inTx)
		close(done)
	}()

	select {
	case <-ctx.Done():
		c.dbConn.cancel()
		return nil, ctx.Err()
	case <-done:
		if onCloser, ok := rows.(onCloser); ok {
			onCloser.setOnClose(c.unlock)
			hasRowsCloser = true
		}
		c.lastError = err
		return rows, err
	}
}

func (s *stmt) ExecContext(ctx context.Context, nvargs []driver.NamedValue) (driver.Result, error) {
	numArg := len(nvargs)
	switch {
	case s.bulk:
		flush := s.flush
		s.flush = false
		if numArg != 0 && numArg != s.pr.numField() {
			return nil, fmt.Errorf("invalid number of arguments %d - %d expected", numArg, s.pr.numField())
		}
		return s.execBulk(ctx, nvargs, flush)
	case s.many:
		s.many = false
		if numArg != 1 {
			return nil, fmt.Errorf("invalid argument of arguments %d when using composite arguments - 1 expected", numArg)
		}
		return s.execMany(ctx, &nvargs[0])
	default:
		if numArg != s.pr.numField() {
			return nil, fmt.Errorf("invalid number of arguments %d - %d expected", numArg, s.pr.numField())
		}
		return s.exec(ctx, nvargs)
	}
}

func (s *stmt) exec(ctx context.Context, nvargs []driver.NamedValue) (r driver.Result, err error) {
	c := s.conn

	if err := c.tryLock(0); err != nil {
		return nil, err
	}
	defer c.unlock()

	if c.isBad() {
		return nil, driver.ErrBadConn
	}

	if connHook != nil {
		connHook(c, choStmtExec)
	}

	if c.trace {
		defer traceSQL(time.Now(), s.query, nvargs)
	}

	done := make(chan struct{})
	go func() {
		r, err = c._execBulk(s.pr, nvargs, !c.inTx) //TODO
		close(done)
	}()

	select {
	case <-ctx.Done():
		c.dbConn.cancel()
		return nil, ctx.Err()
	case <-done:
		c.lastError = err
		return r, err
	}
}

func (s *stmt) execBulk(ctx context.Context, nvargs []driver.NamedValue, flush bool) (r driver.Result, err error) {
	numArg := len(nvargs)

	switch numArg {
	case 0: // exec without args --> flush
		flush = true
	default: // add to argument buffer
		s.nvargs = append(s.nvargs, nvargs...)
		s.numBulk++
		if s.numBulk >= s.bulkSize {
			flush = true
		}
	}

	if !flush || s.numBulk == 0 { // done: no flush
		return driver.ResultNoRows, nil
	}

	// flush
	r, err = s.exec(ctx, s.nvargs)
	s.resetArgs()
	s.numBulk = 0
	return
}

/*
execMany variants
*/

type execManyer interface {
	numRow() int
	fill(conn *conn, pr *prepareResult, startRow, endRow int, nvargs []driver.NamedValue) error
}

type execManyIntfList []interface{}
type execManyIntfMatrix [][]interface{}
type execManyGenList reflect.Value
type execManyGenMatrix reflect.Value

func (em execManyIntfList) numRow() int   { return len(em) }
func (em execManyIntfMatrix) numRow() int { return len(em) }
func (em execManyGenList) numRow() int    { return reflect.Value(em).Len() }
func (em execManyGenMatrix) numRow() int  { return reflect.Value(em).Len() }

func (em execManyIntfList) fill(conn *conn, pr *prepareResult, startRow, endRow int, nvargs []driver.NamedValue) error {
	rows := em[startRow:endRow]
	for i, row := range rows {
		row, err := convertValue(conn, pr, 0, row)
		if err != nil {
			return err
		}
		nvargs[i].Value = row
	}
	return nil
}

func (em execManyGenList) fill(conn *conn, pr *prepareResult, startRow, endRow int, nvargs []driver.NamedValue) error {
	cnt := 0
	for i := startRow; i < endRow; i++ {
		row, err := convertValue(conn, pr, 0, reflect.Value(em).Index(i).Interface())
		if err != nil {
			return err
		}
		nvargs[cnt].Value = row
		cnt++
	}
	return nil
}

func (em execManyIntfMatrix) fill(conn *conn, pr *prepareResult, startRow, endRow int, nvargs []driver.NamedValue) error {
	numField := pr.numField()
	rows := em[startRow:endRow]
	cnt := 0
	for i, row := range rows {
		if len(row) != numField {
			return fmt.Errorf("invalid number of fields in row %d - got %d - expected %d", i, len(row), numField)
		}
		for j, col := range row {
			col, err := convertValue(conn, pr, j, col)
			if err != nil {
				return err
			}
			nvargs[cnt].Value = col
			cnt++
		}
	}
	return nil
}

func (em execManyGenMatrix) fill(conn *conn, pr *prepareResult, startRow, endRow int, nvargs []driver.NamedValue) error {
	numField := pr.numField()
	cnt := 0
	for i := startRow; i < endRow; i++ {
		v, ok := convertMany(reflect.Value(em).Index(i).Interface())
		if !ok {
			return fmt.Errorf("invalid 'many' argument type %[1]T %[1]v", v)
		}
		row := reflect.ValueOf(v) // need to be array or slice
		if row.Len() != numField {
			return fmt.Errorf("invalid number of fields in row %d - got %d - expected %d", i, row.Len(), numField)
		}
		for j := 0; j < numField; j++ {
			col := row.Index(j).Interface()
			col, err := convertValue(conn, pr, j, col)
			if err != nil {
				return err
			}
			nvargs[cnt].Value = col
			cnt++
		}
	}
	return nil
}

func (s *stmt) newExecManyVariant(numField int, v interface{}) execManyer {
	if numField == 1 {
		if v, ok := v.([]interface{}); ok {
			return execManyIntfList(v)
		}
		return execManyGenList(reflect.ValueOf(v))
	}
	if v, ok := v.([][]interface{}); ok {
		return execManyIntfMatrix(v)
	}
	return execManyGenMatrix(reflect.ValueOf(v))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

/*
Non 'atomic' (transactional) operation due to the split in packages (maxBulkSize),
execMany data might only be written partially to the database in case of hdb stmt errors.
*/
func (s *stmt) execMany(ctx context.Context, nvarg *driver.NamedValue) (driver.Result, error) {

	if len(s.nvargs) != 0 {
		return driver.ResultNoRows, fmt.Errorf("execMany: not flushed entries: %d)", len(s.nvargs))
	}

	numField := s.pr.numField()

	defer func() { s.resetArgs() }() // reset args

	var totalRowsAffected int64

	variant := s.newExecManyVariant(numField, nvarg.Value)
	numRow := variant.numRow()

	size := min(numRow*numField, s.bulkSize*numField)
	if s.nvargs == nil || cap(s.nvargs) < size {
		s.nvargs = make([]driver.NamedValue, size)
	} else {
		s.nvargs = s.nvargs[:size]
	}

	numPack := numRow / s.bulkSize
	if numRow%s.bulkSize != 0 {
		numPack++
	}

	for p := 0; p < numPack; p++ {

		startRow := p * s.bulkSize
		endRow := min(startRow+s.bulkSize, numRow)

		nvargs := s.nvargs[0 : (endRow-startRow)*numField]

		if err := variant.fill(s.conn, s.pr, startRow, endRow, nvargs); err != nil {
			return driver.RowsAffected(totalRowsAffected), err
		}

		// flush
		r, err := s.exec(ctx, nvargs)
		if err != nil {
			return driver.RowsAffected(totalRowsAffected), err
		}
		n, err := r.RowsAffected()
		totalRowsAffected += n
		if err != nil {
			return driver.RowsAffected(totalRowsAffected), err
		}
	}

	return driver.RowsAffected(totalRowsAffected), nil
}

// CheckNamedValue implements NamedValueChecker interface.
func (s *stmt) CheckNamedValue(nv *driver.NamedValue) error {
	// check on bulk args
	if nv.Name == bulk {
		if ptr, ok := nv.Value.(**struct{}); ok {
			switch ptr {
			case &noFlushTok:
				s.bulk = true
				return driver.ErrRemoveArgument
			case &flushTok:
				s.flush = true
				return driver.ErrRemoveArgument
			}
		}
	}

	// check on standard value
	err := convertNamedValue(s.conn, s.pr, nv)
	if err == nil || s.bulk || nv.Ordinal != 1 {
		return err // return err in case ordinal != 1
	}

	// check first argument if 'composite'
	var ok bool
	if nv.Value, ok = convertMany(nv.Value); !ok {
		return err
	}

	s.many = true
	return nil

}

// callStmt methods

func (s *callStmt) Close() error {
	c := s.conn

	c.lock()
	defer c.unlock()

	if c.isBad() {
		return driver.ErrBadConn
	}

	s.conn.metrics.addGaugeValue(gaugeStmt, -1) // decrement number of statements.

	return c._dropStatementID(s.pr.stmtID)
}

func (s *callStmt) QueryContext(ctx context.Context, nvargs []driver.NamedValue) (rows driver.Rows, err error) {
	c := s.conn

	if err := c.tryLock(lrNestedQuery); err != nil {
		return nil, err
	}
	hasRowsCloser := false
	defer func() {
		// unlock connection if rows will not do it
		if !hasRowsCloser {
			c.unlock()
		}
	}()

	if c.isBad() {
		return nil, driver.ErrBadConn
	}

	if len(nvargs) != s.pr.numInputField() { // input fields only
		return nil, fmt.Errorf("invalid number of arguments %d - %d expected", len(nvargs), s.pr.numInputField())
	}

	if c.trace {
		defer traceSQL(time.Now(), s.query, nvargs)
	}

	done := make(chan struct{})
	go func() {
		rows, err = c._queryCall(s.pr, nvargs)
		close(done)
	}()

	select {
	case <-ctx.Done():
		c.dbConn.cancel()
		return nil, ctx.Err()
	case <-done:
		if onCloser, ok := rows.(onCloser); ok {
			onCloser.setOnClose(c.unlock)
			hasRowsCloser = true
		}
		c.lastError = err
		return rows, err
	}
}

func (s *callStmt) ExecContext(ctx context.Context, nvargs []driver.NamedValue) (r driver.Result, err error) {
	c := s.conn

	if err := c.tryLock(0); err != nil {
		return nil, err
	}
	defer c.unlock()

	if c.isBad() {
		return nil, driver.ErrBadConn
	}

	if len(nvargs) != s.pr.numField() {
		return nil, fmt.Errorf("invalid number of arguments %d - %d expected", len(nvargs), s.pr.numField())
	}

	if c.trace {
		defer traceSQL(time.Now(), s.query, nvargs)
	}

	done := make(chan struct{})
	go func() {
		r, err = c._execCall(s.pr, nvargs)
		close(done)
	}()

	select {
	case <-ctx.Done():
		c.dbConn.cancel()
		return nil, ctx.Err()
	case <-done:
		c.lastError = err
		return r, err
	}
}

// CheckNamedValue implements NamedValueChecker interface.
func (s *callStmt) CheckNamedValue(nv *driver.NamedValue) error {
	return convertNamedValue(s.conn, s.pr, nv)
}

const defaultSessionID = -1

func (c *conn) _databaseName() string {
	return c.serverOptions[p.CoDatabaseName].(string)
}

func (c *conn) _dbConnectInfo(databaseName string) (*DBConnectInfo, error) {
	ci := dbConnectInfo{p.CiDatabaseName: databaseName}
	if err := c.pw.Write(c.sessionID, p.MtDBConnectInfo, false, ci); err != nil {
		return nil, err
	}

	if err := c.pr.IterateParts(func(ph *p.PartHeader) {
		switch ph.PartKind {
		case p.PkDBConnectInfo:
			c.pr.Read(&ci)
		}
	}); err != nil {
		return nil, err
	}

	host, _ := ci[p.CiHost].(string) //check existencs and covert to string
	port, _ := ci[p.CiPort].(int32)  // check existence and convert to integer
	isConnected, _ := ci[p.CiIsConnected].(bool)

	return &DBConnectInfo{
		DatabaseName: databaseName,
		Host:         host,
		Port:         int(port),
		IsConnected:  isConnected,
	}, nil
}

func (c *conn) _authenticate(auth *p.Auth, applicationName string, dfv int, locale string) (int64, connectOptions, error) {
	defer c.addTimeValue(time.Now(), timeAuth)

	// client context
	clientContext := clientContext{
		p.CcoClientVersion:            DriverVersion,
		p.CcoClientType:               clientType,
		p.CcoClientApplicationProgram: applicationName,
	}

	initRequest, err := auth.InitRequest()
	if err != nil {
		return 0, nil, err
	}
	if err := c.pw.Write(c.sessionID, p.MtAuthenticate, false, clientContext, initRequest); err != nil {
		return 0, nil, err
	}

	initReply, err := auth.InitReply()
	if err != nil {
		return 0, nil, err
	}
	if err := c.pr.IterateParts(func(ph *p.PartHeader) {
		if ph.PartKind == p.PkAuthentication {
			c.pr.Read(initReply)
		}
	}); err != nil {
		return 0, nil, err
	}

	finalRequest, err := auth.FinalRequest()
	if err != nil {
		return 0, nil, err
	}
	//co := c.defaultClientOptions()

	co := func() connectOptions {
		co := connectOptions{
			p.CoDistributionProtocolVersion: false,
			p.CoSelectForUpdateSupported:    false,
			p.CoSplitBatchCommands:          true,
			p.CoDataFormatVersion2:          int32(dfv),
			p.CoCompleteArrayExecution:      true,
			p.CoClientDistributionMode:      int32(p.CdmOff),
		}
		if locale != "" {
			co[p.CoClientLocale] = locale
		}
		return co
	}()

	if err := c.pw.Write(c.sessionID, p.MtConnect, false, finalRequest, p.ClientID(clientID), co); err != nil {
		return 0, nil, err
	}

	finalReply, err := auth.FinalReply()
	if err != nil {
		return 0, nil, err
	}
	if err := c.pr.IterateParts(func(ph *p.PartHeader) {
		switch ph.PartKind {
		case p.PkAuthentication:
			c.pr.Read(finalReply)
		case p.PkConnectOptions:
			c.pr.Read(&co)
			// set data format version
			// TODO generalize for sniffer
			c.pr.SetDfv(int(co[p.CoDataFormatVersion2].(int32)))
		}
	}); err != nil {
		return 0, nil, err
	}
	return c.pr.SessionID(), co, nil
}

func (c *conn) _queryDirect(query string, commit bool) (driver.Rows, error) {
	defer c.addTimeValue(time.Now(), timeQuery)

	// allow e.g inserts as query -> handle commit like in _execDirect
	if err := c.pw.Write(c.sessionID, p.MtExecuteDirect, commit, p.Command(query)); err != nil {
		return nil, err
	}

	qr := &queryResult{conn: c}
	meta := &p.ResultMetadata{}
	resSet := &p.Resultset{}

	if err := c.pr.IterateParts(func(ph *p.PartHeader) {
		switch ph.PartKind {
		case p.PkResultMetadata:
			c.pr.Read(meta)
			qr.fields = meta.ResultFields
		case p.PkResultsetID:
			c.pr.Read((*p.ResultsetID)(&qr.rsID))
		case p.PkResultset:
			resSet.ResultFields = qr.fields
			c.pr.Read(resSet)
			qr.fieldValues = resSet.FieldValues
			qr.decodeErrors = resSet.DecodeErrors
			qr.attributes = ph.PartAttributes
		}
	}); err != nil {
		return nil, err
	}
	if qr.rsID == 0 { // non select query
		return noResult, nil
	}
	return qr, nil
}

func (c *conn) _execDirect(query string, commit bool) (driver.Result, error) {
	defer c.addTimeValue(time.Now(), timeExec)

	if err := c.pw.Write(c.sessionID, p.MtExecuteDirect, commit, p.Command(query)); err != nil {
		return nil, err
	}

	rows := &p.RowsAffected{}
	var numRow int64
	if err := c.pr.IterateParts(func(ph *p.PartHeader) {
		if ph.PartKind == p.PkRowsAffected {
			c.pr.Read(rows)
			numRow = rows.Total()
		}
	}); err != nil {
		return nil, err
	}
	if c.pr.FunctionCode() == p.FcDDL {
		return driver.ResultNoRows, nil
	}
	return driver.RowsAffected(numRow), nil
}

func (c *conn) _prepare(query string) (*prepareResult, error) {
	defer c.addTimeValue(time.Now(), timePrepare)

	if err := c.pw.Write(c.sessionID, p.MtPrepare, false, p.Command(query)); err != nil {
		return nil, err
	}

	pr := &prepareResult{}
	resMeta := &p.ResultMetadata{}
	prmMeta := &p.ParameterMetadata{}

	if err := c.pr.IterateParts(func(ph *p.PartHeader) {
		switch ph.PartKind {
		case p.PkStatementID:
			c.pr.Read((*p.StatementID)(&pr.stmtID))
		case p.PkResultMetadata:
			c.pr.Read(resMeta)
			pr.resultFields = resMeta.ResultFields
		case p.PkParameterMetadata:
			c.pr.Read(prmMeta)
			pr.parameterFields = prmMeta.ParameterFields
		}
	}); err != nil {
		return nil, err
	}
	pr.fc = c.pr.FunctionCode()
	return pr, nil
}

// fetchFirstLobChunk reads the first LOB data ckunk.
func (c *conn) _fetchFirstLobChunk(nvargs []driver.NamedValue) (bool, error) {
	hasNext := false
	for _, arg := range nvargs {
		if lobInDescr, ok := arg.Value.(*p.LobInDescr); ok {
			last, err := lobInDescr.FetchNext(c.lobChunkSize)
			if !last {
				hasNext = true
			}
			if err != nil {
				return hasNext, err
			}
		}
	}
	return hasNext, nil
}

/*
Exec executes a sql statement.

Bulk insert containing LOBs:
  - Precondition:
    .Sending more than one row with partial LOB data.
  - Observations:
    .In hdb version 1 and 2 'piecewise' LOB writing does work.
    .Same does not work in case of geo fields which are LOBs en,- decoded as well.
    .In hana version 4 'piecewise' LOB writing seems not to work anymore at all.
  - Server implementation (not documented):
    .'piecewise' LOB writing is only supported for the last row of a 'bulk insert'.
  - Current implementation:
    One server call in case of
    .'non bulk' execs or
    .'bulk' execs without LOBs
    else potential several server calls (split into packages).
  - Package invariant:
    .for all packages except the last one, the last row contains 'incomplete' LOB data ('piecewise' writing)
*/
func (c *conn) _execBulk(pr *prepareResult, nvargs []driver.NamedValue, commit bool) (driver.Result, error) {
	defer c.addTimeValue(time.Now(), timeExec)

	hasLob := func() bool {
		for _, f := range pr.parameterFields {
			if f.TC.IsLob() {
				return true
			}
		}
		return false
	}()

	// no split needed: no LOB or only one row
	if !hasLob || len(pr.parameterFields) == len(nvargs) {
		return c._exec(pr, nvargs, hasLob, commit)
	}

	// args need to be potentially splitted (piecewise LOB handling)
	numColumns := len(pr.parameterFields)
	numRows := len(nvargs) / numColumns
	totRowsAffected := int64(0)
	lastFrom := 0

	for i := 0; i < numRows; i++ { // row-by-row

		from := i * numColumns
		to := from + numColumns

		hasNext, err := c._fetchFirstLobChunk(nvargs[from:to])
		if err != nil {
			return nil, err
		}

		/*
			trigger server call (exec) if piecewise lob handling is needed
			or we did reach the last row
		*/
		if hasNext || i == (numRows-1) {
			r, err := c._exec(pr, nvargs[lastFrom:to], true, commit)
			//if err != nil {
			//	return driver.RowsAffected(totRowsAffected), err
			//}
			if rowsAffected, err := r.RowsAffected(); err != nil {
				totRowsAffected += rowsAffected
			}
			if err != nil {
				return driver.RowsAffected(totRowsAffected), err
			}
			lastFrom = to
		}
	}
	return driver.RowsAffected(totRowsAffected), nil
}

func (c *conn) _exec(pr *prepareResult, nvargs []driver.NamedValue, hasLob, commit bool) (driver.Result, error) {
	inputParameters, err := p.NewInputParameters(pr.parameterFields, nvargs, hasLob)
	if err != nil {
		return nil, err
	}
	if err := c.pw.Write(c.sessionID, p.MtExecute, commit, p.StatementID(pr.stmtID), inputParameters); err != nil {
		return nil, err
	}

	rows := &p.RowsAffected{}
	var ids []p.LocatorID
	lobReply := &p.WriteLobReply{}
	var rowsAffected int64

	if err := c.pr.IterateParts(func(ph *p.PartHeader) {
		switch ph.PartKind {
		case p.PkRowsAffected:
			c.pr.Read(rows)
			rowsAffected = rows.Total()
		case p.PkWriteLobReply:
			c.pr.Read(lobReply)
			ids = lobReply.IDs
		}
	}); err != nil {
		return nil, err
	}
	fc := c.pr.FunctionCode()

	if len(ids) != 0 {
		/*
			writeLobParameters:
			- chunkReaders
			- nil (no callResult, exec does not have output parameters)
		*/
		if err := c.encodeLobs(nil, ids, pr.parameterFields, nvargs); err != nil {
			return nil, err
		}
	}

	if fc == p.FcDDL {
		return driver.ResultNoRows, nil
	}
	return driver.RowsAffected(rowsAffected), nil
}

func (c *conn) _queryCall(pr *prepareResult, nvargs []driver.NamedValue) (driver.Rows, error) {
	defer c.addTimeValue(time.Now(), timeCall)

	/*
		only in args
		invariant: #inPrmFields == #args
	*/
	var inPrmFields, outPrmFields []*p.ParameterField
	hasInLob := false
	for _, f := range pr.parameterFields {
		if f.In() {
			inPrmFields = append(inPrmFields, f)
			if f.TC.IsLob() {
				hasInLob = true
			}
		}
		if f.Out() {
			outPrmFields = append(outPrmFields, f)
		}
	}

	if hasInLob {
		if _, err := c._fetchFirstLobChunk(nvargs); err != nil {
			return nil, err
		}
	}
	inputParameters, err := p.NewInputParameters(inPrmFields, nvargs, hasInLob)
	if err != nil {
		return nil, err
	}
	if err := c.pw.Write(c.sessionID, p.MtExecute, false, p.StatementID(pr.stmtID), inputParameters); err != nil {
		return nil, err
	}

	/*
		call without lob input parameters:
		--> callResult output parameter values are set after read call
		call with lob input parameters:
		--> callResult output parameter values are set after last lob input write
	*/

	cr, ids, _, err := c._readCall(outPrmFields) // ignore numRow
	if err != nil {
		return nil, err
	}

	if len(ids) != 0 {
		/*
			writeLobParameters:
			- chunkReaders
			- cr (callResult output parameters are set after all lob input parameters are written)
		*/
		if err := c.encodeLobs(cr, ids, inPrmFields, nvargs); err != nil {
			return nil, err
		}
	}

	// legacy mode?
	if c.legacy {
		cr.appendTableRefFields() // TODO review
		for _, qr := range cr.qrs {
			// add to cache
			stdQueryResultCache.set(qr.rsID, qr)
		}
	} else {
		cr.appendTableRowsFields()
	}
	return cr, nil
}

func (c *conn) _execCall(pr *prepareResult, nvargs []driver.NamedValue) (driver.Result, error) {
	defer c.addTimeValue(time.Now(), timeCall)

	/*
		in,- and output args
		invariant: #prmFields == #args
	*/
	var (
		inPrmFields, outPrmFields []*p.ParameterField
		inArgs                    []driver.NamedValue
		// outArgs []driver.NamedValue
	)
	hasInLob := false
	for i, f := range pr.parameterFields {
		if f.In() {
			inPrmFields = append(inPrmFields, f)
			inArgs = append(inArgs, nvargs[i])
			if f.TC.IsLob() {
				hasInLob = true
			}
		}
		if f.Out() {
			outPrmFields = append(outPrmFields, f)
			// outArgs = append(outArgs, nvargs[i])
		}
	}

	// TODO release v1.0.0 - assign output parameters
	if len(outPrmFields) != 0 {
		return nil, fmt.Errorf("stmt.Exec: support of output parameters not implemented yet")
	}

	if hasInLob {
		if _, err := c._fetchFirstLobChunk(inArgs); err != nil {
			return nil, err
		}
	}
	inputParameters, err := p.NewInputParameters(inPrmFields, inArgs, hasInLob)
	if err != nil {
		return nil, err
	}
	if err := c.pw.Write(c.sessionID, p.MtExecute, false, p.StatementID(pr.stmtID), inputParameters); err != nil {
		return nil, err
	}

	/*
		call without lob input parameters:
		--> callResult output parameter values are set after read call
		call with lob output parameters:
		--> callResult output parameter values are set after last lob input write
	*/

	cr, ids, numRow, err := c._readCall(outPrmFields)
	if err != nil {
		return nil, err
	}

	if len(ids) != 0 {
		/*
			writeLobParameters:
			- chunkReaders
			- cr (callResult output parameters are set after all lob input parameters are written)
		*/
		if err := c.encodeLobs(cr, ids, inPrmFields, inArgs); err != nil {
			return nil, err
		}
	}
	return driver.RowsAffected(numRow), nil
}

func (c *conn) _readCall(outputFields []*p.ParameterField) (*callResult, []p.LocatorID, int64, error) {
	cr := &callResult{conn: c, outputFields: outputFields}

	//var qrs []*QueryResult
	var qr *queryResult
	rows := &p.RowsAffected{}
	var ids []p.LocatorID
	outPrms := &p.OutputParameters{}
	meta := &p.ResultMetadata{}
	resSet := &p.Resultset{}
	lobReply := &p.WriteLobReply{}
	var numRow int64

	if err := c.pr.IterateParts(func(ph *p.PartHeader) {
		switch ph.PartKind {
		case p.PkRowsAffected:
			c.pr.Read(rows)
			numRow = rows.Total()
		case p.PkOutputParameters:
			outPrms.OutputFields = cr.outputFields
			c.pr.Read(outPrms)
			cr.fieldValues = outPrms.FieldValues
			cr.decodeErrors = outPrms.DecodeErrors
		case p.PkResultMetadata:
			/*
				procedure call with table parameters does return metadata for each table
				sequence: metadata, resultsetID, resultset
				but:
				- resultset might not be provided for all tables
				- so, 'additional' query result is detected by new metadata part
			*/
			qr = &queryResult{conn: c}
			cr.qrs = append(cr.qrs, qr)
			c.pr.Read(meta)
			qr.fields = meta.ResultFields
		case p.PkResultset:
			resSet.ResultFields = qr.fields
			c.pr.Read(resSet)
			qr.fieldValues = resSet.FieldValues
			qr.decodeErrors = resSet.DecodeErrors
			qr.attributes = ph.PartAttributes
		case p.PkResultsetID:
			c.pr.Read((*p.ResultsetID)(&qr.rsID))
		case p.PkWriteLobReply:
			c.pr.Read(lobReply)
			ids = lobReply.IDs
		}
	}); err != nil {
		return nil, nil, 0, err
	}
	return cr, ids, numRow, nil
}

func (c *conn) _query(pr *prepareResult, nvargs []driver.NamedValue, commit bool) (driver.Rows, error) {
	defer c.addTimeValue(time.Now(), timeQuery)

	// allow e.g inserts as query -> handle commit like in exec

	hasLob := func() bool {
		for _, f := range pr.parameterFields {
			if f.TC.IsLob() {
				return true
			}
		}
		return false
	}()

	if hasLob {
		if _, err := c._fetchFirstLobChunk(nvargs); err != nil {
			return nil, err
		}
	}
	inputParameters, err := p.NewInputParameters(pr.parameterFields, nvargs, hasLob)
	if err != nil {
		return nil, err
	}
	if err := c.pw.Write(c.sessionID, p.MtExecute, commit, p.StatementID(pr.stmtID), inputParameters); err != nil {
		return nil, err
	}

	qr := &queryResult{conn: c, fields: pr.resultFields}
	resSet := &p.Resultset{}

	if err := c.pr.IterateParts(func(ph *p.PartHeader) {
		switch ph.PartKind {
		case p.PkResultsetID:
			c.pr.Read((*p.ResultsetID)(&qr.rsID))
		case p.PkResultset:
			resSet.ResultFields = qr.fields
			c.pr.Read(resSet)
			qr.fieldValues = resSet.FieldValues
			qr.decodeErrors = resSet.DecodeErrors
			qr.attributes = ph.PartAttributes
		}
	}); err != nil {
		return nil, err
	}
	if qr.rsID == 0 { // non select query
		return noResult, nil
	}
	return qr, nil
}

func (c *conn) _fetchNext(qr *queryResult) error {
	//TODO: query?
	defer c.addTimeValue(time.Now(), timeFetch)

	if err := c.pw.Write(c.sessionID, p.MtFetchNext, false, p.ResultsetID(qr.rsID), p.Fetchsize(c.fetchSize)); err != nil {
		return err
	}

	resSet := &p.Resultset{ResultFields: qr.fields, FieldValues: qr.fieldValues} // reuse field values

	return c.pr.IterateParts(func(ph *p.PartHeader) {
		if ph.PartKind == p.PkResultset {
			c.pr.Read(resSet)
			qr.fieldValues = resSet.FieldValues
			qr.decodeErrors = resSet.DecodeErrors
			qr.attributes = ph.PartAttributes
		}
	})
}

func (c *conn) _dropStatementID(id uint64) error {
	if err := c.pw.Write(c.sessionID, p.MtDropStatementID, false, p.StatementID(id)); err != nil {
		return err
	}
	return c.pr.ReadSkip()
}

func (c *conn) _closeResultsetID(id uint64) error {
	if err := c.pw.Write(c.sessionID, p.MtCloseResultset, false, p.ResultsetID(id)); err != nil {
		return err
	}
	return c.pr.ReadSkip()
}

func (c *conn) _commit() error {
	defer c.addTimeValue(time.Now(), timeCommit)

	if err := c.pw.Write(c.sessionID, p.MtCommit, false); err != nil {
		return err
	}
	if err := c.pr.ReadSkip(); err != nil {
		return err
	}
	return nil
}

func (c *conn) _rollback() error {
	defer c.addTimeValue(time.Now(), timeRollback)

	if err := c.pw.Write(c.sessionID, p.MtRollback, false); err != nil {
		return err
	}
	if err := c.pr.ReadSkip(); err != nil {
		return err
	}
	return nil
}

func (c *conn) _disconnect() error {
	if err := c.pw.Write(c.sessionID, p.MtDisconnect, false); err != nil {
		return err
	}
	/*
		Do not read server reply as on slow connections the TCP/IP connection is closed (by Server)
		before the reply can be read completely.

		// if err := s.pr.readSkip(); err != nil {
		// 	return err
		// }

	*/
	return nil
}

// decodeLobs decodes (reads from db) output lob or result lob parameters.

// read lob reply
// - seems like readLobreply returns only a result for one lob - even if more then one is requested
// --> read single lobs
func (c *conn) decodeLobs(descr *p.LobOutDescr, wr io.Writer) error {
	defer c.addTimeValue(time.Now(), timeFetchLob)

	var err error

	if descr.IsCharBased {
		wrcl := transform.NewWriter(wr, c.cesu8Decoder()) // CESU8 transformer
		err = c._decodeLobs(descr, wrcl, func(b []byte) (int64, error) {
			// Caution: hdb counts 4 byte utf-8 encodings (cesu-8 6 bytes) as 2 (3 byte) chars
			numChars := int64(0)
			for len(b) > 0 {
				if !cesu8.FullRune(b) { //
					return 0, fmt.Errorf("lob chunk consists of incomplete CESU-8 runes")
				}
				_, size := cesu8.DecodeRune(b)
				b = b[size:]
				numChars++
				if size == cesu8.CESUMax {
					numChars++
				}
			}
			return numChars, nil
		})
	} else {
		err = c._decodeLobs(descr, wr, func(b []byte) (int64, error) { return int64(len(b)), nil })
	}

	if pw, ok := wr.(*io.PipeWriter); ok { // if the writer is a pipe-end -> close at the end
		if err != nil {
			pw.CloseWithError(err)
		} else {
			pw.Close()
		}
	}
	return err
}

func (c *conn) _decodeLobs(descr *p.LobOutDescr, wr io.Writer, countChars func(b []byte) (int64, error)) error {
	lobChunkSize := int64(c.lobChunkSize)

	chunkSize := func(numChar, ofs int64) int32 {
		chunkSize := numChar - ofs
		if chunkSize > lobChunkSize {
			return int32(lobChunkSize)
		}
		return int32(chunkSize)
	}

	if _, err := wr.Write(descr.B); err != nil {
		return err
	}

	lobRequest := &p.ReadLobRequest{}
	lobRequest.ID = descr.ID

	lobReply := &p.ReadLobReply{}

	eof := descr.Opt.IsLastData()

	ofs, err := countChars(descr.B)
	if err != nil {
		return err
	}

	for !eof {

		lobRequest.Ofs += ofs
		lobRequest.ChunkSize = chunkSize(descr.NumChar, ofs)

		if err := c.pw.Write(c.sessionID, p.MtWriteLob, false, lobRequest); err != nil {
			return err
		}

		if err := c.pr.IterateParts(func(ph *p.PartHeader) {
			if ph.PartKind == p.PkReadLobReply {
				c.pr.Read(lobReply)
			}
		}); err != nil {
			return err
		}

		if lobReply.ID != lobRequest.ID {
			return fmt.Errorf("internal error: invalid lob locator %d - expected %d", lobReply.ID, lobRequest.ID)
		}

		if _, err := wr.Write(lobReply.B); err != nil {
			return err
		}

		ofs, err = countChars(lobReply.B)
		if err != nil {
			return err
		}
		eof = lobReply.Opt.IsLastData()
	}
	return nil
}

// encodeLobs encodes (write to db) input lob parameters.
func (c *conn) encodeLobs(cr *callResult, ids []p.LocatorID, inPrmFields []*p.ParameterField, nvargs []driver.NamedValue) error {

	descrs := make([]*p.WriteLobDescr, 0, len(ids))

	numInPrmField := len(inPrmFields)

	j := 0
	for i, arg := range nvargs { // range over args (mass / bulk operation)
		f := inPrmFields[i%numInPrmField]
		if f.TC.IsLob() {
			lobInDescr, ok := arg.Value.(*p.LobInDescr)
			if !ok {
				return fmt.Errorf("protocol error: invalid lob parameter %[1]T %[1]v - *lobInDescr expected", arg)
			}
			if j >= len(ids) {
				return fmt.Errorf("protocol error: invalid number of lob parameter ids %d", len(ids))
			}
			descrs = append(descrs, &p.WriteLobDescr{LobInDescr: lobInDescr, ID: ids[j]})
			j++
		}
	}

	writeLobRequest := &p.WriteLobRequest{}

	for len(descrs) != 0 {

		if len(descrs) != len(ids) {
			return fmt.Errorf("protocol error: invalid number of lob parameter ids %d - expected %d", len(descrs), len(ids))
		}
		for i, descr := range descrs { // check if ids and descrs are in sync
			if descr.ID != ids[i] {
				return fmt.Errorf("protocol error: lob parameter id mismatch %d - expected %d", descr.ID, ids[i])
			}
		}

		// TODO check total size limit
		for _, descr := range descrs {
			if err := descr.FetchNext(c.lobChunkSize); err != nil {
				return err
			}
		}

		writeLobRequest.Descrs = descrs

		if err := c.pw.Write(c.sessionID, p.MtReadLob, false, writeLobRequest); err != nil {
			return err
		}

		lobReply := &p.WriteLobReply{}
		outPrms := &p.OutputParameters{}

		if err := c.pr.IterateParts(func(ph *p.PartHeader) {
			switch ph.PartKind {
			case p.PkOutputParameters:
				outPrms.OutputFields = cr.outputFields
				c.pr.Read(outPrms)
				cr.fieldValues = outPrms.FieldValues
				cr.decodeErrors = outPrms.DecodeErrors
			case p.PkWriteLobReply:
				c.pr.Read(lobReply)
				ids = lobReply.IDs
			}
		}); err != nil {
			return err
		}

		// remove done descr
		j := 0
		for _, descr := range descrs {
			if !descr.Opt.IsLastData() {
				descrs[j] = descr
				j++
			}
		}
		descrs = descrs[:j]
	}
	return nil
}
