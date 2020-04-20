module github.com/btcsuite/btcwallet/walletdb

go 1.12

require (
	github.com/btcsuite/btclog v0.0.0-20170628155309-84c8d2346e9f
	github.com/davecgh/go-spew v1.1.1
	go.etcd.io/bbolt v1.3.4
)

replace go.etcd.io/bbolt v1.3.4 => github.com/jrick/bbolt v1.3.4-0.20200402153132-d6be1a74f54c
