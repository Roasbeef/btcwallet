// Copyright (c) 2015-2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package ldb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/btcsuite/goleveldb/leveldb"
	"github.com/btcsuite/goleveldb/leveldb/comparer"
	"github.com/btcsuite/goleveldb/leveldb/filter"
	"github.com/btcsuite/goleveldb/leveldb/iterator"
	"github.com/btcsuite/goleveldb/leveldb/opt"
	"github.com/btcsuite/goleveldb/leveldb/util"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcwallet/walletdb"
	"github.com/roasbeef/btcwallet/walletdb/ldb/treap"
)

const (
	// metadataDbName is the name used for the metadata database.
	metadataDbName = "metadata"
)

var (
	// byteOrder is the preferred byte order used through the database.
	// Sometimes big endian will be used to allow ordered byte sortable
	// integer values.
	byteOrder = binary.LittleEndian

	// bucketIndexPrefix is the prefix used for all entries in the bucket
	// index.
	bucketIndexPrefix = []byte("bidx")

	// curBucketIDKeyName is the name of the key used to keep track of the
	// current bucket ID counter.
	curBucketIDKeyName = []byte("bidx-cbid")

	// metadataBucketID is the ID of the top-level metadata bucket.
	// It is the value 0 encoded as an unsigned big-endian uint32.
	metadataBucketID = [4]byte{}
)

// convertErr converts the passed leveldb error into a database error with an
// equivalent error code  and the passed description.  It also sets the passed
// error as the underlying error.
func convertErr(ldbErr error) error {
	switch {
	// Database open/create errors.
	case ldbErr == leveldb.ErrClosed:
		return walletdb.ErrDbNotOpen

	// Transaction errors.
	case ldbErr == leveldb.ErrSnapshotReleased:
		return walletdb.ErrTxClosed
	case ldbErr == leveldb.ErrIterReleased:
		return walletdb.ErrTxClosed
	}

	return ldbErr
}

// copySlice returns a copy of the passed slice.  This is mostly used to copy
// leveldb iterator keys and values since they are only valid until the iterator
// is moved instead of during the entirety of the transaction.
func copySlice(slice []byte) []byte {
	ret := make([]byte, len(slice))
	copy(ret, slice)
	return ret
}

// cursor is an internal type used to represent a cursor over key/value pairs
// and nested buckets of a bucket and implements the database.Cursor interface.
type cursor struct {
	bucket      *bucket
	dbIter      iterator.Iterator
	pendingIter iterator.Iterator
	currentIter iterator.Iterator
}

// Enforce cursor implements the database.Cursor interface.
var _ walletdb.ReadWriteCursor = (*cursor)(nil)
var _ walletdb.ReadCursor = (*cursor)(nil)

// Bucket returns the bucket the cursor was created for.
//
// This function is part of the database.Cursor interface implementation.
/*func (c *cursor) Bucket() walletdb.Bucket {
	// Ensure transaction state is valid.
	if err := c.bucket.tx.checkClosed(); err != nil {
		return nil
	}

	return c.bucket
}*/

// Delete removes the current key/value pair the cursor is at without
// invalidating the cursor.
//
// Returns the following errors as required by the interface contract:
//   - ErrIncompatibleValue if attempted when the cursor points to a nested
//     bucket
//   - ErrTxNotWritable if attempted against a read-only transaction
//   - ErrTxClosed if the transaction has already been closed
//
// This function is part of the database.Cursor interface implementation.
func (c *cursor) Delete() error {
	// Ensure transaction state is valid.
	if err := c.bucket.tx.checkClosed(); err != nil {
		return err
	}

	// Error if the cursor is exhausted.
	if c.currentIter == nil {
		return walletdb.ErrIncompatibleValue
	}

	// Do not allow buckets to be deleted via the cursor.
	key := c.currentIter.Key()
	if bytes.HasPrefix(key, bucketIndexPrefix) {
		return walletdb.ErrIncompatibleValue
	}

	c.bucket.tx.deleteKey(copySlice(key), true)
	return nil
}

// skipPendingUpdates skips any keys at the current database iterator position
// that are being updated by the transaction.  The forwards flag indicates the
// direction the cursor is moving.
func (c *cursor) skipPendingUpdates(forwards bool) {
	for c.dbIter.Valid() {
		var skip bool
		key := c.dbIter.Key()
		if c.bucket.tx.pendingRemove.Has(key) {
			skip = true
		} else if c.bucket.tx.pendingKeys.Has(key) {
			skip = true
		}
		if !skip {
			break
		}

		if forwards {
			c.dbIter.Next()
		} else {
			c.dbIter.Prev()
		}
	}
}

// chooseIterator first skips any entries in the database iterator that are
// being updated by the transaction and sets the current iterator to the
// appropriate iterator depending on their validity and the order they compare
// in while taking into account the direction flag.  When the cursor is being
// moved forwards and both iterators are valid, the iterator with the smaller
// key is chosen and vice versa when the cursor is being moved backwards.
func (c *cursor) chooseIterator(forwards bool) bool {
	// Skip any keys at the current database iterator position that are
	// being updated by the transaction.
	c.skipPendingUpdates(forwards)

	// When both iterators are exhausted, the cursor is exhausted too.
	if !c.dbIter.Valid() && !c.pendingIter.Valid() {
		c.currentIter = nil
		return false
	}

	// Choose the database iterator when the pending keys iterator is
	// exhausted.
	if !c.pendingIter.Valid() {
		c.currentIter = c.dbIter
		return true
	}

	// Choose the pending keys iterator when the database iterator is
	// exhausted.
	if !c.dbIter.Valid() {
		c.currentIter = c.pendingIter
		return true
	}

	// Both iterators are valid, so choose the iterator with either the
	// smaller or larger key depending on the forwards flag.
	compare := bytes.Compare(c.dbIter.Key(), c.pendingIter.Key())
	if (forwards && compare > 0) || (!forwards && compare < 0) {
		c.currentIter = c.pendingIter
	} else {
		c.currentIter = c.dbIter
	}
	return true
}

// First positions the cursor at the first key/value pair and returns whether or
// not the pair exists.
//
// This function is part of the database.Cursor interface implementation.
func (c *cursor) First() (key, value []byte) {
	// Ensure transaction state is valid.
	if err := c.bucket.tx.checkClosed(); err != nil {
		return nil, nil
	}

	// Seek to the first key in both the database and pending iterators and
	// choose the iterator that is both valid and has the smaller key.
	c.dbIter.First()
	c.pendingIter.First()
	if c.chooseIterator(true) {
		return c.Key(), c.Value()
	}
	return nil, nil
}

// Last positions the cursor at the last key/value pair and returns whether or
// not the pair exists.
//
// This function is part of the database.Cursor interface implementation.
func (c *cursor) Last() (key, value []byte) {
	// Ensure transaction state is valid.
	if err := c.bucket.tx.checkClosed(); err != nil {
		return nil, nil
	}

	// Seek to the last key in both the database and pending iterators and
	// choose the iterator that is both valid and has the larger key.
	c.dbIter.Last()
	c.pendingIter.Last()
	if c.chooseIterator(false) {
		return c.Key(), c.Value()
	}
	return nil, nil
}

// Next moves the cursor one key/value pair forward and returns whether or not
// the pair exists.
//
// This function is part of the database.Cursor interface implementation.
func (c *cursor) Next() (key, value []byte) {
	// Ensure transaction state is valid.
	if err := c.bucket.tx.checkClosed(); err != nil {
		return nil, nil
	}

	// Nothing to return if cursor is exhausted.
	if c.currentIter == nil {
		return nil, nil
	}

	// Move the current iterator to the next entry and choose the iterator
	// that is both valid and has the smaller key.
	c.currentIter.Next()
	if c.chooseIterator(true) {
		return c.Key(), c.Value()
	}
	return nil, nil
}

// Prev moves the cursor one key/value pair backward and returns whether or not
// the pair exists.
//
// This function is part of the database.Cursor interface implementation.
func (c *cursor) Prev() (key, value []byte) {
	// Ensure transaction state is valid.
	if err := c.bucket.tx.checkClosed(); err != nil {
		return nil, nil
	}

	// Nothing to return if cursor is exhausted.
	if c.currentIter == nil {
		return nil, nil
	}

	// Move the current iterator to the previous entry and choose the
	// iterator that is both valid and has the larger key.
	c.currentIter.Prev()
	if c.chooseIterator(false) {
		return c.Key(), c.Value()
	}
	return nil, nil
}

// Seek positions the cursor at the first key/value pair that is greater than or
// equal to the passed seek key.  Returns false if no suitable key was found.
//
// This function is part of the database.Cursor interface implementation.
func (c *cursor) Seek(seek []byte) (key, value []byte) {
	// Ensure transaction state is valid.
	if err := c.bucket.tx.checkClosed(); err != nil {
		return nil, nil
	}

	// Seek to the provided key in both the database and pending iterators
	// then choose the iterator that is both valid and has the larger key.
	seekKey := bucketizedKey(c.bucket.id, seek)
	c.dbIter.Seek(seekKey)
	c.pendingIter.Seek(seekKey)
	if c.chooseIterator(true) {
		return c.Key(), c.Value()
	}
	return nil, nil
}

// rawKey returns the current key the cursor is pointing to without stripping
// the current bucket prefix or bucket index prefix.
func (c *cursor) rawKey() []byte {
	// Nothing to return if cursor is exhausted.
	if c.currentIter == nil {
		return nil
	}

	return copySlice(c.currentIter.Key())
}

// Key returns the current key the cursor is pointing to.
//
// This function is part of the database.Cursor interface implementation.
func (c *cursor) Key() []byte {
	// Ensure transaction state is valid.
	if err := c.bucket.tx.checkClosed(); err != nil {
		return nil
	}

	// Nothing to return if cursor is exhausted.
	if c.currentIter == nil {
		return nil
	}

	// Slice out the actual key name and make a copy since it is no longer
	// valid after iterating to the next item.
	//
	// The key is after the bucket index prefix and parent ID when the
	// cursor is pointing to a nested bucket.
	key := c.currentIter.Key()
	if bytes.HasPrefix(key, bucketIndexPrefix) {
		key = key[len(bucketIndexPrefix)+4:]
		return copySlice(key)
	}

	// The key is after the bucket ID when the cursor is pointing to a
	// normal entry.
	key = key[len(c.bucket.id):]
	return copySlice(key)
}

// rawValue returns the current value the cursor is pointing to without
// stripping without filtering bucket index values.
func (c *cursor) rawValue() []byte {
	// Nothing to return if cursor is exhausted.
	if c.currentIter == nil {
		return nil
	}

	return copySlice(c.currentIter.Value())
}

// Value returns the current value the cursor is pointing to.  This will be nil
// for nested buckets.
//
// This function is part of the database.Cursor interface implementation.
func (c *cursor) Value() []byte {
	// Ensure transaction state is valid.
	if err := c.bucket.tx.checkClosed(); err != nil {
		return nil
	}

	// Nothing to return if cursor is exhausted.
	if c.currentIter == nil {
		return nil
	}

	// Return nil for the value when the cursor is pointing to a nested
	// bucket.
	if bytes.HasPrefix(c.currentIter.Key(), bucketIndexPrefix) {
		return nil
	}

	return copySlice(c.currentIter.Value())
}

// cursorType defines the type of cursor to create.
type cursorType int

// The following constants define the allowed cursor types.
const (
	// ctKeys iterates through all of the keys in a given bucket.
	ctKeys cursorType = iota

	// ctBuckets iterates through all directly nested buckets in a given
	// bucket.
	ctBuckets

	// ctFull iterates through both the keys and the directly nested buckets
	// in a given bucket.
	ctFull
)

// cursorFinalizer is either invoked when a cursor is being garbage collected or
// called manually to ensure the underlying cursor iterators are released.
func cursorFinalizer(c *cursor) {
	c.dbIter.Release()
	c.pendingIter.Release()
}

// newCursor returns a new cursor for the given bucket, bucket ID, and cursor
// type.
//
// NOTE: The caller is responsible for calling the cursorFinalizer function on
// the returned cursor.
func newCursor(b *bucket, bucketID []byte, cursorTyp cursorType) *cursor {
	var dbIter, pendingIter iterator.Iterator
	switch cursorTyp {
	case ctKeys:
		keyRange := util.BytesPrefix(bucketID)
		dbIter = b.tx.snapshot.NewIterator(keyRange)
		pendingKeyIter := newLdbTreapIter(b.tx, keyRange)
		pendingIter = pendingKeyIter

	case ctBuckets:
		// The serialized bucket index key format is:
		//   <bucketindexprefix><parentbucketid><bucketname>

		// Create an iterator for the both the database and the pending
		// keys which are prefixed by the bucket index identifier and
		// the provided bucket ID.
		prefix := make([]byte, len(bucketIndexPrefix)+4)
		copy(prefix, bucketIndexPrefix)
		copy(prefix[len(bucketIndexPrefix):], bucketID)
		bucketRange := util.BytesPrefix(prefix)

		dbIter = b.tx.snapshot.NewIterator(bucketRange)
		pendingBucketIter := newLdbTreapIter(b.tx, bucketRange)
		pendingIter = pendingBucketIter

	case ctFull:
		fallthrough
	default:
		// The serialized bucket index key format is:
		//   <bucketindexprefix><parentbucketid><bucketname>
		prefix := make([]byte, len(bucketIndexPrefix)+4)
		copy(prefix, bucketIndexPrefix)
		copy(prefix[len(bucketIndexPrefix):], bucketID)
		bucketRange := util.BytesPrefix(prefix)
		keyRange := util.BytesPrefix(bucketID)

		// Since both keys and buckets are needed from the database,
		// create an individual iterator for each prefix and then create
		// a merged iterator from them.
		dbKeyIter := b.tx.snapshot.NewIterator(keyRange)
		dbBucketIter := b.tx.snapshot.NewIterator(bucketRange)
		iters := []iterator.Iterator{dbKeyIter, dbBucketIter}
		dbIter = iterator.NewMergedIterator(iters,
			comparer.DefaultComparer, true)

		// Since both keys and buckets are needed from the pending keys,
		// create an individual iterator for each prefix and then create
		// a merged iterator from them.
		pendingKeyIter := newLdbTreapIter(b.tx, keyRange)
		pendingBucketIter := newLdbTreapIter(b.tx, bucketRange)
		iters = []iterator.Iterator{pendingKeyIter, pendingBucketIter}
		pendingIter = iterator.NewMergedIterator(iters,
			comparer.DefaultComparer, true)
	}

	// Create the cursor using the iterators.
	return &cursor{bucket: b, dbIter: dbIter, pendingIter: pendingIter}
}

// bucket is an internal type used to represent a collection of key/value pairs
// and implements the database.Bucket interface.
type bucket struct {
	tx *transaction
	id [4]byte
}

// Enforce bucket implements the database.Bucket interface.
var _ walletdb.ReadWriteBucket = (*bucket)(nil)
var _ walletdb.ReadBucket = (*bucket)(nil)

func (b *bucket) DeleteNestedBucket(key []byte) error {
	return b.DeleteBucket(key)
}

func (b *bucket) NestedReadBucket(key []byte) walletdb.ReadBucket {
	return b.Bucket(key)
}

func (b *bucket) NestedReadWriteBucket(key []byte) walletdb.ReadWriteBucket {
	return b.Bucket(key)
}

func (b *bucket) ReadCursor() walletdb.ReadCursor {
	return b.Cursor()
}

func (b *bucket) ReadWriteCursor() walletdb.ReadWriteCursor {
	return b.Cursor()
}

// bucketIndexKey returns the actual key to use for storing and retrieving a
// child bucket in the bucket index.  This is required because additional
// information is needed to distinguish nested buckets with the same name.
func bucketIndexKey(parentID [4]byte, key []byte) []byte {
	// The serialized bucket index key format is:
	//   <bucketindexprefix><parentbucketid><bucketname>
	indexKey := make([]byte, len(bucketIndexPrefix)+4+len(key))
	copy(indexKey, bucketIndexPrefix)
	copy(indexKey[len(bucketIndexPrefix):], parentID[:])
	copy(indexKey[len(bucketIndexPrefix)+4:], key)
	return indexKey
}

// bucketizedKey returns the actual key to use for storing and retrieving a key
// for the provided bucket ID.  This is required because bucketizing is handled
// through the use of a unique prefix per bucket.
func bucketizedKey(bucketID [4]byte, key []byte) []byte {
	// The serialized block index key format is:
	//   <bucketid><key>
	bKey := make([]byte, 4+len(key))
	copy(bKey, bucketID[:])
	copy(bKey[4:], key)
	return bKey
}

// Bucket retrieves a nested bucket with the given key.  Returns nil if
// the bucket does not exist.
//
// This function is part of the database.Bucket interface implementation.
func (b *bucket) Bucket(key []byte) walletdb.ReadWriteBucket {
	// Ensure transaction state is valid.
	if err := b.tx.checkClosed(); err != nil {
		return nil
	}

	// Attempt to fetch the ID for the child bucket.  The bucket does not
	// exist if the bucket index entry does not exist.
	childID := b.tx.fetchKey(bucketIndexKey(b.id, key))
	if childID == nil {
		return nil
	}

	childBucket := &bucket{tx: b.tx}
	copy(childBucket.id[:], childID)
	return childBucket
}

// CreateBucket creates and returns a new nested bucket with the given key.
//
// Returns the following errors as required by the interface contract:
//   - ErrBucketExists if the bucket already exists
//   - ErrBucketNameRequired if the key is empty
//   - ErrIncompatibleValue if the key is otherwise invalid for the particular
//     implementation
//   - ErrTxNotWritable if attempted against a read-only transaction
//   - ErrTxClosed if the transaction has already been closed
//
// This function is part of the database.Bucket interface implementation.
func (b *bucket) CreateBucket(key []byte) (walletdb.ReadWriteBucket, error) {
	// Ensure transaction state is valid.
	if err := b.tx.checkClosed(); err != nil {
		return nil, err
	}

	// Ensure the transaction is writable.
	if !b.tx.writable {
		return nil, walletdb.ErrTxNotWritable
	}

	// Ensure a key was provided.
	if len(key) == 0 {
		return nil, walletdb.ErrBucketNameRequired
	}

	// Ensure bucket does not already exist.
	bidxKey := bucketIndexKey(b.id, key)
	if b.tx.hasKey(bidxKey) {
		return nil, walletdb.ErrBucketExists
	}

	// Find the appropriate next bucket ID to use for the new bucket.  In
	// the case of the special internal block index, keep the fixed ID.
	childID, err := b.tx.nextBucketID()
	if err != nil {
		return nil, err
	}

	// Add the new bucket to the bucket index.
	if err = b.tx.putKey(bidxKey, childID[:]); err != nil {
		return nil, convertErr(err)
	}
	return &bucket{tx: b.tx, id: childID}, nil
}

// CreateBucketIfNotExists creates and returns a new nested bucket with the
// given key if it does not already exist.
//
// Returns the following errors as required by the interface contract:
//   - ErrBucketNameRequired if the key is empty
//   - ErrIncompatibleValue if the key is otherwise invalid for the particular
//     implementation
//   - ErrTxNotWritable if attempted against a read-only transaction
//   - ErrTxClosed if the transaction has already been closed
//
// This function is part of the database.Bucket interface implementation.
func (b *bucket) CreateBucketIfNotExists(key []byte) (walletdb.ReadWriteBucket, error) {
	// Ensure transaction state is valid.
	if err := b.tx.checkClosed(); err != nil {
		return nil, err
	}

	// Ensure the transaction is writable.
	if !b.tx.writable {
		return nil, walletdb.ErrTxNotWritable
	}

	// Return existing bucket if it already exists, otherwise create it.
	if bucket := b.Bucket(key); bucket != nil {
		return bucket, nil
	}
	return b.CreateBucket(key)
}

// DeleteBucket removes a nested bucket with the given key.
//
// Returns the following errors as required by the interface contract:
//   - ErrBucketNotFound if the specified bucket does not exist
//   - ErrTxNotWritable if attempted against a read-only transaction
//   - ErrTxClosed if the transaction has already been closed
//
// This function is part of the database.Bucket interface implementation.
func (b *bucket) DeleteBucket(key []byte) error {
	// Ensure transaction state is valid.
	if err := b.tx.checkClosed(); err != nil {
		return err
	}

	// Ensure the transaction is writable.
	if !b.tx.writable {
		return walletdb.ErrTxNotWritable
	}

	// Ensure a key was provided.
	if len(key) == 0 {
		return walletdb.ErrIncompatibleValue
	}

	// Attempt to fetch the ID for the child bucket.  The bucket does not
	// exist if the bucket index entry does not exist.  In the case of the
	// special internal block index, keep the fixed ID.
	bidxKey := bucketIndexKey(b.id, key)
	childID := b.tx.fetchKey(bidxKey)
	if childID == nil {
		return walletdb.ErrBucketNotFound
	}

	// Remove all nested buckets and their keys.
	childIDs := [][]byte{childID}
	for len(childIDs) > 0 {
		childID = childIDs[len(childIDs)-1]
		childIDs = childIDs[:len(childIDs)-1]

		// Delete all keys in the nested bucket.
		keyCursor := newCursor(b, childID, ctKeys)
		for key, _ := keyCursor.First(); key != nil; key, _ = keyCursor.Next() {
			b.tx.deleteKey(keyCursor.rawKey(), false)
		}
		cursorFinalizer(keyCursor)

		// Iterate through all nested buckets.
		bucketCursor := newCursor(b, childID, ctBuckets)
		for key, _ := bucketCursor.First(); key != nil; key, _ = bucketCursor.Next() {
			// Push the id of the nested bucket onto the stack for
			// the next iteration.
			childID := bucketCursor.rawValue()
			childIDs = append(childIDs, childID)

			// Remove the nested bucket from the bucket index.
			b.tx.deleteKey(bucketCursor.rawKey(), false)
		}
		cursorFinalizer(bucketCursor)
	}

	// Remove the nested bucket from the bucket index.  Any buckets nested
	// under it were already removed above.
	b.tx.deleteKey(bidxKey, true)
	return nil
}

// Cursor returns a new cursor, allowing for iteration over the bucket's
// key/value pairs and nested buckets in forward or backward order.
//
// You must seek to a position using the First, Last, or Seek functions before
// calling the Next, Prev, Key, or Value functions.  Failure to do so will
// result in the same return values as an exhausted cursor, which is false for
// the Prev and Next functions and nil for Key and Value functions.
//
// This function is part of the database.Bucket interface implementation.
func (b *bucket) Cursor() walletdb.ReadWriteCursor {
	// Ensure transaction state is valid.
	if err := b.tx.checkClosed(); err != nil {
		return &cursor{bucket: b}
	}

	// Create the cursor and setup a runtime finalizer to ensure the
	// iterators are released when the cursor is garbage collected.
	c := newCursor(b, b.id[:], ctFull)
	runtime.SetFinalizer(c, cursorFinalizer)
	return c
}

// ForEach invokes the passed function with every key/value pair in the bucket.
// This does not include nested buckets or the key/value pairs within those
// nested buckets.
//
// WARNING: It is not safe to mutate data while iterating with this method.
// Doing so may cause the underlying cursor to be invalidated and return
// unexpected keys and/or values.
//
// Returns the following errors as required by the interface contract:
//   - ErrTxClosed if the transaction has already been closed
//
// NOTE: The values returned by this function are only valid during a
// transaction.  Attempting to access them after a transaction has ended will
// likely result in an access violation.
//
// This function is part of the database.Bucket interface implementation.
func (b *bucket) ForEach(fn func(k, v []byte) error) error {
	// Ensure transaction state is valid.
	if err := b.tx.checkClosed(); err != nil {
		return err
	}

	// Invoke the callback for each cursor item.  Return the error returned
	// from the callback when it is non-nil.
	c := newCursor(b, b.id[:], ctKeys)
	defer cursorFinalizer(c)
	for key, value := c.First(); key != nil; key, value = c.Next() {
		err := fn(key, value)
		if err != nil {
			return err
		}
	}

	return nil
}

// ForEachBucket invokes the passed function with the key of every nested bucket
// in the current bucket.  This does not include any nested buckets within those
// nested buckets.
//
// WARNING: It is not safe to mutate data while iterating with this method.
// Doing so may cause the underlying cursor to be invalidated and return
// unexpected keys.
//
// Returns the following errors as required by the interface contract:
//   - ErrTxClosed if the transaction has already been closed
//
// NOTE: The values returned by this function are only valid during a
// transaction.  Attempting to access them after a transaction has ended will
// likely result in an access violation.
//
// This function is part of the database.Bucket interface implementation.
func (b *bucket) ForEachBucket(fn func(k []byte) error) error {
	// Ensure transaction state is valid.
	if err := b.tx.checkClosed(); err != nil {
		return err
	}

	// Invoke the callback for each cursor item.  Return the error returned
	// from the callback when it is non-nil.
	c := newCursor(b, b.id[:], ctBuckets)
	defer cursorFinalizer(c)
	for key, _ := c.First(); key != nil; key, _ = c.Next() {
		err := fn(key)
		if err != nil {
			return err
		}
	}

	return nil
}

// Writable returns whether or not the bucket is writable.
//
// This function is part of the database.Bucket interface implementation.
func (b *bucket) Writable() bool {
	return b.tx.writable
}

// Put saves the specified key/value pair to the bucket.  Keys that do not
// already exist are added and keys that already exist are overwritten.
//
// Returns the following errors as required by the interface contract:
//   - ErrKeyRequired if the key is empty
//   - ErrIncompatibleValue if the key is the same as an existing bucket
//   - ErrTxNotWritable if attempted against a read-only transaction
//   - ErrTxClosed if the transaction has already been closed
//
// This function is part of the database.Bucket interface implementation.
func (b *bucket) Put(key, value []byte) error {
	// Ensure transaction state is valid.
	if err := b.tx.checkClosed(); err != nil {
		return err
	}

	// Ensure the transaction is writable.
	if !b.tx.writable {
		return walletdb.ErrTxNotWritable
	}

	// Ensure a key was provided.
	if len(key) == 0 {
		return walletdb.ErrKeyRequired
	}

	return b.tx.putKey(bucketizedKey(b.id, key), value)
}

// Get returns the value for the given key.  Returns nil if the key does not
// exist in this bucket.  An empty slice is returned for keys that exist but
// have no value assigned.
//
// NOTE: The value returned by this function is only valid during a transaction.
// Attempting to access it after a transaction has ended results in undefined
// behavior.  Additionally, the value must NOT be modified by the caller.
//
// This function is part of the database.Bucket interface implementation.
func (b *bucket) Get(key []byte) []byte {
	// Ensure transaction state is valid.
	if err := b.tx.checkClosed(); err != nil {
		return nil
	}

	// Nothing to return if there is no key.
	if len(key) == 0 {
		return nil
	}

	return b.tx.fetchKey(bucketizedKey(b.id, key))
}

// Delete removes the specified key from the bucket.  Deleting a key that does
// not exist does not return an error.
//
// Returns the following errors as required by the interface contract:
//   - ErrKeyRequired if the key is empty
//   - ErrIncompatibleValue if the key is the same as an existing bucket
//   - ErrTxNotWritable if attempted against a read-only transaction
//   - ErrTxClosed if the transaction has already been closed
//
// This function is part of the database.Bucket interface implementation.
func (b *bucket) Delete(key []byte) error {
	// Ensure transaction state is valid.
	if err := b.tx.checkClosed(); err != nil {
		return err
	}

	// Ensure the transaction is writable.
	if !b.tx.writable {
		return walletdb.ErrTxNotWritable
	}

	// Nothing to do if there is no key.
	if len(key) == 0 {
		return nil
	}

	b.tx.deleteKey(bucketizedKey(b.id, key), true)
	return nil
}

// pendingBlock houses a block that will be written to disk when the database
// transaction is committed.
type pendingBlock struct {
	hash  *chainhash.Hash
	bytes []byte
}

// transaction represents a database transaction.  It can either be read-only or
// read-write and implements the database.Bucket interface.  The transaction
// provides a root bucket against which all read and writes occur.
type transaction struct {
	managed    bool             // Is the transaction managed?
	closed     bool             // Is the transaction closed?
	writable   bool             // Is the transaction writable?
	db         *db              // DB instance the tx was created from.
	snapshot   *dbCacheSnapshot // Underlying snapshot for txns.
	metaBucket *bucket          // The root metadata bucket.

	// Keys that need to be stored or deleted on commit.
	pendingKeys   *treap.Mutable
	pendingRemove *treap.Mutable

	// Active iterators that need to be notified when the pending keys have
	// been updated so the cursors can properly handle updates to the
	// transaction state.
	activeIterLock sync.RWMutex
	activeIters    []*treap.Iterator
}

// Enforce transaction implements the database.Tx interface.
var _ walletdb.ReadWriteTx = (*transaction)(nil)
var _ walletdb.ReadTx = (*transaction)(nil)

// removeActiveIter removes the passed iterator from the list of active
// iterators against the pending keys treap.
func (tx *transaction) removeActiveIter(iter *treap.Iterator) {
	// An indexing for loop is intentionally used over a range here as range
	// does not reevaluate the slice on each iteration nor does it adjust
	// the index for the modified slice.
	tx.activeIterLock.Lock()
	for i := 0; i < len(tx.activeIters); i++ {
		if tx.activeIters[i] == iter {
			copy(tx.activeIters[i:], tx.activeIters[i+1:])
			tx.activeIters[len(tx.activeIters)-1] = nil
			tx.activeIters = tx.activeIters[:len(tx.activeIters)-1]
		}
	}
	tx.activeIterLock.Unlock()
}

// addActiveIter adds the passed iterator to the list of active iterators for
// the pending keys treap.
func (tx *transaction) addActiveIter(iter *treap.Iterator) {
	tx.activeIterLock.Lock()
	tx.activeIters = append(tx.activeIters, iter)
	tx.activeIterLock.Unlock()
}

// notifyActiveIters notifies all of the active iterators for the pending keys
// treap that it has been updated.
func (tx *transaction) notifyActiveIters() {
	tx.activeIterLock.RLock()
	for _, iter := range tx.activeIters {
		iter.ForceReseek()
	}
	tx.activeIterLock.RUnlock()
}

// checkClosed returns an error if the the database or transaction is closed.
func (tx *transaction) checkClosed() error {
	// The transaction is no longer valid if it has been closed.
	if tx.closed {
		return walletdb.ErrTxClosed
	}

	return nil
}

// hasKey returns whether or not the provided key exists in the database while
// taking into account the current transaction state.
func (tx *transaction) hasKey(key []byte) bool {
	// When the transaction is writable, check the pending transaction
	// state first.
	if tx.writable {
		if tx.pendingRemove.Has(key) {
			return false
		}
		if tx.pendingKeys.Has(key) {
			return true
		}
	}

	// Consult the database cache and underlying database.
	return tx.snapshot.Has(key)
}

// putKey adds the provided key to the list of keys to be updated in the
// database when the transaction is committed.
//
// NOTE: This function must only be called on a writable transaction.  Since it
// is an internal helper function, it does not check.
func (tx *transaction) putKey(key, value []byte) error {
	// Prevent the key from being deleted if it was previously scheduled
	// to be deleted on transaction commit.
	tx.pendingRemove.Delete(key)

	// Add the key/value pair to the list to be written on transaction
	// commit.
	tx.pendingKeys.Put(key, value)
	tx.notifyActiveIters()
	return nil
}

// fetchKey attempts to fetch the provided key from the database cache (and
// hence underlying database) while taking into account the current transaction
// state.  Returns nil if the key does not exist.
func (tx *transaction) fetchKey(key []byte) []byte {
	// When the transaction is writable, check the pending transaction
	// state first.
	if tx.writable {
		if tx.pendingRemove.Has(key) {
			return nil
		}
		if value := tx.pendingKeys.Get(key); value != nil {
			return value
		}
	}

	// Consult the database cache and underlying database.
	return tx.snapshot.Get(key)
}

// deleteKey adds the provided key to the list of keys to be deleted from the
// database when the transaction is committed.  The notify iterators flag is
// useful to delay notifying iterators about the changes during bulk deletes.
//
// NOTE: This function must only be called on a writable transaction.  Since it
// is an internal helper function, it does not check.
func (tx *transaction) deleteKey(key []byte, notifyIterators bool) {
	// Remove the key from the list of pendings keys to be written on
	// transaction commit if needed.
	tx.pendingKeys.Delete(key)

	// Add the key to the list to be deleted on transaction	commit.
	tx.pendingRemove.Put(key, nil)

	// Notify the active iterators about the change if the flag is set.
	if notifyIterators {
		tx.notifyActiveIters()
	}
}

// nextBucketID returns the next bucket ID to use for creating a new bucket.
//
// NOTE: This function must only be called on a writable transaction.  Since it
// is an internal helper function, it does not check.
func (tx *transaction) nextBucketID() ([4]byte, error) {
	// Load the currently highest used bucket ID.
	curIDBytes := tx.fetchKey(curBucketIDKeyName)
	curBucketNum := binary.BigEndian.Uint32(curIDBytes)

	// Increment and update the current bucket ID and return it.
	var nextBucketID [4]byte
	binary.BigEndian.PutUint32(nextBucketID[:], curBucketNum+1)
	if err := tx.putKey(curBucketIDKeyName, nextBucketID[:]); err != nil {
		return [4]byte{}, err
	}
	return nextBucketID, nil
}

// Metadata returns the top-most bucket for all metadata storage.
//
// This function is part of the database.Tx interface implementation.
func (tx *transaction) Metadata() walletdb.ReadWriteBucket {
	return tx.metaBucket
}

// close marks the transaction closed then releases any pending data, the
// underlying snapshot, the transaction read lock, and the write lock when the
// transaction is writable.
func (tx *transaction) close() {
	tx.closed = true

	// Clear pending keys that would have been written or deleted on commit.
	tx.pendingKeys = nil
	tx.pendingRemove = nil

	// Release the snapshot.
	if tx.snapshot != nil {
		tx.snapshot.Release()
		tx.snapshot = nil
	}

	tx.db.closeLock.RUnlock()

	// Release the writer lock for writable transactions to unblock any
	// other write transaction which are possibly waiting.
	if tx.writable {
		tx.db.writeLock.Unlock()
	}
}

func (tx *transaction) CreateTopLevelBucket(key []byte) (walletdb.ReadWriteBucket, error) {
	return tx.metaBucket.CreateBucket(key)
}

func (tx *transaction) DeleteTopLevelBucket(key []byte) error {
	return tx.metaBucket.DeleteBucket(key)
}

func (tx *transaction) ReadBucket(key []byte) walletdb.ReadBucket {
	return tx.metaBucket.Bucket(key)
}

func (tx *transaction) ReadWriteBucket(key []byte) walletdb.ReadWriteBucket {
	return tx.metaBucket.Bucket(key)
}

// writePendingAndCommit writes pending block data to the flat block files,
// updates the metadata with their locations as well as the new current write
// location, and commits the metadata to the memory database cache.  It also
// properly handles rollback in the case of failures.
//
// This function MUST only be called when there is pending data to be written.
func (tx *transaction) writePendingAndCommit() error {
	// Atomically update the database cache.  The cache automatically
	// handles flushing to the underlying persistent storage database.
	return tx.db.cache.commitTx(tx)
}

// Commit commits all changes that have been made to the root metadata bucket
// and all of its sub-buckets to the database cache which is periodically synced
// to persistent storage.  In addition, it commits all new blocks directly to
// persistent storage bypassing the db cache.  Blocks can be rather large, so
// this help increase the amount of cache available for the metadata updates and
// is safe since blocks are immutable.
//
// This function is part of the database.Tx interface implementation.
func (tx *transaction) Commit() error {
	// Prevent commits on managed transactions.
	if tx.managed {
		tx.close()
		panic("managed transaction commit not allowed")
	}

	// Ensure transaction state is valid.
	if err := tx.checkClosed(); err != nil {
		return err
	}

	// Regardless of whether the commit succeeds, the transaction is closed
	// on return.
	defer tx.close()

	// Ensure the transaction is writable.
	if !tx.writable {
		return walletdb.ErrTxNotWritable
	}

	// Write pending data.  The function will rollback if any errors occur.
	return tx.writePendingAndCommit()
}

// Rollback undoes all changes that have been made to the root bucket and all of
// its sub-buckets.
//
// This function is part of the database.Tx interface implementation.
func (tx *transaction) Rollback() error {
	// Prevent rollbacks on managed transactions.
	if tx.managed {
		tx.close()
		panic("managed transaction rollback not allowed")
	}

	// Ensure transaction state is valid.
	if err := tx.checkClosed(); err != nil {
		return err
	}

	tx.close()
	return nil
}

// db represents a collection of namespaces which are persisted and implements
// the database.DB interface.  All database access is performed through
// transactions which are obtained through the specific Namespace.
type db struct {
	writeLock sync.Mutex   // Limit to one write transaction at a time.
	closeLock sync.RWMutex // Make database close block while txns active.
	closed    bool         // Is the database closed?
	cache     *dbCache     // Cache layer which wraps underlying leveldb DB.
}

// Enforce db implements the database.DB interface.
var _ walletdb.DB = (*db)(nil)

// Type returns the database driver type the current database instance was
// created with.
//
// This function is part of the database.DB interface implementation.
func (db *db) Type() string {
	return dbType
}

// begin is the implementation function for the Begin database method.  See its
// documentation for more details.
//
// This function is only separate because it returns the internal transaction
// which is used by the managed transaction code while the database method
// returns the interface.
func (db *db) begin(writable bool) (*transaction, error) {
	// Whenever a new writable transaction is started, grab the write lock
	// to ensure only a single write transaction can be active at the same
	// time.  This lock will not be released until the transaction is
	// closed (via Rollback or Commit).
	if writable {
		db.writeLock.Lock()
	}

	// Whenever a new transaction is started, grab a read lock against the
	// database to ensure Close will wait for the transaction to finish.
	// This lock will not be released until the transaction is closed (via
	// Rollback or Commit).
	db.closeLock.RLock()
	if db.closed {
		db.closeLock.RUnlock()
		if writable {
			db.writeLock.Unlock()
		}
		return nil, walletdb.ErrDbNotOpen
	}

	// Grab a snapshot of the database cache (which in turn also handles the
	// underlying database).
	snapshot, err := db.cache.Snapshot()
	if err != nil {
		db.closeLock.RUnlock()
		if writable {
			db.writeLock.Unlock()
		}

		return nil, err
	}

	// The metadata and block index buckets are internal-only buckets, so
	// they have defined IDs.
	tx := &transaction{
		writable:      writable,
		db:            db,
		snapshot:      snapshot,
		pendingKeys:   treap.NewMutable(),
		pendingRemove: treap.NewMutable(),
	}
	tx.metaBucket = &bucket{tx: tx, id: metadataBucketID}
	return tx, nil
}

// BeginReadTx starts a read-only transaction.  Multiple read-only transactions
// can be started simultaneously while only a single read-write transaction can
// be started at a time.
//
// NOTE: The transaction must be closed by calling Rollbackon it when it is no
// longer needed.  Failure to do so will result in unclaimed memory.
//
// This function is part of the walletdb.DB interface implementation.
func (db *db) BeginReadTx() (walletdb.ReadTx, error) {
	return db.begin(false)
}

// BeginReadTx starts a read-write transaction.  Multiple read-only transactions
// can be started simultaneously while only a single read-write transaction can
// be started at a time.  The call will block when a read-write transaction is
// already open.
//
// NOTE: The transaction must be closed by calling Rollback or Commit on it when
// it is no longer needed.  Failure to do so will result in unclaimed memory.
//
// This function is part of the walletdb.DB interface implementation.
func (db *db) BeginReadWriteTx() (walletdb.ReadWriteTx, error) {
	return db.begin(true)
}

// TODO(aakselrod): implement this
func (db *db) Copy(w io.Writer) error {
	return errors.New("Not implemented yet")
}

// rollbackOnPanic rolls the passed transaction back if the code in the calling
// function panics.  This is needed since the mutex on a transaction must be
// released and a panic in called code would prevent that from happening.
//
// NOTE: This can only be handled manually for managed transactions since they
// control the life-cycle of the transaction.  As the documentation on Begin
// calls out, callers opting to use manual transactions will have to ensure the
// transaction is rolled back on panic if it desires that functionality as well
// or the database will fail to close since the read-lock will never be
// released.
func rollbackOnPanic(tx *transaction) {
	if err := recover(); err != nil {
		tx.managed = false
		_ = tx.Rollback()
		panic(err)
	}
}

// View invokes the passed function in the context of a managed read-only
// transaction with the root bucket for the namespace.  Any errors returned from
// the user-supplied function are returned from this function.
//
// This function is part of the database.DB interface implementation.
func (db *db) View(fn func(walletdb.ReadTx) error) error {
	// Start a read-only transaction.
	tx, err := db.begin(false)
	if err != nil {
		return err
	}

	// Since the user-provided function might panic, ensure the transaction
	// releases all mutexes and resources.  There is no guarantee the caller
	// won't use recover and keep going.  Thus, the database must still be
	// in a usable state on panics due to caller issues.
	defer rollbackOnPanic(tx)

	tx.managed = true
	err = fn(tx)
	tx.managed = false
	if err != nil {
		// The error is ignored here because nothing was written yet
		// and regardless of a rollback failure, the tx is closed now
		// anyways.
		_ = tx.Rollback()
		return err
	}

	return tx.Rollback()
}

// Update invokes the passed function in the context of a managed read-write
// transaction with the root bucket for the namespace.  Any errors returned from
// the user-supplied function will cause the transaction to be rolled back and
// are returned from this function.  Otherwise, the transaction is committed
// when the user-supplied function returns a nil error.
//
// This function is part of the database.DB interface implementation.
func (db *db) Update(fn func(walletdb.ReadWriteTx) error) error {
	// Start a read-write transaction.
	tx, err := db.begin(true)
	if err != nil {
		return err
	}

	// Since the user-provided function might panic, ensure the transaction
	// releases all mutexes and resources.  There is no guarantee the caller
	// won't use recover and keep going.  Thus, the database must still be
	// in a usable state on panics due to caller issues.
	defer rollbackOnPanic(tx)

	tx.managed = true
	err = fn(tx)
	tx.managed = false
	if err != nil {
		// The error is ignored here because nothing was written yet
		// and regardless of a rollback failure, the tx is closed now
		// anyways.
		_ = tx.Rollback()
		return err
	}

	return tx.Commit()
}

// Close cleanly shuts down the database and syncs all data.  It will block
// until all database transactions have been finalized (rolled back or
// committed).
//
// This function is part of the database.DB interface implementation.
func (db *db) Close() error {
	// Since all transactions have a read lock on this mutex, this will
	// cause Close to wait for all readers to complete.
	db.closeLock.Lock()
	defer db.closeLock.Unlock()

	if db.closed {
		return walletdb.ErrDbNotOpen
	}
	db.closed = true

	// NOTE: Since the above lock waits for all transactions to finish and
	// prevents any new ones from being started, it is safe to flush the
	// cache and clear all state without the individual locks.

	// Close the database cache which will flush any existing entries to
	// disk and close the underlying leveldb database.  Any error is saved
	// and returned at the end after the remaining cleanup since the
	// database will be marked closed even if this fails given there is no
	// good way for the caller to recover from a failure here anyways.
	return db.cache.Close()
}

// filesExists reports whether the named file or directory exists.
func fileExists(name string) bool {
	if _, err := os.Stat(name); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// initDB creates the initial buckets and values used by the package.  This is
// mainly in a separate function for testing purposes.
func initDB(ldb *leveldb.DB) error {
	batch := new(leveldb.Batch)
	// Initialize the current bucket ID as the metadata bucket's ID.
	batch.Put(curBucketIDKeyName, metadataBucketID[:])

	// Write everything as a single batch.
	if err := ldb.Write(batch, nil); err != nil {
		return convertErr(err)
	}

	return nil
}

// openDB opens the database at the provided path.  database.ErrDbDoesNotExist
// is returned if the database doesn't exist and the create flag is not set.
func openDB(dbPath string, create bool, useCache bool) (walletdb.DB, error) {
	// Error if the database doesn't exist and the create flag is not set.
	metadataDbPath := filepath.Join(dbPath, metadataDbName)
	dbExists := fileExists(metadataDbPath)
	if !create && !dbExists {
		return nil, walletdb.ErrDbDoesNotExist
	}

	// Ensure the full path to the database exists.
	if !dbExists {
		// The error can be ignored here since the call to
		// leveldb.OpenFile will fail if the directory couldn't be
		// created.
		_ = os.MkdirAll(dbPath, 0700)
	}

	// Open the metadata database (will create it if needed).
	opts := opt.Options{
		Strict:      opt.DefaultStrict,
		Compression: opt.NoCompression,
		Filter:      filter.NewBloomFilter(10),
	}
	ldb, err := leveldb.OpenFile(metadataDbPath, &opts)
	if err != nil {
		return nil, convertErr(err)
	}

	// Create the database cache which wraps the underlying leveldb
	// database to provide write caching.
	var cache *dbCache
	if useCache {
		cache = newDbCache(ldb, defaultCacheSize, defaultFlushSecs)
	} else {
		cache = newDbCache(ldb, 0, 0)
	}
	pdb := &db{cache: cache}

	// Perform initial internal bucket and value creation during database
	// creation.
	if create && !dbExists {
		if err := initDB(pdb.cache.ldb); err != nil {
			return nil, err
		}
	}

	// Perform any reconciliation needed between the block and metadata as
	// well as database initialization, if needed.
	return pdb, nil
}
