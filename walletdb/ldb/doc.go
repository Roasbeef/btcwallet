// Copyright (c) 2014 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

/*
Package ldb implements an instance of walletdb that uses leveldb for the backing
datastore.

Usage

This package is only a driver to the walletdb package and provides the database
type of "ldb".  The only parameter the Open and Create functions take is the
database path as a string:

	db, err := walletdb.Open("ldb", "path/to/database")
	if err != nil {
		// Handle error
	}

	db, err := walletdb.Create("ldb", "path/to/database")
	if err != nil {
		// Handle error
	}
*/
package ldb
