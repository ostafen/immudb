/*
Copyright 2025 Codenotary Inc. All rights reserved.

SPDX-License-Identifier: BUSL-1.1
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	https://mariadb.com/bsl11/

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tbtree

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/codenotary/immudb/embedded/appendable"
	"github.com/codenotary/immudb/embedded/appendable/multiapp"
	"github.com/codenotary/immudb/embedded/logger"
	"github.com/codenotary/immudb/embedded/multierr"
)

var (
	ErrActiveSnapshots      = errors.New("tree has active snapshots")
	ErrCompactionInProgress = errors.New("compaction in progress")
	ErrStaleRootTimestamp   = errors.New("stale root timestamp")
	ErrInvalidTimestamp     = errors.New("invalid timestamp")
	ErrNoValidPageFound     = errors.New("no valid page found")
	ErrTreeLocked           = errors.New("tree is locked")
)

const (
	MaxEntrySize = 2 * 1024

	HistoryLogFileName = "history"
	TreeLogFileName    = "tree"
)

type (
	TreeID        uint16
	ReadDirFunc   func(path string) ([]os.DirEntry, error)
	AppRemoveFunc func(rootPath, subPath string) error
)

type TBTree struct {
	mtx sync.RWMutex

	path    string
	id      TreeID
	mutated bool

	logger logger.Logger

	wb    *WriteBuffer
	pgBuf *PageBuffer

	rootID         atomic.Uint64
	lastSnapshotID atomic.Uint64

	rootTs         atomic.Uint64
	lastSnapshotTs atomic.Uint64

	indexedEntryCount atomic.Uint32

	headHistoryPageID   PageID
	tailHistoryPageID   PageID
	bufferedHistoryData uint64

	treeApp    appendable.Appendable
	historyApp appendable.Appendable

	numPages   atomic.Uint64
	stalePages atomic.Uint32 // refers to the last persisted snapshot

	nSplits int
	depth   int

	snapshotCount      atomic.Uint64
	maxActiveSnapshots int

	syncThld      int
	unsyncedBytes atomic.Uint32

	fileSize           int
	fileMode           os.FileMode
	appWriteBufferSize int
	readOnly           bool

	compactionThld float32
	compacting     atomic.Bool

	readDirFunc ReadDirFunc
	appFactory  AppFactoryFunc
	appRemove   AppRemoveFunc
}

func Open(
	path string,
	opts *Options,
) (*TBTree, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}

	appOpts := multiapp.DefaultOptions().
		WithReadOnly(opts.readOnly).
		WithRetryableSync(false).
		WithFileSize(opts.fileSize).
		WithFileMode(opts.fileMode).
		WithWriteBufferSize(opts.appWriteBufferSize)

	historyApp, err := opts.appFactory(path, HistoryLogFileName, appOpts.WithFileExt("hx"))
	if err != nil {
		return nil, err
	}

	var t *TBTree
	recoveryAttempts, err := recoverLatestValidTreeSnapshot(path, opts.readDir, func(snapPath string, snapTs uint64) error {
		treeApp, err := opts.appFactory(path, snapPath, appOpts.WithFileExt("t"))
		if err != nil {
			return err
		}

		t, err = OpenWith(path, treeApp, historyApp, opts)
		if err != nil {
			_ = opts.appRemove(path, snapPath)
		}
		return err
	})
	if err != nil {
		return nil, err
	}

	if t != nil {
		return t, nil
	}

	if recoveryAttempts > 0 {
		opts.logger.Warningf(
			"%s: no snapshot could be recovered, attempts=%d",
			t.Path(),
			recoveryAttempts,
		)
	}

	treeApp, err := opts.appFactory(path, TreeLogFileName, appOpts.WithFileExt("t"))
	if err != nil {
		return nil, err
	}

	return OpenWith(
		path,
		treeApp,
		historyApp,
		opts,
	)
}

func OpenWith(
	path string,
	treeApp,
	historyApp appendable.Appendable,
	opts *Options,
) (*TBTree, error) {
	if treeApp == nil || historyApp == nil {
		return nil, ErrIllegalArguments
	}

	if err := opts.Validate(); err != nil {
		return nil, err
	}

	t := &TBTree{
		path:               path,
		logger:             opts.logger,
		id:                 opts.id,
		wb:                 opts.wb,
		pgBuf:              opts.pgBuf,
		treeApp:            treeApp,
		historyApp:         historyApp,
		headHistoryPageID:  PageNone,
		tailHistoryPageID:  PageNone,
		depth:              0,
		mutated:            false,
		maxActiveSnapshots: opts.maxActiveSnapshots,
		fileSize:           opts.fileSize,
		fileMode:           opts.fileMode,
		appWriteBufferSize: opts.appWriteBufferSize,
		syncThld:           opts.syncThld,
		compactionThld:     opts.compactionThld,
		readOnly:           opts.readOnly,
		appFactory:         opts.appFactory,
		appRemove:          opts.appRemove,
		readDirFunc:        opts.readDir,
	}

	err := t.recoverRootPage()
	if errors.Is(err, ErrNoValidPageFound) {
		t.rootID.Store(uint64(PageNone))
		t.lastSnapshotID.Store(uint64(PageNone))
		return t, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

func recoverLatestValidTreeSnapshot(dir string, readDir ReadDirFunc, recoverSnap func(snapPath string, snapTs uint64) error) (int, error) {
	entries, err := readDir(dir)
	if err != nil {
		return 0, err
	}

	recoveryAttempts := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), TreeLogFileName) {
			continue
		}

		parts := strings.Split(e.Name(), "_")
		if len(parts) != 2 {
			continue
		}

		snapTs, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil || parts[0] != TreeLogFileName {
			continue
		}

		if err := recoverSnap(e.Name(), snapTs); err == nil {
			return 0, nil
		}
		recoveryAttempts++
	}
	return recoveryAttempts, nil
}

func (t *TBTree) recoverRootPage() error {
	commitEntry, rootPageOff, err := t.findLastValidPage()
	if errors.Is(err, ErrNoValidPageFound) {
		if err := t.historyApp.SetOffset(0); err != nil {
			return err
		}
		return ErrNoValidPageFound
	}
	if err != nil {
		return err
	}

	var pgBuf [PageSize]byte
	if err := t.readPage(pgBuf[:], PageID(rootPageOff)); err != nil {
		return err
	}

	pg := PageFromBytes(pgBuf[:])
	if !pg.IsRoot() {
		return fmt.Errorf("%w: expected a valid root page", ErrCorruptedTreeLog)
	}

	if err := t.treeApp.SetOffset(rootPageOff + CommitEntrySize); err != nil {
		return err
	}

	if err := t.historyApp.SetOffset(int64(commitEntry.HLogLastEntryOff)); err != nil {
		return err
	}

	rootPageID := PageID(rootPageOff)

	t.rootTs.Store(commitEntry.Ts)
	t.rootID.Store(uint64(rootPageID))
	t.lastSnapshotID.Store(uint64(rootPageID))
	t.lastSnapshotTs.Store(commitEntry.Ts)
	t.indexedEntryCount.Store(commitEntry.IndexedEntryCount)
	t.numPages.Store(commitEntry.TotalPages)
	t.stalePages.Store(commitEntry.StalePages)

	return nil
}

func (t *TBTree) findLastValidPage() (CommitEntry, int64, error) {
	size, err := t.treeApp.Size()
	if err != nil {
		return CommitEntry{}, 0, err
	}

	// search for the latest valid committed entry
	var buf [CommitEntrySize]byte
	for off := size - CommitEntrySize; off >= 0; {
		if _, err := t.treeApp.ReadAt(buf[:], off); err != nil {
			return CommitEntry{}, -1, err
		}

		e, err := readCommitEntry(buf[:])
		if err == nil {
			return e, off, nil
		}

		i := findMagic(buf[:])
		if i >= 0 {
			off -= int64(CommitEntrySize - i - 1)
		} else {
			off -= CommitEntrySize
		}
	}
	return CommitEntry{}, 0, ErrNoValidPageFound
}

func findMagic(buf []byte) int {
	if len(buf) < 2 {
		return -1
	}

	m0 := byte(CommitMagic >> 8)
	m1 := byte(CommitMagic & 0xFF)

	for i := len(buf) - 2; i >= 1; i-- {
		if buf[i] == m1 && buf[i-1] == m0 {
			return i
		}
	}

	if buf[0] == m1 {
		return 0
	}
	return -1
}

func readPageToBuf(pgBuf []byte, r io.ReaderAt, off int64) error {
	var err error

	if off < 0 {
		_, err = r.ReadAt(pgBuf[-int(off):], 0)
	} else {
		_, err = r.ReadAt(pgBuf[:], off)
	}
	return err
}

// Ensures the write buffer maintains the invariant of having at least depth + 2 pages.
// At this point, this guarantees at least 3 free pages are available in the buffer.
func (t *TBTree) canAccommodateWrite() bool {
	return t.wb.Grow(t.depth + 2)
}

func (t *TBTree) Insert(e Entry) error {
	if e.Ts == 0 {
		return fmt.Errorf("%w: timestamp must be greater than zero", ErrInvalidTimestamp)
	}
	if e.Ts < t.Ts() {
		return fmt.Errorf("%w: attempt to insert a value with an older timestamp", ErrInvalidTimestamp)
	}

	if !t.mtx.TryLock() {
		return ErrTreeLocked
	}
	defer t.mtx.Unlock()

	if !t.canAccommodateWrite() {
		return ErrWriteBufferFull
	}

	res, err := t.insert(t.rootPageID(), e, 0)
	if err != nil {
		return err
	}

	if res.split {
		t.nSplits++
		t.depth++

		newRootNode, newRootID, err := t.wb.AllocInnerPage()
		if err != nil {
			return err
		}

		_, err = newRootNode.InsertKey(res.sepKey(), res.splitPageID)
		if err != nil {
			return err
		}
		newRootNode.SetPageID(0, res.newPageID)
		newRootNode.SetAsRoot()

		t.rootID.Store(uint64(newRootID))
	} else {
		t.rootID.Store(uint64(res.newPageID))
	}

	t.rootTs.Store(e.Ts)
	t.mutated = true
	return nil
}

func (t *TBTree) readPage(dst []byte, pgID PageID) error {
	off := int64(pgID) - PageSize
	if off+PageSize-1 < 0 {
		return ErrInvalidPageID
	}

	err := readPageToBuf(dst, t.treeApp, off)
	if err != nil {
		return err
	}
	_, _, err = expandToFixedPage(dst)
	return err
}

func (t *TBTree) dupPage(pgID PageID, dst []byte) error {
	pg, err := t.pgBuf.Get(t.id, pgID, t.readPage)
	if err != nil {
		return err
	}
	defer t.pgBuf.Release(t.id, pgID)

	copy(dst, pg.Bytes())
	return nil
}

func (t *TBTree) Advance(ts uint64, entryCount uint32) error {
	rootTs := t.Ts()
	if ts == t.Ts() {
		return nil
	} else if ts < rootTs {
		return ErrInvalidTimestamp
	}

	// Locking the tree prevents flushers to observe inconsistent (Ts, IndexedEntryCount) pairs.
	if !t.mtx.TryLock() {
		return ErrTreeLocked
	}

	// No need to set mutated = true here because advancing the timestamp
	// does not add any new entries to the tree, so flushing is unnecessary.

	t.rootTs.Store(ts)
	t.indexedEntryCount.Store(entryCount)

	t.mtx.Unlock()

	//t.rootID.Store(uint64(newPageID))

	/*
		rootPageID := t.rootPageID()
		if rootPageID == PageNone {
			pg, newPageID, err = t.wb.AllocLeafPage()
		} else {
			pg, newPageID, err = t.wb.GetOrDup(t.rootPageID(), t.dupPage)
		}
		if err != nil {
			return err
		}*/

	//pg.SetTs(ts)
	//pg.SetIndexedEntryCount(entryCount)
	//pg.SetAsRoot()

	return nil
}

func (t *TBTree) insert(id PageID, e Entry, depth int) (insertResult, error) {
	if id == PageNone {
		return t.insertEmpty(&e)
	}

	pg, newPageID, err := t.wb.GetOrDup(id, t.dupPage)
	if err != nil {
		return insertResult{}, err
	}

	if pg.IsLeaf() {
		return t.insertLeaf(pg, newPageID, e, depth)
	}

	idx, childPageID, err := pg.Find(e.Key)
	if err != nil {
		return insertResult{}, err
	}

	res, err := t.insert(childPageID, e, depth+1)
	if err != nil {
		return insertResult{}, err
	}

	if res.split {
		res, splitted, err := t.insertInnerPage(pg, newPageID, res)
		if splitted || err != nil {
			return res, err
		}
	}
	pg.SetPageID(idx, res.newPageID)

	return insertResult{
		split:     false,
		newPageID: newPageID,
	}, nil
}

func (t *TBTree) insertInnerPage(pg *Page, newPageID PageID, res insertResult) (insertResult, bool, error) {
	sepKey := res.sepKey()
	_, err := pg.InsertKey(sepKey, res.splitPageID)
	if errors.Is(err, ErrPageFull) {
		t.nSplits++

		splitPage, splitPageID, err := t.wb.AllocInnerPage()
		if err != nil {
			return insertResult{}, true, err
		}

		pg.splitInnerPage(
			splitPage,
			sepKey,
			res.splitPageID,
			res.newPageID,
		)

		return insertResult{
			split:       true,
			newPageID:   newPageID,
			splitPageID: splitPageID,
			splitPage:   pg,
		}, true, err
	}
	return insertResult{}, false, nil
}

func (t *TBTree) insertEmpty(e *Entry) (insertResult, error) {
	pg, newPageID, err := t.wb.AllocLeafPage()
	if err != nil {
		return insertResult{}, err
	}

	if _, _, err := pg.InsertEntry(e); err != nil {
		return insertResult{}, err
	}
	pg.SetAsRoot()

	t.depth++

	return insertResult{
		newPageID: newPageID,
	}, nil
}

func (t *TBTree) insertLeaf(pg *Page, pgID PageID, e Entry, depth int) (insertResult, error) {
	prevEntry, replaced, err := pg.InsertEntry(&e)
	if errors.Is(err, ErrPageFull) {
		return t.splitLeafPage(pg, pgID, &e)
	}
	if err != nil {
		return insertResult{}, err
	}

	if replaced {
		hoff, err := t.archiveEntry(&prevEntry)
		if err != nil {
			return insertResult{}, err
		}
		pg.UpdateHistory(e.Key, hoff)
	}

	t.depth = depth

	return insertResult{
		split:     false,
		newPageID: pgID,
	}, nil
}

func (t *TBTree) archiveEntry(e *Entry) (uint64, error) {
	pg, err := t.getCurrHistoryPage()
	if err != nil {
		return OffsetNone, err
	}

	n, err := pg.Append(&HistoryEntry{
		PrevOffset: uint64(e.HOff),
		Ts:         e.Ts,
		Value:      e.Value,
	})
	if errors.Is(err, ErrPageFull) {
		newPage, newPageID, err1 := t.wb.AllocHistoryPage()
		if err1 != nil {
			return OffsetNone, err1
		}
		pg.SetNextPageID(newPageID)
		t.tailHistoryPageID = newPageID

		n, err = newPage.Append(&HistoryEntry{
			PrevOffset: uint64(e.HOff),
			Ts:         e.Ts,
			Value:      e.Value,
		})
	}
	if err != nil {
		return OffsetNone, err
	}

	hoff := t.bufferedHistoryData
	t.bufferedHistoryData += uint64(n)

	return hoff, nil
}

func (t *TBTree) splitLeafPage(pg *Page, pgID PageID, e *Entry) (insertResult, error) {
	t.nSplits++

	splitPage, splitPageID, err := t.wb.AllocLeafPage()
	if err != nil {
		return insertResult{}, err
	}

	prevEntry, err := pg.Remove(e.Key)
	if errors.Is(err, ErrKeyNotFound) {
		err = nil
	}
	if err != nil {
		return insertResult{}, err
	}

	if prevEntry != nil {
		hoff, err := t.archiveEntry(prevEntry)
		if err != nil {
			return insertResult{}, err
		}

		e.HOff = hoff
		e.HC = prevEntry.HC + 1
	}

	pg.splitLeafPage(splitPage, e)

	return insertResult{
		split:       true,
		newPageID:   pgID,
		splitPageID: splitPageID,
		splitPage:   splitPage,
	}, nil
}

func (t *TBTree) getCurrHistoryPage() (*HistoryPage, error) {
	if t.headHistoryPageID == PageNone {
		pg, id, err := t.wb.AllocHistoryPage()
		if err != nil {
			return nil, err
		}
		t.headHistoryPageID = id
		t.tailHistoryPageID = id
		return pg, nil
	}
	return t.wb.GetHistoryPage(t.tailHistoryPageID)
}

type insertResult struct {
	split       bool
	newPageID   PageID
	splitPageID PageID
	splitPage   *Page
}

func (r *insertResult) sepKey() []byte {
	if r.splitPage.IsLeaf() {
		return r.splitPage.firstKey()
	}
	return r.splitPage.keyAt(int(r.splitPage.NumEntries))
}

func (t *TBTree) UseEntry(key []byte, onEntry func(e *Entry) error) error {
	snap, err := t.WriteSnapshot()
	if err != nil {
		return err
	}
	defer snap.Close()

	return snap.UseEntry(key, onEntry)
}

func (t *TBTree) Get(key []byte) (value []byte, ts uint64, hc uint64, err error) {
	snap, err := t.WriteSnapshot()
	if err != nil {
		return nil, 0, 0, err
	}
	defer snap.Close()

	return snap.Get(key)
}

func (t *TBTree) GetWithPrefix(key, neq []byte) ([]byte, []byte, uint64, uint64, error) {
	snap, err := t.WriteSnapshot()
	if err != nil {
		return nil, nil, 0, 0, err
	}
	defer snap.Close()

	return snap.GetWithPrefix(key, neq)
}

func (t *TBTree) GetRevision(key []byte, version int) ([]byte, uint64, error) {
	pg, pgID, err := t.findPage(key, t.lastSnapshotRootID())
	if err != nil {
		return nil, 0, err
	}

	e, err := pg.GetEntry(key)
	if err != nil {
		t.pgBuf.Release(t.id, pgID)
		return nil, 0, err
	}

	if version == int(e.HC)+1 {
		value := cp(e.Value)
		t.pgBuf.Release(t.id, pgID)

		return value, e.Ts, nil
	}
	t.pgBuf.Release(t.id, pgID)

	if version > int(e.HC) {
		return nil, 0, ErrKeyRevisionNotFound
	}

	n := int(e.HC-uint64(version)) + 1
	he, err := t.getRevision(e.HOff, n)
	if err != nil {
		return nil, 0, err
	}
	return he.Value, he.Ts, nil
}

func (t *TBTree) GetBetween(key []byte, initialTs, finalTs uint64) (value []byte, ts uint64, hc uint64, err error) {
	snap, err := t.ReadSnapshot()
	if err != nil {
		return nil, 0, 0, err
	}
	defer snap.Close()

	return snap.GetBetween(key, initialTs, finalTs)
}

type TimedValue struct {
	Value []byte
	Ts    uint64
}

func (tv *TimedValue) Copy() TimedValue {
	return TimedValue{
		Ts:    tv.Ts,
		Value: cp(tv.Value),
	}
}

func (t *TBTree) History(key []byte, offset uint64, descOrder bool, limit int) (timedValues []TimedValue, hCount uint64, err error) {
	snap, err := t.ReadSnapshot()
	if errors.Is(err, ErrNoSnapshotAvailable) {
		return nil, 0, ErrKeyNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	defer snap.Close()

	return snap.History(key, offset, descOrder, limit)
}

func (t *TBTree) WriteSnapshot() (Snapshot, error) {
	t.mtx.RLock()

	snap, err := t.newSnapshot(
		true,
		t.rootPageID(),
		t.Ts(),
	)
	if err != nil {
		t.mtx.RUnlock()
	}
	return snap, err
}

func (t *TBTree) ReadSnapshot() (Snapshot, error) {
	return t.snapshot()
}

func (t *TBTree) snapshot() (Snapshot, error) {
	// TODO: check max number of active snapshots

	ts := t.lastSnapshotTs.Load()

	snapRootID := t.lastSnapshotRootID()
	if snapRootID == PageNone {
		return nil, ErrNoSnapshotAvailable
	}
	assert(!snapRootID.isMemPage(), "should not be a mem page")

	return t.newReadSnapshot(
		snapRootID,
		ts,
	)
}

func (t *TBTree) getRevision(hoff uint64, n int) (HistoryEntry, error) {
	var buf [MaxEntrySize]byte
	for i := 0; i < n-1; i++ {
		_, err := t.historyApp.ReadAt(buf[:8], int64(hoff))
		if err != nil {
			return HistoryEntry{}, err
		}
		hoff = binary.BigEndian.Uint64(buf[:])
	}

	_, err := t.historyApp.ReadAt(buf[:18], int64(hoff))
	if err != nil {
		return HistoryEntry{}, err
	}

	ts := binary.BigEndian.Uint64(buf[8:])
	vlen := binary.BigEndian.Uint16(buf[16:])

	_, err = t.historyApp.ReadAt(buf[:vlen], int64(hoff+18))
	if err != nil {
		return HistoryEntry{}, err
	}

	return HistoryEntry{
		PrevOffset: OffsetNone,
		Ts:         ts,
		Value:      buf[:vlen],
	}, nil
}

func (t *TBTree) findPage(key []byte, id PageID) (*Page, PageID, error) {
	page, err := t.pgBuf.Get(t.id, id, t.readPage)
	if err != nil {
		return nil, PageNone, err
	}

	if page.IsLeaf() {
		return page, id, nil
	}

	_, childPageID, err := page.Find(key)
	t.pgBuf.Release(t.id, id)

	if err != nil {
		return nil, PageNone, err
	}
	return t.findPage(key, childPageID)
}

func (t *TBTree) rootPageID() PageID {
	return PageID(t.rootID.Load())
}

func (t *TBTree) lastSnapshotRootID() PageID {
	return PageID(t.lastSnapshotID.Load())
}

func (t *TBTree) Flush() error {
	t.mtx.Lock()
	defer t.mtx.Unlock()

	return t.flushToTreeLog()
}

func (t *TBTree) FlushReset() error {
	t.mtx.Lock()
	defer t.mtx.Unlock()

	err := t.flushToTreeLog()
	t.wb.Reset()
	return err
}

func (t *TBTree) TryFlush() error {
	if !t.mtx.TryLock() {
		return ErrTreeLocked
	}
	defer t.mtx.Unlock()

	return t.flushToTreeLog()
}

func (t *TBTree) flushToTreeLog() error {
	if !t.mutated {
		t.logger.Infof("flushing not needed. exiting...")
		return nil
	}

	hLogBytesWritten, res, err := t.flushTo(t.treeApp)
	if err != nil {
		return err
	}

	t.rootID.Store(uint64(res.rootID))
	t.lastSnapshotID.Store(uint64(res.rootID))
	t.lastSnapshotTs.Store(t.Ts())

	t.headHistoryPageID = PageNone
	t.tailHistoryPageID = PageNone
	t.mutated = false

	t.maybeSync(uint32(res.bytesWritten + hLogBytesWritten))
	return nil
}

func (t *TBTree) flushTo(dstApp appendable.Appendable) (int, flushRes, error) {
	t.logger.Infof("starting flushing, index=%s, ts=%d", t.path, t.Ts())

	// TODO: should get checksum of last history entry?

	hLogBytesWritten, hlogLastEntryChecksum, hLogLastEntryOff, err := t.flushHistory()
	if err != nil {
		return -1, flushRes{}, err
	}

	opts := flushOptions{
		dstApp: dstApp,
	}
	res, err := t.flushTreeLog(t.rootPageID(), opts)
	if err != nil {
		return -1, flushRes{}, err
	}

	t.stalePages.Add(res.stalePages)
	t.numPages.Add(uint64(res.pagesFlushed))

	ts := t.Ts()
	commitEntry := CommitEntry{
		Ts:                    ts,
		HLogLastEntryOff:      hLogLastEntryOff,
		HLogLastEntryChecksum: hlogLastEntryChecksum,
		TotalPages:            uint64(res.pagesFlushed),
		StalePages:            t.stalePages.Load(),
		IndexedEntryCount:     t.IndexedEntryCount(),
	}
	if err := commit(&commitEntry, opts.dstApp); err != nil {
		return -1, flushRes{}, err
	}

	t.logger.Infof("flushing completed, index=%s", t.path)
	return hLogBytesWritten, res, nil
}

func (t *TBTree) maybeSync(n uint32) {
	if t.unsyncedBytes.Add(n) < uint32(t.syncThld) {
		return
	}

	go func() {
		// Prevent compaction to swap the treeApp file during sync
		t.snapshotCount.Add(1)

		err := t.historyApp.Sync()
		if err != nil {
			t.logger.Warningf("%w: unable to sync history log, path=%s", err, t.path)
		}

		err = t.treeApp.Sync()
		if err != nil {
			t.logger.Warningf("%w: unable to sync tree log, path=%s", err, t.path)
		}
		t.unsyncedBytes.Store(0)

		t.snapshotCount.Add(^uint64(0))
	}()
}

func (t *TBTree) flushHistory() (int, uint32, uint64, error) {
	currPage := t.headHistoryPageID
	if currPage == PageNone {
		// TODO: save last entry checksum and return it
		return 0, 0, 0, nil
	}

	var lastWrittenEntry []byte
	var lastEntryOff int64

	var n int
	for currPage != PageNone {
		hp, err := t.wb.GetHistoryPage(currPage)
		if err != nil {
			return -1, 0, 0, err
		}

		data := hp.Data()
		lastEntryOff, _, err = t.historyApp.Append(data)
		if err != nil {
			return -1, 0, 0, err
		}
		lastWrittenEntry = data

		n += len(data)

		next := hp.NextPageID()
		currPage = next
	}

	t.headHistoryPageID = PageNone
	err := t.historyApp.Flush()
	if err != nil {
		return -1, 0, 0, err
	}

	// TODO: save to tree
	checksum := computeChecksum(lastWrittenEntry)
	return n, checksum, uint64(lastEntryOff), err
}

type flushRes struct {
	pagesFlushed int
	bytesWritten int
	rootID       PageID
	stalePages   uint32
}

type flushOptions struct {
	fullDump bool
	dstApp   appendable.Appendable
}

func (t *TBTree) flushTreeLog(pageID PageID, opts flushOptions) (flushRes, error) {
	isMemPage := pageID.isMemPage()
	if !isMemPage && !opts.fullDump {
		return flushRes{}, fmt.Errorf("attempted to flush a non in memory page")
	}

	pg, err := t.getWritePage(pageID)
	if err != nil {
		return flushRes{rootID: PageNone}, err
	}

	var stalePages uint32
	if pg.IsLeaf() {
		if isMemPage && pg.IsCopied() {
			stalePages++
		}

		n, pgID, err := t.appendPage(pg, opts.dstApp)
		if err != nil {
			return flushRes{rootID: PageNone}, err
		}

		return flushRes{
			pagesFlushed: 1,
			bytesWritten: n,
			rootID:       pgID,
			stalePages:   stalePages,
		}, err
	}

	var pagesFlushed int
	var totalBytesWritten int

	for i := 0; i < int(pg.NumEntries); i++ {
		childPageID := pg.ChildPageAt(i)
		if !childPageID.isMemPage() && !opts.fullDump {
			continue
		}

		res, err := t.flushTreeLog(childPageID, opts)
		if err != nil {
			return flushRes{rootID: PageNone}, err
		}
		pg.SetPageID(i, res.rootID)

		stalePages += res.stalePages
		totalBytesWritten += res.bytesWritten
		pagesFlushed += res.pagesFlushed
	}

	if isMemPage && pg.IsCopied() {
		stalePages++
	}

	n, pgID, err := t.appendPage(pg, opts.dstApp)
	return flushRes{
		pagesFlushed: pagesFlushed + 1,
		bytesWritten: totalBytesWritten + n,
		rootID:       pgID,
		stalePages:   stalePages,
	}, err
}

func (t *TBTree) appendPage(pg *Page, tlog appendable.Appendable) (int, PageID, error) {
	writeOffset, err := tlog.Size()
	if err != nil {
		return 0, PageNone, err
	}

	// TODO: move to t's fields
	var buf [PageSize]byte
	n := pg.Put(buf[:])

	if _, _, err := tlog.Append(buf[:n]); err != nil {
		return 0, PageNone, err
	}

	// TODO: move written page to cache, but without triggering eviction
	return n, PageID(writeOffset) + PageID(n), nil
}

func (t *TBTree) getPage(pgID PageID) (*Page, error) {
	if pgID.isMemPage() {
		return t.wb.Get(pgID)
	}
	return t.pgBuf.Get(t.id, pgID, t.readPage)
}

func (t *TBTree) release(pgID PageID) {
	if !pgID.isMemPage() {
		t.pgBuf.Release(t.id, pgID)
	}
}

func (t *TBTree) getWritePage(pgID PageID) (*Page, error) {
	if pgID.isMemPage() {
		return t.wb.Get(pgID)
	}

	// TODO: remove this allocation
	var pgCopy Page
	err := t.pgBuf.UsePage(t.id, pgID, t.readPage, func(pg *Page) error {
		pgCopy = *pg
		return nil
	})
	return &pgCopy, err
}

func (t *TBTree) ID() TreeID {
	return t.id
}

func (t *TBTree) Ts() uint64 {
	return t.rootTs.Load()
}

func (t *TBTree) IndexedEntryCount() uint32 {
	return t.indexedEntryCount.Load()
}

func (t *TBTree) StalePages() uint32 {
	return t.stalePages.Load()
}

func (t *TBTree) NumPages() uint64 {
	return t.numPages.Load()
}

func (t *TBTree) StalePagePercentage() float32 {
	stalePages := t.StalePages()
	numPages := t.NumPages()
	if numPages == 0 {
		return 0
	}
	return float32(stalePages) / float32(numPages)
}

func (t *TBTree) SnapshotAtTs(ctx context.Context, ts uint64) (Snapshot, error) {
	snapRootID, snapTs, err := t.ensureLatestSnapshotContainsTs(ctx, ts)
	if err != nil {
		return nil, err
	}

	snapAtTs := ts
	if snapAtTs == 0 {
		snapAtTs = snapTs
	}

	return t.newReadSnapshot(
		snapRootID,
		snapAtTs,
	)
}

func (t *TBTree) SnapshotMustIncludeTs(ctx context.Context, ts uint64) (Snapshot, error) {
	snapRootID, snapTs, err := t.ensureLatestSnapshotContainsTs(ctx, ts)
	if err != nil {
		return nil, err
	}

	return t.newReadSnapshot(
		snapRootID,
		snapTs,
	)
}

func (t *TBTree) ensureLatestSnapshotContainsTs(
	ctx context.Context,
	ts uint64,
) (PageID, uint64, error) {
	// TODO: should only attempt locking when t.lastSnapshotTs < txID

	t.mtx.Lock()
	defer t.mtx.Unlock()

	lastSnapRootID := t.lastSnapshotRootID()
	flushNeeded := lastSnapRootID == PageNone || (ts > 0 && t.lastSnapshotTs.Load() < ts)

	if rootTs := t.Ts(); rootTs < ts {
		return PageNone, 0, fmt.Errorf("%w: root timestamp (%d) must be >= %d", ErrStaleRootTimestamp, rootTs, ts)
	}

	if flushNeeded {
		err := t.flushToTreeLog()
		if err != nil {
			return PageNone, 0, err
		}
	}
	return t.lastSnapshotRootID(), t.lastSnapshotTs.Load(), nil
}

func (tb *TBTree) Path() string {
	return tb.path
}

func (t *TBTree) Close() error {
	t.mtx.Lock()
	defer t.mtx.Unlock()

	if t.ActiveSnapshots() > 0 {
		return ErrActiveSnapshots
	}

	if err := t.flushToTreeLog(); err != nil {
		return err
	}

	return multierr.NewMultiErr().
		Append(t.historyApp.Sync()).
		Append(t.treeApp.Sync()).
		Append(t.historyApp.Close()).
		Append(t.treeApp.Close()).
		Reduce()
}

func (t *TBTree) ActiveSnapshots() int {
	return int(t.snapshotCount.Load())
}

const CommitEntrySize = 40 + CommitMagicSize

func commit(e *CommitEntry, app appendable.Appendable) error {
	var buf [CommitEntrySize]byte
	putCommitEntry(e, buf[:])

	_, _, err := app.Append(buf[:])
	if err != nil {
		return err
	}
	return app.Flush()
}

func putCommitEntry(e *CommitEntry, buf []byte) {
	off := 0

	binary.BigEndian.PutUint32(buf[off:], 0)
	off += 4

	binary.BigEndian.PutUint64(buf[off:], e.Ts)
	off += 8

	binary.BigEndian.PutUint64(buf[off:], e.HLogLastEntryOff)
	off += 8

	binary.BigEndian.PutUint32(buf[off:], e.HLogLastEntryChecksum)
	off += 4

	binary.BigEndian.PutUint64(buf[off:], e.TotalPages)
	off += 8

	binary.BigEndian.PutUint32(buf[off:], e.StalePages)
	off += 4

	binary.BigEndian.PutUint32(buf[off:], e.IndexedEntryCount)
	off += 4

	binary.BigEndian.PutUint16(buf[off:], CommitMagic)

	checksum := computeChecksum(buf[ChecksumSize:])
	binary.BigEndian.PutUint32(buf[:], checksum)
}

func readCommitEntry(buf []byte) (CommitEntry, error) {
	assert(len(buf) == CommitEntrySize, "buf is too small")

	var e CommitEntry

	off := 0

	e.Checksum = binary.BigEndian.Uint32(buf[off:])
	off += 4

	e.Ts = binary.BigEndian.Uint64(buf[off:])
	off += 8

	e.HLogLastEntryOff = binary.BigEndian.Uint64(buf[off:])
	off += 8

	e.HLogLastEntryChecksum = binary.BigEndian.Uint32(buf[off:])
	off += 4

	e.TotalPages = binary.BigEndian.Uint64(buf[off:])
	off += 8

	e.StalePages = binary.BigEndian.Uint32(buf[off:])
	off += 4

	e.IndexedEntryCount = binary.BigEndian.Uint32(buf[off:])

	if computeChecksum(buf[ChecksumSize:]) != e.Checksum {
		return e, fmt.Errorf("%w: commit entry checksum doesn't match", ErrCorruptedEntry)
	}
	return e, nil
}

func computeChecksum(data []byte) uint32 {
	crc := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	crc.Write(data)
	return crc.Sum32()
}
