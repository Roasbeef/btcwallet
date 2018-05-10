ldb
===

[![Build Status](https://travis-ci.org/roasbeef/btcwallet.png?branch=master)]
(https://travis-ci.org/roasbeef/btcwallet)

Package ldb implements an driver for walletdb that uses levelb for the backing
datastore.  Package ldb is licensed under the copyfree ISC license.

## Usage

This package is only a driver to the walletdb package and provides the database
type of "ldb".  The only parameter the Open and Create functions take is the
database path as a string:

```Go
db, err := walletdb.Open("ldb", "path/to/database")
if err != nil {
	// Handle error
}
```

```Go
db, err := walletdb.Create("ldb", "path/to/database")
if err != nil {
	// Handle error
}
```

## Documentation

[![GoDoc](https://godoc.org/github.com/roasbeef/btcwallet/walletdb/ldb?status.png)]
(http://godoc.org/github.com/roasbeef/btcwallet/walletdb/ldb)

Full `go doc` style documentation for the project can be viewed online without
installing this package by using the GoDoc site here:
http://godoc.org/github.com/roasbeef/btcwallet/walletdb/ldb

You can also view the documentation locally once the package is installed with
the `godoc` tool by running `godoc -http=":6060"` and pointing your browser to
http://localhost:6060/pkg/github.com/roasbeef/btcwallet/walletdb/ldb

## License

Package ldb is licensed under the [copyfree](http://copyfree.org) ISC
License.
