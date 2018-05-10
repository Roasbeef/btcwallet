// Copyright (c) 2014 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package ldb

import (
	"fmt"

	"github.com/roasbeef/btcwallet/walletdb"
)

const (
	dbType        = "ldb"
	dbTypeNoCache = "ldb-nc"
)

// parseArgs parses the arguments from the walletdb Open/Create methods.
func parseArgs(funcName string, args ...interface{}) (string, error) {
	if len(args) != 1 {
		return "", fmt.Errorf("invalid arguments to %s.%s -- "+
			"expected database path", dbType, funcName)
	}

	dbPath, ok := args[0].(string)
	if !ok {
		return "", fmt.Errorf("first argument to %s.%s is invalid -- "+
			"expected database path string", dbType, funcName)
	}

	return dbPath, nil
}

// openDBDriver is the callback provided during driver registration that opens
// an existing database for use.
func openDBDriver(args ...interface{}) (walletdb.DB, error) {
	dbPath, err := parseArgs("Open", args...)
	if err != nil {
		return nil, err
	}

	return openDB(dbPath, false, true)
}

// createDBDriver is the callback provided during driver registration that
// creates, initializes, and opens a database for use.
func createDBDriver(args ...interface{}) (walletdb.DB, error) {
	dbPath, err := parseArgs("Create", args...)
	if err != nil {
		return nil, err
	}

	return openDB(dbPath, true, true)
}

// openDBDriverNoCache is the callback provided during driver registration that
// opens an existing database for use and disables caching.
func openDBDriverNoCache(args ...interface{}) (walletdb.DB, error) {
	dbPath, err := parseArgs("Open", args...)
	if err != nil {
		return nil, err
	}

	return openDB(dbPath, false, false)
}

// createDBDriverNoCache is the callback provided during driver registration
// that creates, initializes, and opens a database for use and disables caching.
func createDBDriverNoCache(args ...interface{}) (walletdb.DB, error) {
	dbPath, err := parseArgs("Create", args...)
	if err != nil {
		return nil, err
	}

	return openDB(dbPath, true, false)
}

func init() {
	// Register the driver.
	driver := walletdb.Driver{
		DbType: dbType,
		Create: createDBDriver,
		Open:   openDBDriver,
	}
	driverNoCache := walletdb.Driver{
		DbType: dbTypeNoCache,
		Create: createDBDriverNoCache,
		Open:   openDBDriverNoCache,
	}
	if err := walletdb.RegisterDriver(driver); err != nil {
		panic(fmt.Sprintf("Failed to regiser database driver '%s': %v",
			dbType, err))
	}
	if err := walletdb.RegisterDriver(driverNoCache); err != nil {
		panic(fmt.Sprintf("Failed to regiser database driver '%s': %v",
			dbTypeNoCache, err))
	}
}
