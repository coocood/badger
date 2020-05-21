/*
 * Copyright 2017 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package table

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"sort"

	"github.com/coocood/badger/surf"
	"github.com/coocood/badger/y"
)

type singleKeyIterator struct {
	oldOffset uint32
	latestVer uint64
	latestVal []byte
	oldVals   entrySlice
	idx       int
	oldBlock  []byte
}

func (ski *singleKeyIterator) set(oldOffset uint32, latestVer uint64, latestVal []byte) {
	ski.oldOffset = oldOffset
	numEntries := bytesToU32(ski.oldBlock[oldOffset:])
	endOffsStartIdx := oldOffset + 4
	endOffsEndIdx := endOffsStartIdx + 4*numEntries
	ski.oldVals.endOffs = bytesToU32Slice(ski.oldBlock[endOffsStartIdx:endOffsEndIdx])
	valueEndOff := endOffsEndIdx + ski.oldVals.endOffs[numEntries-1]
	ski.oldVals.data = ski.oldBlock[endOffsEndIdx:valueEndOff]
	ski.latestVer = latestVer
	ski.latestVal = latestVal
}

func (ski *singleKeyIterator) versionAndVal() (ver uint64, val []byte) {
	if ski.idx == 0 {
		return ski.latestVer, ski.latestVal
	}
	oldEntry := ski.oldVals.getEntry(ski.idx - 1)
	return bytesToU64(oldEntry), oldEntry[8:]
}

func (ski *singleKeyIterator) length() int {
	return ski.oldVals.length() + 1
}

func (ski *singleKeyIterator) seekVersion(sVer uint64) (ver uint64, val []byte) {
	for ski.idx = 0; ski.idx < ski.length(); ski.idx++ {
		ver, val = ski.versionAndVal()
		if sVer >= ver {
			return
		}
	}
	return
}

type blockIterator struct {
	entries entrySlice
	idx     int
	err     error

	globalTsBytes [8]byte
	globalTs      uint64
	key           y.Key
	val           []byte

	baseLen uint16
	ski     singleKeyIterator
}

func (itr *blockIterator) setBlock(b block) {
	itr.err = nil
	itr.idx = 0
	itr.key.Reset()
	itr.val = itr.val[:0]
	itr.loadEntries(b.data)
	itr.key.UserKey = append(itr.key.UserKey[:0], b.baseKey[:itr.baseLen]...)
}

func (itr *blockIterator) valid() bool {
	return itr != nil && itr.err == nil
}

func (itr *blockIterator) Error() error {
	return itr.err
}

// loadEntries loads the entryEndOffsets for binary searching for a key.
func (itr *blockIterator) loadEntries(data []byte) {
	// Get the number of entries from the end of `data` (and remove it).
	dataLen := len(data)
	itr.baseLen = binary.LittleEndian.Uint16(data[dataLen-2:])
	entriesNum := int(bytesToU32(data[dataLen-6:]))
	entriesEnd := dataLen - 6
	entriesStart := entriesEnd - entriesNum*4
	itr.entries.endOffs = bytesToU32Slice(data[entriesStart:entriesEnd])
	itr.entries.data = data[:entriesStart]
}

// Seek brings us to the first block element that is >= input key.
// The binary search will begin at `start`, you can use it to skip some items.
func (itr *blockIterator) seek(key y.Key) {
	foundEntryIdx := sort.Search(itr.entries.length(), func(idx int) bool {
		itr.setIdx(idx)
		return bytes.Compare(itr.key.UserKey, key.UserKey) >= 0
	})
	itr.setIdx(foundEntryIdx)
	if itr.err != nil {
		return
	}
	if itr.key.Version > key.Version && bytes.Equal(key.UserKey, itr.key.UserKey) {
		if itr.hasOldVersion() {
			itr.key.Version, itr.val = itr.ski.seekVersion(key.Version)
		}
		if itr.key.Version > key.Version {
			itr.setIdx(foundEntryIdx + 1)
		}
	}
}

// seekToFirst brings us to the first element. Valid should return true.
func (itr *blockIterator) seekToFirst() {
	itr.setIdx(0)
}

// seekToLast brings us to the last element. Valid should return true.
func (itr *blockIterator) seekToLast() {
	itr.setIdx(itr.entries.length() - 1)
	itr.seekToLastVersion()
}

// setIdx sets the iterator to the entry index and set the current key and value.
func (itr *blockIterator) setIdx(i int) {
	itr.idx = i
	if i >= itr.entries.length() || i < 0 {
		itr.err = io.EOF
		return
	}
	itr.err = nil
	entryData := itr.entries.getEntry(i)
	diffKeyLen := binary.LittleEndian.Uint16(entryData)
	entryData = entryData[2:]
	itr.key.UserKey = append(itr.key.UserKey[:itr.baseLen], entryData[:diffKeyLen]...)
	entryData = entryData[diffKeyLen:]
	hasOld := entryData[0] != 0
	entryData = entryData[1:]
	var oldOffset uint32
	if hasOld {
		oldOffset = bytesToU32(entryData)
		entryData = entryData[4:]
	}
	if itr.globalTs != 0 {
		itr.key.Version = itr.globalTs
	} else {
		itr.key.Version = bytesToU64(entryData)
		entryData = entryData[8:]
	}
	itr.val = entryData
	itr.ski.idx = 0
	if hasOld {
		itr.ski.set(oldOffset, itr.key.Version, itr.val)
	}
}

func (itr *blockIterator) hasOldVersion() bool {
	return itr.ski.oldOffset != 0
}

func (itr *blockIterator) next() {
	if itr.hasOldVersion() {
		itr.ski.idx++
		if itr.ski.idx < itr.ski.oldVals.length() {
			itr.key.Version, itr.val = itr.ski.versionAndVal()
			return
		}
	}
	itr.setIdx(itr.idx + 1)
}

func (itr *blockIterator) prev() {
	if itr.prevVersion() {
		return
	}
	itr.setIdx(itr.idx - 1)
	itr.seekToLastVersion()
}

func (itr *blockIterator) prevVersion() bool {
	if itr.hasOldVersion() {
		itr.ski.idx--
		if itr.ski.idx >= 0 {
			itr.key.Version, itr.val = itr.ski.versionAndVal()
			return true
		}
	}
	return false
}

func (itr *blockIterator) seekToLastVersion() {
	if itr.hasOldVersion() {
		itr.ski.idx = itr.ski.length() - 1
		itr.key.Version, itr.val = itr.ski.versionAndVal()
	}
}

// Iterator is an iterator for a Table.
type Iterator struct {
	t    *Table
	tIdx *tableIndex
	surf *surf.Iterator
	bpos int
	bi   blockIterator
	err  error

	// Internally, Iterator is bidirectional. However, we only expose the
	// unidirectional functionality for now.
	reversed bool
}

// NewIterator returns a new iterator of the Table
func (t *Table) NewIterator(reversed bool) *Iterator {
	idx, err := t.getIndex()
	if err != nil {
		return &Iterator{err: err}
	}
	return t.newIterator(reversed, idx)
}

func (t *Table) newIterator(reversed bool, index *tableIndex) *Iterator {
	it := &Iterator{t: t, reversed: reversed, tIdx: index}
	it.bi.globalTs = t.globalTs
	if t.oldBlockLen > 0 {
		y.Assert(len(t.oldBlock) > 0)
	}
	it.bi.ski.oldBlock = t.oldBlock
	binary.BigEndian.PutUint64(it.bi.globalTsBytes[:], math.MaxUint64-t.globalTs)
	if index.surf != nil {
		it.surf = index.surf.NewIterator()
	}
	return it
}

func (itr *Iterator) reset() {
	itr.bpos = 0
	itr.err = nil
}

// Valid follows the y.Iterator interface
func (itr *Iterator) Valid() bool {
	return itr.err == nil
}

func (itr *Iterator) Error() error {
	if itr.err == io.EOF {
		return nil
	}
	return itr.err
}

func (itr *Iterator) seekToFirst() {
	numBlocks := len(itr.tIdx.blockEndOffsets)
	if numBlocks == 0 {
		itr.err = io.EOF
		return
	}
	itr.bpos = 0
	block, err := itr.t.block(itr.bpos, itr.tIdx)
	if err != nil {
		itr.err = err
		return
	}
	itr.bi.setBlock(block)
	itr.bi.seekToFirst()
	itr.err = itr.bi.Error()
}

func (itr *Iterator) seekToLast() {
	numBlocks := len(itr.tIdx.blockEndOffsets)
	if numBlocks == 0 {
		itr.err = io.EOF
		return
	}
	itr.bpos = numBlocks - 1
	block, err := itr.t.block(itr.bpos, itr.tIdx)
	if err != nil {
		itr.err = err
		return
	}
	itr.bi.setBlock(block)
	itr.bi.seekToLast()
	itr.err = itr.bi.Error()
}

func (itr *Iterator) seekInBlock(blockIdx int, key y.Key) {
	itr.bpos = blockIdx
	block, err := itr.t.block(blockIdx, itr.tIdx)
	if err != nil {
		itr.err = err
		return
	}
	itr.bi.setBlock(block)
	itr.bi.seek(key)
	itr.err = itr.bi.Error()
}

func (itr *Iterator) seekFromOffset(blockIdx int, offset int, key y.Key) {
	itr.bpos = blockIdx
	block, err := itr.t.block(blockIdx, itr.tIdx)
	if err != nil {
		itr.err = err
		return
	}
	itr.bi.setBlock(block)
	itr.bi.setIdx(offset)
	if itr.bi.key.Compare(key) >= 0 {
		return
	}
	itr.bi.seek(key)
	itr.err = itr.bi.err
}

func (itr *Iterator) seekBlock(key y.Key) int {
	return sort.Search(len(itr.tIdx.blockEndOffsets), func(idx int) bool {
		blockBaseKey := itr.tIdx.baseKeys.getEntry(idx)
		return bytes.Compare(blockBaseKey, key.UserKey) > 0
	})
}

// seekFrom brings us to a key that is >= input key.
func (itr *Iterator) seekFrom(key y.Key) {
	itr.err = nil
	itr.reset()

	idx := itr.seekBlock(key)
	if itr.err != nil {
		return
	}
	if idx == 0 {
		// The smallest key in our table is already strictly > key. We can return that.
		// This is like a SeekToFirst.
		itr.seekInBlock(0, key)
		return
	}

	// block[idx].smallest is > key.
	// Since idx>0, we know block[idx-1].smallest is <= key.
	// There are two cases.
	// 1) Everything in block[idx-1] is strictly < key. In this case, we should go to the first
	//    element of block[idx].
	// 2) Some element in block[idx-1] is >= key. We should go to that element.
	itr.seekInBlock(idx-1, key)
	if itr.err == io.EOF {
		// Case 1. Need to visit block[idx].
		if idx == len(itr.tIdx.blockEndOffsets) {
			// If idx == len(itr.t.blockEndOffsets), then input key is greater than ANY element of table.
			// There's nothing we can do. Valid() should return false as we seek to end of table.
			return
		}
		// Since block[idx].smallest is > key. This is essentially a block[idx].SeekToFirst.
		itr.seekFromOffset(idx, 0, key)
	}
	// Case 2: No need to do anything. We already did the seek in block[idx-1].
}

// seek will reset iterator and seek to >= key.
func (itr *Iterator) seek(key y.Key) {
	itr.err = nil
	itr.reset()
	if itr.surf == nil {
		itr.seekFrom(key)
		return
	}

	sit := itr.surf
	sit.Seek(key.UserKey)
	if !sit.Valid() {
		itr.err = io.EOF
		return
	}

	var pos entryPosition
	pos.decode(sit.Value())
	itr.seekFromOffset(int(pos.blockIdx), int(pos.offset), key)
}

// seekForPrev will reset iterator and seek to <= key.
func (itr *Iterator) seekForPrev(key y.Key) {
	// TODO: Optimize this. We shouldn't have to take a Prev step.
	itr.seekFrom(key)
	if !itr.Key().Equal(key) {
		itr.prev()
	}
}

func (itr *Iterator) next() {
	itr.err = nil

	if itr.bpos >= len(itr.tIdx.blockEndOffsets) {
		itr.err = io.EOF
		return
	}

	if itr.bi.entries.length() == 0 {
		block, err := itr.t.block(itr.bpos, itr.tIdx)
		if err != nil {
			itr.err = err
			return
		}
		itr.bi.setBlock(block)
		itr.bi.seekToFirst()
		itr.err = itr.bi.Error()
		return
	}

	itr.bi.next()
	if !itr.bi.valid() {
		itr.bpos++
		itr.bi.entries.reset()
		itr.next()
		return
	}
}

func (itr *Iterator) prev() {
	itr.err = nil
	if itr.bpos < 0 {
		itr.err = io.EOF
		return
	}

	if itr.bi.entries.length() == 0 {
		block, err := itr.t.block(itr.bpos, itr.tIdx)
		if err != nil {
			itr.err = err
			return
		}
		itr.bi.setBlock(block)
		itr.bi.seekToLast()
		itr.err = itr.bi.Error()
		return
	}

	itr.bi.prev()
	if !itr.bi.valid() {
		itr.bpos--
		itr.bi.entries.reset()
		itr.prev()
		return
	}
}

// Key follows the y.Iterator interface
func (itr *Iterator) Key() y.Key {
	return itr.bi.key
}

// Value follows the y.Iterator interface
func (itr *Iterator) Value() (ret y.ValueStruct) {
	ret.Decode(itr.bi.val)
	return
}

// FillValue fill the value struct.
func (itr *Iterator) FillValue(vs *y.ValueStruct) {
	vs.Decode(itr.bi.val)
}

// Next follows the y.Iterator interface
func (itr *Iterator) Next() {
	if !itr.reversed {
		itr.next()
	} else {
		itr.prev()
	}
}

// Rewind follows the y.Iterator interface
func (itr *Iterator) Rewind() {
	if !itr.reversed {
		itr.seekToFirst()
	} else {
		itr.seekToLast()
	}
}

// Seek follows the y.Iterator interface
func (itr *Iterator) Seek(key y.Key) {
	if !itr.reversed {
		itr.seek(key)
	} else {
		itr.seekForPrev(key)
	}
}

// ConcatIterator concatenates the sequences defined by several iterators.  (It only works with
// TableIterators, probably just because it's faster to not be so generic.)
type ConcatIterator struct {
	idx      int // Which iterator is active now.
	cur      *Iterator
	iters    []*Iterator // Corresponds to tables.
	tables   []*Table    // Disregarding reversed, this is in ascending order.
	reversed bool
}

// NewConcatIterator creates a new concatenated iterator
func NewConcatIterator(tbls []*Table, reversed bool) *ConcatIterator {
	return &ConcatIterator{
		reversed: reversed,
		iters:    make([]*Iterator, len(tbls)),
		tables:   tbls,
		idx:      -1, // Not really necessary because s.it.Valid()=false, but good to have.
	}
}

func (s *ConcatIterator) setIdx(idx int) {
	s.idx = idx
	if idx < 0 || idx >= len(s.iters) {
		s.cur = nil
	} else {
		if s.iters[s.idx] == nil {
			// We already increased table refs, so init without IncrRef here
			ti := s.tables[s.idx].NewIterator(s.reversed)
			ti.next()
			s.iters[s.idx] = ti
		}
		s.cur = s.iters[s.idx]
	}
}

// Rewind implements y.Interface
func (s *ConcatIterator) Rewind() {
	if len(s.iters) == 0 {
		return
	}
	if !s.reversed {
		s.setIdx(0)
	} else {
		s.setIdx(len(s.iters) - 1)
	}
	s.cur.Rewind()
}

// Valid implements y.Interface
func (s *ConcatIterator) Valid() bool {
	return s.cur != nil && s.cur.Valid()
}

// Key implements y.Interface
func (s *ConcatIterator) Key() y.Key {
	return s.cur.Key()
}

// Value implements y.Interface
func (s *ConcatIterator) Value() y.ValueStruct {
	return s.cur.Value()
}

func (s *ConcatIterator) FillValue(vs *y.ValueStruct) {
	s.cur.FillValue(vs)
}

// Seek brings us to element >= key if reversed is false. Otherwise, <= key.
func (s *ConcatIterator) Seek(key y.Key) {
	var idx int
	if !s.reversed {
		idx = sort.Search(len(s.tables), func(i int) bool {
			return s.tables[i].Biggest().Compare(key) >= 0
		})
	} else {
		n := len(s.tables)
		idx = n - 1 - sort.Search(n, func(i int) bool {
			return s.tables[n-1-i].Smallest().Compare(key) <= 0
		})
	}
	if idx >= len(s.tables) || idx < 0 {
		s.setIdx(-1)
		return
	}
	// For reversed=false, we know s.tables[i-1].Biggest() < key. Thus, the
	// previous table cannot possibly contain key.
	s.setIdx(idx)
	s.cur.Seek(key)
}

// Next advances our concat iterator.
func (s *ConcatIterator) Next() {
	s.cur.Next()
	if s.cur.Valid() {
		// Nothing to do. Just stay with the current table.
		return
	}
	for { // In case there are empty tables.
		if !s.reversed {
			s.setIdx(s.idx + 1)
		} else {
			s.setIdx(s.idx - 1)
		}
		if s.cur == nil {
			// End of list. Valid will become false.
			return
		}
		s.cur.Rewind()
		if s.cur.Valid() {
			break
		}
	}
}
