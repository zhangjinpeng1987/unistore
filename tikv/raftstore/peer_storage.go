package raftstore

import (
	"bytes"
	"fmt"
	"math"
	"time"

	"github.com/coocood/badger"
	"github.com/coocood/badger/y"
	"github.com/coreos/etcd/raft"
	"github.com/coreos/etcd/raft/raftpb"
	"github.com/cznic/mathutil"
	"github.com/golang/protobuf/proto"
	"github.com/ngaut/log"
	"github.com/pingcap/errors"
	"github.com/pingcap/kvproto/pkg/eraftpb"
	"github.com/pingcap/kvproto/pkg/metapb"
	rspb "github.com/pingcap/kvproto/pkg/raft_serverpb"
)

const (
	// When we create a region peer, we should initialize its log term/index > 0,
	// so that we can force the follower peer to sync the snapshot first.
	RaftInitLogTerm  = 5
	RaftInitLogIndex = 5

	raftLogMultiGetCnt = 8

	MaxCacheCapacity = 1024 - 1
)

// CompactRaftLog discards all log entries prior to compact_index. We must guarantee
// that the compact_index is not greater than applied index.
func CompactRaftLog(tag string, state *rspb.RaftApplyState, compactIndex, compactTerm uint64) error {
	log.Debugf("%s compact log entries to prior to %d", tag, compactIndex)

	if compactIndex <= state.TruncatedState.GetIndex() {
		return errors.New("try to truncate compacted entries")
	} else if compactIndex > state.GetAppliedIndex() {
		return errors.Errorf("compact index %d > applied index %d", compactIndex, state.GetAppliedIndex())
	}

	// we don't actually delete the logs now, we add an async task to do it.
	state.TruncatedState.Index = compactIndex
	state.TruncatedState.Term = compactTerm
	return nil
}

type EntryCache struct {
	cache []*raftpb.Entry
}

func (ec *EntryCache) front() *raftpb.Entry {
	return ec.cache[0]
}

func (ec *EntryCache) back() *raftpb.Entry {
	return ec.cache[len(ec.cache)-1]
}

func (ec *EntryCache) length() int {
	return len(ec.cache)
}

func (ec *EntryCache) fetchEntriesTo(begin, end, maxSize uint64, fetchSize *uint64, ents []raftpb.Entry) []raftpb.Entry {
	if begin >= end {
		return nil
	}
	y.Assert(ec.length() > 0)
	cacheLow := ec.front().Index
	y.Assert(begin >= cacheLow)
	cacheStart := int(begin - cacheLow)
	cacheEnd := int(end - cacheLow)
	if cacheEnd > ec.length() {
		cacheEnd = ec.length()
	}
	for i := cacheStart; i < cacheEnd; i++ {
		entry := ec.cache[i]
		y.AssertTruef(entry.Index == cacheLow+uint64(i), "%d %d %d", entry.Index, cacheLow, i)
		entrySize := uint64(entry.Size())
		*fetchSize += uint64(entrySize)
		if *fetchSize != entrySize && *fetchSize > maxSize {
			break
		}
		ents = append(ents, *entry)
	}
	return ents
}

func (ec *EntryCache) append(tag string, entries []*raftpb.Entry) {
	if len(entries) == 0 {
		return
	}
	if ec.length() > 0 {
		firstIndex := entries[0].Index
		cacheLastIndex := ec.back().Index
		if cacheLastIndex >= firstIndex {
			if ec.front().Index >= firstIndex {
				ec.cache = ec.cache[:0]
			} else {
				left := ec.length() - int(cacheLastIndex-firstIndex+1)
				ec.cache = ec.cache[:left]
			}
		} else if cacheLastIndex+1 < firstIndex {
			panic(fmt.Sprintf("%s unexpected hole %d < %d", tag, cacheLastIndex, firstIndex))
		}
	}
	ec.cache = append(ec.cache, entries...)
	if ec.length() > MaxCacheCapacity {
		extraSize := ec.length() - MaxCacheCapacity
		ec.cache = ec.cache[extraSize:]
	}
}

func (ec *EntryCache) compactTo(idx uint64) {
	if ec.length() == 0 {
		return
	}
	firstIdx := ec.front().Index
	if firstIdx > idx {
		return
	}
	pos := mathutil.Min(int(idx-firstIdx), ec.length())
	ec.cache = ec.cache[pos:]
}

type InvokeContext struct {
	RegionID   uint64
	RaftState  rspb.RaftLocalState
	ApplyState rspb.RaftApplyState
	lastTerm   uint64
	SnapRegion *metapb.Region
}

func NewInvokeContext(store *PeerStorage) *InvokeContext {
	ctx := &InvokeContext{
		RegionID:   store.region.GetId(),
		RaftState:  *store.raftState,
		ApplyState: *store.applyState,
		lastTerm:   store.lastTerm,
	}
	*ctx.RaftState.HardState = *store.raftState.HardState
	*ctx.ApplyState.TruncatedState = *store.applyState.TruncatedState
	return ctx
}

func (ic *InvokeContext) hasSnapshot() bool {
	return ic.SnapRegion != nil
}

func (ic *InvokeContext) saveRaftStateTo(wb *WriteBatch) error {
	key := RaftStateKey(ic.RegionID)
	return wb.SetMsg(key, &ic.RaftState)
}

func (ic *InvokeContext) saveApplyStateTo(wb *WriteBatch) error {
	key := ApplyStateKey(ic.RegionID)
	return wb.SetMsg(key, &ic.ApplyState)
}

type HandleRaftReadyContext interface {
	KVWB() *WriteBatch
	RaftWB() *WriteBatch
	SyncLog() bool
	SetSyncLog(sync bool)
}

var _ raft.Storage = new(PeerStorage)

type PeerStorage struct {
	Engines *Engines

	region           *metapb.Region
	raftState        *rspb.RaftLocalState
	applyState       *rspb.RaftApplyState
	appliedIndexTerm uint64
	lastTerm         uint64

	cache *EntryCache
	stats *CacheQueryStats

	Tag string
}

func NewPeerStorage(engines *Engines, region *metapb.Region, tag string) (*PeerStorage, error) {
	log.Debugf("%s creating storage for %s", tag, region.String())
	raftState, err := initRaftState(engines.raft, region)
	if err != nil {
		return nil, err
	}
	applyState, err := initApplyState(engines.kv, region)
	if err != nil {
		return nil, err
	}
	if raftState.LastIndex < applyState.AppliedIndex {
		panic(fmt.Sprintf("%s unexpected raft log index: lastIndex %d < appliedIndex %d",
			tag, raftState.LastIndex, applyState.AppliedIndex))
	}
	lastTerm, err := initLastTerm(engines.raft, region, raftState, applyState)
	if err != nil {
		return nil, err
	}
	return &PeerStorage{
		Engines:    engines,
		region:     region,
		Tag:        tag,
		raftState:  raftState,
		applyState: applyState,
		lastTerm:   lastTerm,
		cache:      &EntryCache{},
		stats:      &CacheQueryStats{},
	}, nil
}

func getMsg(engine *badger.DB, key []byte, msg proto.Message) error {
	return engine.View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if err != nil {
			return err
		}
		val, err := item.Value()
		if err != nil {
			return err
		}
		return proto.Unmarshal(val, msg)
	})
}

func putMsg(engine *badger.DB, key []byte, msg proto.Message) error {
	return engine.Update(func(txn *badger.Txn) error {
		val, err := proto.Marshal(msg)
		if err != nil {
			return err
		}
		return txn.Set(key, val)
	})
}

func initRaftState(raftEngine *badger.DB, region *metapb.Region) (*rspb.RaftLocalState, error) {
	stateKey := RaftStateKey(region.Id)
	raftState := new(rspb.RaftLocalState)
	err := getMsg(raftEngine, stateKey, raftState)
	if err != nil && err != badger.ErrKeyNotFound {
		return nil, err
	}
	if err == badger.ErrKeyNotFound {
		if len(region.Peers) > 0 {
			// new split region
			raftState.LastIndex = RaftInitLogIndex
			raftState.HardState = &eraftpb.HardState{
				Term:   RaftInitLogTerm,
				Commit: RaftInitLogIndex,
			}
			putMsg(raftEngine, stateKey, raftState)
		}
	}
	return raftState, nil
}

func initApplyState(kvEngine *badger.DB, region *metapb.Region) (*rspb.RaftApplyState, error) {
	key := ApplyStateKey(region.Id)
	applyState := new(rspb.RaftApplyState)
	err := getMsg(kvEngine, key, applyState)
	if err != nil && err != badger.ErrKeyNotFound {
		return nil, err
	}
	if err == badger.ErrKeyNotFound {
		if len(region.Peers) > 0 {
			applyState.AppliedIndex = RaftInitLogIndex
			applyState.TruncatedState = &rspb.RaftTruncatedState{
				Index: RaftInitLogIndex,
				Term:  RaftInitLogTerm,
			}
		}
	}
	return applyState, nil
}

func initLastTerm(raftEngine *badger.DB, region *metapb.Region,
	raftState *rspb.RaftLocalState, applyState *rspb.RaftApplyState) (uint64, error) {
	lastIdx := raftState.LastIndex
	if lastIdx == 0 {
		return 0, nil
	} else if lastIdx == RaftInitLogIndex {
		return RaftInitLogTerm, nil
	} else if lastIdx == applyState.TruncatedState.GetIndex() {
		return applyState.TruncatedState.GetTerm(), nil
	} else {
		y.Assert(lastIdx > RaftInitLogIndex)
	}
	lastLogKey := RaftLogKey(region.Id, lastIdx)
	e := new(raftpb.Entry)
	err := getMsg(raftEngine, lastLogKey, e)
	if err != nil {
		return 0, errors.Errorf("[region %s] entry at %d doesn't exist, may lost data.", region, lastIdx)
	}
	return e.Term, nil
}

func (ps *PeerStorage) InitialState() (raftpb.HardState, raftpb.ConfState, error) {
	hardState := ps.raftState.HardState
	if hardState.Commit == 0 && hardState.Term == 0 && hardState.Vote == 0 {
		y.AssertTruef(!ps.isInitialized(),
			"peer for region %s is initialized but local state %s has empty hard state",
			ps.region, ps.raftState)
		return raftpb.HardState{}, raftpb.ConfState{}, nil
	}
	return raftpb.HardState{
		Term:   hardState.Term,
		Vote:   hardState.Vote,
		Commit: hardState.Commit,
	}, confStateFromRegion(ps.region), nil
}

func confStateFromRegion(region *metapb.Region) (confState raftpb.ConfState) {
	for _, p := range region.Peers {
		if p.IsLearner {
			confState.Learners = append(confState.Learners, p.GetId())
		} else {
			confState.Nodes = append(confState.Nodes, p.GetId())
		}
	}
	return
}

func (ps *PeerStorage) isInitialized() bool {
	return len(ps.region.Peers) > 0
}

func (ps *PeerStorage) Entries(low, high, maxSize uint64) ([]raftpb.Entry, error) {
	err := ps.checkRange(low, high)
	if err != nil {
		return nil, err
	}
	ents := make([]raftpb.Entry, 0, high-low)
	if low == high {
		return ents, nil
	}
	cacheLow := uint64(math.MaxUint64)
	if ps.cache.length() > 0 {
		cacheLow = ps.cache.front().Index
	}
	reginID := ps.region.Id
	if high <= cacheLow {
		// not overlap
		ps.stats.miss++
		ents, _, err = fetchEntriesTo(ps.Engines.raft, reginID, low, high, maxSize, ents)
		if err != nil {
			return ents, err
		}
		return ents, nil
	}
	var fetchedSize, beginIdx uint64
	if low < cacheLow {
		ps.stats.miss++
		ents, fetchedSize, err = fetchEntriesTo(ps.Engines.raft, reginID, low, cacheLow, maxSize, ents)
		if fetchedSize > maxSize {
			// maxSize exceed.
			return ents, nil
		}
		beginIdx = cacheLow
	} else {
		beginIdx = low
	}
	ps.stats.hit++
	return ps.cache.fetchEntriesTo(beginIdx, high, maxSize, &fetchedSize, ents), nil
}

func (ps *PeerStorage) Term(idx uint64) (uint64, error) {
	if idx == ps.truncatedIndex() {
		return ps.truncatedTerm(), nil
	}
	err := ps.checkRange(idx, idx+1)
	if err != nil {
		return 0, err
	}
	if ps.truncatedTerm() == ps.lastTerm || idx == ps.raftState.LastIndex {
		return ps.lastTerm, nil
	}
	entries, err := ps.Entries(idx, idx+1, math.MaxUint64)
	if err != nil {
		return 0, err
	}
	return entries[0].Term, nil
}

func (ps *PeerStorage) checkRange(low, high uint64) error {
	if low > high {
		return errors.Errorf("low %d is greater than high %d", low, high)
	} else if low <= ps.truncatedIndex() {
		return raft.ErrCompacted
	} else if high > ps.raftState.LastIndex+1 {
		return errors.Errorf("entries' high %d is out of bound, lastIndex %d",
			high, ps.raftState.LastIndex)
	}
	return nil
}

func (ps *PeerStorage) truncatedIndex() uint64 {
	return ps.applyState.TruncatedState.GetIndex()
}

func (ps *PeerStorage) truncatedTerm() uint64 {
	return ps.applyState.TruncatedState.GetTerm()
}

func (ps *PeerStorage) LastIndex() (uint64, error) {
	return ps.raftState.LastIndex, nil
}

func (ps *PeerStorage) FirstIndex() (uint64, error) {
	return firstIndex(ps.applyState), nil
}

func firstIndex(applyState *rspb.RaftApplyState) uint64 {
	return applyState.TruncatedState.GetIndex() + 1
}

func (ps *PeerStorage) Snapshot() (raftpb.Snapshot, error) {
	return raftpb.Snapshot{}, raft.ErrSnapshotTemporarilyUnavailable
}

// Append the given entries to the raft log using previous last index or self.last_index.
// Return the new last index for later update. After we commit in engine, we can set last_index
// to the return one.
func (ps *PeerStorage) Append(invokeCtx *InvokeContext, entries []*raftpb.Entry, readyCtx HandleRaftReadyContext) error {
	log.Debugf("%s append %d entries", ps.Tag, len(entries))
	prevLastIndex := invokeCtx.RaftState.GetLastIndex()
	if len(entries) == 0 {
		return nil
	}
	lastEntry := entries[len(entries)-1]
	lastIndex := lastEntry.Index
	lastTerm := lastEntry.Term
	for _, entry := range entries {
		if !readyCtx.SyncLog() {
			readyCtx.SetSyncLog(getSyncLogFromEntry(entry))
		}
		err := readyCtx.RaftWB().SetMsg(RaftLogKey(ps.region.Id, entry.Index), entry)
		if err != nil {
			return err
		}
	}
	// Delete any previously appended log entries which never committed.
	for i := lastIndex + 1; i <= prevLastIndex; i++ {
		readyCtx.RaftWB().Delete(RaftLogKey(ps.region.Id, i))
	}
	invokeCtx.RaftState.LastIndex = lastIndex
	invokeCtx.lastTerm = lastTerm

	// TODO: if the writebatch is failed to commit, the cache will be wrong.
	ps.cache.append(ps.Tag, entries)
	return nil
}

func (ps *PeerStorage) CompactTo(idx uint64) {
	ps.cache.compactTo(idx)
}

func (ps *PeerStorage) clearMeta(kvWB, raftWB *WriteBatch) error {
	return ClearMeta(ps.Engines, kvWB, raftWB, ps.region.Id, ps.raftState)
}

type CacheQueryStats struct {
	hit  uint64
	miss uint64
}

func getSyncLogFromEntry(entry *raftpb.Entry) bool {
	return entryCtx(entry.Data[len(entry.Data)-1]).IsSyncLog()
}

func fetchEntriesTo(engine *badger.DB, regionID, low, high, maxSize uint64, buf []raftpb.Entry) ([]raftpb.Entry, uint64, error) {
	var totalSize uint64
	nextIndex := low
	exceededMaxSize := false
	txn := engine.NewTransaction(false)
	defer txn.Discard()
	if high-low <= raftLogMultiGetCnt {
		// If election happens in inactive regions, they will just try
		// to fetch one empty log.
		for i := low; i < high; i++ {
			key := RaftLogKey(regionID, i)
			item, err := txn.Get(key)
			if err == badger.ErrKeyNotFound {
				return nil, 0, raft.ErrUnavailable
			} else if err != nil {
				return nil, 0, err
			}
			val, err := item.Value()
			if err != nil {
				return nil, 0, err
			}
			var entry raftpb.Entry
			err = entry.Unmarshal(val)
			if err != nil {
				return nil, 0, err
			}
			y.Assert(entry.Index == i)
			totalSize += uint64(len(val))

			if len(buf) == 0 || totalSize <= maxSize {
				buf = append(buf, entry)
			}
			if totalSize > maxSize {
				break
			}
		}
		return buf, totalSize, nil
	}
	startKey := RaftLogKey(regionID, low)
	endKey := RaftLogKey(regionID, high)
	opt := badger.DefaultIteratorOptions
	opt.StartKey = startKey
	opt.EndKey = endKey
	opt.PrefetchValues = false
	iter := txn.NewIterator(opt)
	defer iter.Close()
	for iter.Seek(startKey); iter.Valid(); iter.Next() {
		item := iter.Item()
		if bytes.Compare(item.Key(), endKey) >= 0 {
			break
		}
		val, err := item.Value()
		if err != nil {
			return nil, 0, err
		}
		var entry raftpb.Entry
		err = entry.Unmarshal(val)
		if err != nil {
			return nil, 0, err
		}
		// May meet gap or has been compacted.
		if entry.Index != nextIndex {
			break
		}
		nextIndex++
		totalSize += uint64(len(val))
		exceededMaxSize = totalSize > maxSize
		if !exceededMaxSize || len(buf) == 0 {
			buf = append(buf, entry)
		}
		if exceededMaxSize {
			break
		}
	}
	// If we get the correct number of entries, returns,
	// or the total size almost exceeds max_size, returns.
	if len(buf) == int(high-low) || exceededMaxSize {
		return buf, totalSize, nil
	}
	// Here means we don't fetch enough entries.
	return nil, 0, raft.ErrUnavailable
}

func ClearMeta(engines *Engines, kvWB, raftWB *WriteBatch, regionID uint64, raftState *rspb.RaftLocalState) error {
	start := time.Now()
	kvWB.Delete(RegionStateKey(regionID))
	kvWB.Delete(ApplyStateKey(regionID))

	lastIndex := raftState.LastIndex
	firstIndex := lastIndex + 1
	beginLogKey := RaftLogKey(regionID, 0)
	endLogKey := RaftLogKey(regionID, firstIndex)
	err := engines.raft.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		it.Seek(beginLogKey)
		if it.Valid() && bytes.Compare(it.Item().Key(), endLogKey) < 0 {
			logIdx, err1 := RaftLogIndex(it.Item().Key())
			if err1 != nil {
				return err1
			}
			firstIndex = logIdx
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := firstIndex; i <= lastIndex; i++ {
		raftWB.Delete(RaftLogKey(regionID, i))
	}
	raftWB.Delete(RaftStateKey(regionID))
	log.Infof(
		"[region %d] clear peer 1 meta key 1 apply key 1 raft key and %d raft logs, takes %v",
		regionID,
		lastIndex+1-firstIndex,
		time.Since(start),
	)
	return nil
}
