//go:build !unit
// +build !unit

// SPDX-FileCopyrightText: 2014-2022 SAP SE
//
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"database/sql"
	"fmt"
	"log"
)

// TODO
// ExampleQuery: tbd
func Example_query() {
	db := sql.OpenDB(NewTestConnector())
	defer db.Close()

	table := RandomIdentifier("testNamedArg_")
	if _, err := db.Exec(fmt.Sprintf("create table %s (i integer, j integer)", table)); err != nil {
		log.Fatal(err)
	}

	var i = 0
	if err := db.QueryRow(fmt.Sprintf("select count(*) from %s where i = :1 and j = :1", table), 1).Scan(&i); err != nil {
		log.Fatal(err)
	}

	if err := db.QueryRow(fmt.Sprintf("select count(*) from %s where i = ? and j = :3", table), 1, "soso", 2).Scan(&i); err != nil {
		log.Fatal(err)
	}

	fmt.Print(i)
	// output: 0
}
