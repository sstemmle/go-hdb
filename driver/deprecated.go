// SPDX-FileCopyrightText: 2014-2022 SAP SE
//
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"database/sql"
	"database/sql/driver"
)

// NullTime represents an time.Time that may be null.
//
// Deprecated: Please use database/sql NullTime instead.
type NullTime = sql.NullTime

// deprecated driver interface methods

// Prepare implements the driver.Conn interface.
func (*conn) Prepare(query string) (driver.Stmt, error) { panic("deprecated") }

// Begin implements the driver.Conn interface.
func (*conn) Begin() (driver.Tx, error) { panic("deprecated") }

// Exec implements the driver.Execer interface.
func (*conn) Exec(query string, args []driver.Value) (driver.Result, error) { panic("deprecated") }

// Query implements the driver.Queryer interface.
func (*conn) Query(query string, args []driver.Value) (driver.Rows, error) { panic("deprecated") }

func (*stmt) Exec(args []driver.Value) (driver.Result, error)             { panic("deprecated") }
func (*stmt) Query(args []driver.Value) (rows driver.Rows, err error)     { panic("deprecated") }
func (*callStmt) Exec(args []driver.Value) (driver.Result, error)         { panic("deprecated") }
func (*callStmt) Query(args []driver.Value) (rows driver.Rows, err error) { panic("deprecated") }

// replaced driver interface methods
// sql.Stmt.ColumnConverter --> replaced by CheckNamedValue
