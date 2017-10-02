// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"errors"
	"sort"
	"time"

	"github.com/eapache/channels"
	"github.com/keybase/client/go/logger"
	"github.com/keybase/kbfs/kbfsblock"
	"github.com/keybase/kbfs/tlf"
	"golang.org/x/net/context"
)

const (
	fileIndirectBlockPrefetchPriority int           = -100
	dirEntryPrefetchPriority          int           = -200
	updatePointerPrefetchPriority     int           = 0
	defaultPrefetchPriority           int           = -1024
	prefetchTimeout                   time.Duration = 15 * time.Minute
	maxNumPrefetches                  int           = 10000
)

type prefetcherConfig interface {
	syncedTlfGetterSetter
	dataVersioner
	logMaker
	blockCacher
	diskBlockCacheGetter
}

type prefetchRequest struct {
	ptr            BlockPointer
	block          Block
	kmd            KeyMetadata
	priority       int
	lifetime       BlockCacheLifetime
	prefetchStatus PrefetchStatus
	isDeepSync     bool
}

type prefetch struct {
	subtreeBlockCount int
	subtreeTriggered  bool
	req               *prefetchRequest
	parents           map[kbfsblock.ID]bool
}

type blockPrefetcher struct {
	config prefetcherConfig
	log    logger.Logger
	// blockRetriever to retrieve blocks from the server
	retriever BlockRetriever
	// channel to synchronize prefetch requests with the prefetcher shutdown
	prefetchRequestCh channels.Channel
	// channel that is idempotently closed when a shutdown occurs
	shutdownCh chan struct{}
	// channel that is closed when all current fetches are done and prefetches
	// have been triggered
	almostDoneCh chan struct{}
	// channel that is closed when a shutdown completes and all pending
	// prefetch requests are complete
	doneCh chan struct{}
	// channels to track the success or failure of prefetches
	prefetchCancelCh channels.Channel
	// map to store prefetch metadata
	prefetches map[kbfsblock.ID]*prefetch
	// channel to allow synchronization on completion
	inFlightFetches channels.Channel
}

var _ Prefetcher = (*blockPrefetcher)(nil)

func newBlockPrefetcher(retriever BlockRetriever,
	config prefetcherConfig, testSyncCh <-chan struct{}) *blockPrefetcher {
	p := &blockPrefetcher{
		config:            config,
		retriever:         retriever,
		prefetchRequestCh: channels.NewInfiniteChannel(),
		prefetchCancelCh:  channels.NewInfiniteChannel(),
		shutdownCh:        make(chan struct{}),
		almostDoneCh:      make(chan struct{}, 1),
		doneCh:            make(chan struct{}),
		prefetches:        make(map[kbfsblock.ID]*prefetch),
		inFlightFetches:   channels.NewInfiniteChannel(),
	}
	if config != nil {
		p.log = config.MakeLogger("PRE")
	} else {
		p.log = logger.NewNull()
	}
	if retriever == nil {
		// If we pass in a nil retriever, this prefetcher shouldn't do
		// anything. Treat it as already shut down.
		p.Shutdown()
		close(p.doneCh)
	} else {
		go p.run(testSyncCh)
		go p.shutdownLoop()
	}
	return p
}

func (p *blockPrefetcher) applyToParentsRecursive(f func(kbfsblock.ID, *prefetch),
	blockID kbfsblock.ID, pre *prefetch) {
	for b := range pre.parents {
		parent, ok := p.prefetches[b]
		if !ok {
			delete(pre.parents, b)
			continue
		}
		p.applyToParentsRecursive(f, b, parent)
	}
	f(blockID, pre)
}

// Walk up the block tree decrementing each node by one. Any
// zeroes we hit get marked complete and deleted.
// TODO: If we ever hit a lower number than the child, panic.
func (p *blockPrefetcher) completePrefetch(
	numBlocks int) func(kbfsblock.ID, *prefetch) {
	return func(blockID kbfsblock.ID, pp *prefetch) {
		pp.subtreeBlockCount -= numBlocks
		if pp.subtreeBlockCount < 0 {
			panic("completePrefetch overstepped its bounds")
		}
		if pp.subtreeBlockCount == 0 {
			delete(p.prefetches, blockID)
			ctx := context.Background()
			err := p.retriever.PutInCaches(ctx, pp.req.ptr, pp.req.kmd.TlfID(),
				pp.req.block, pp.req.lifetime, FinishedPrefetch)
			if err != nil {
				p.log.Warning("Failed to complete prefetch due to "+
					"cache error, canceled it instead: %+v", err)
			}
		}
	}
}

func (p *blockPrefetcher) decrementPrefetch(blockID kbfsblock.ID, pp *prefetch) {
	pp.subtreeBlockCount--
	if pp.subtreeBlockCount < 0 {
		panic("decrementPrefetch overstepped its bounds")
	}
}

func (p *blockPrefetcher) cancelPrefetch(blockID kbfsblock.ID, pp *prefetch) {
	delete(p.prefetches, blockID)
}

func (p *blockPrefetcher) isShutdown() bool {
	select {
	case <-p.shutdownCh:
		return true
	default:
		return false
	}
}

// shutdownLoop tracks in-flight requests
func (p *blockPrefetcher) shutdownLoop() {
top:
	for {
		select {
		case chInterface := <-p.inFlightFetches.Out():
			ch := chInterface.(<-chan error)
			<-ch
		case <-p.shutdownCh:
			break top
		}
	}
	for p.inFlightFetches.Len() > 0 {
		chInterface := <-p.inFlightFetches.Out()
		ch := chInterface.(<-chan error)
		<-ch
	}
	p.almostDoneCh <- struct{}{}
}

// run prefetches blocks.
// E.g. a synced prefetch:
// a -> {b -> {c, d}, e -> {f, g}}:
// * state of prefetch tree in `p.prefetches`.
// 1) a is fetched, triggers b and e.
//    * a:2 -> {b:1, e:1}
// 2) b is fetched, decrements b and a by 1, and triggers c and d to increment
//    b and a by 2.
//    * a:3 -> {b:2 -> {c:1, d:1}, e:1}
// 3) c is fetched, and isTail==true so it completes up the tree.
//    * a:2 -> {b:1 -> {d:1}, e:1}
// 4) d is fetched, and isTail==true so it completes up the tree.
//    * a:1 -> {e:1}
// 5) e is fetched, decrements e and a by 1, and triggers f and g to increment
//    e an a by 2.
//    * a:2 -> {e:2 -> {f:1, g:1}}
// 6) f is fetched, and isTail==true so it completes up the tree.
//    * a:1 -> {e:1 -> {g:1}}
// 5) g is fetched, completing g, e, and a.
//    * <empty>
//
// A *critical* assumption here is that a block tree will never have a diamond
// topology. That is, while a block may have multiple parents, at no point can
// there exist more than one path from a block to another block in the tree.
// That assumption should hold because blocks are content addressed, so
// changing anything about one block creates brand new parents all the way up
// the tree. If this did ever happen, a completed fetch downstream of the
// diamond would be double counted in all nodes above the diamond, and the
// prefetcher would eventually panic.
func (p *blockPrefetcher) run(testSyncCh <-chan struct{}) {
	defer func() {
		close(p.doneCh)
	}()
	isShuttingDown := false
	for {
		if isShuttingDown && p.inFlightFetches.Len() == 0 &&
			p.prefetchRequestCh.Len() == 0 && p.prefetchCancelCh.Len() == 0 {
			return
		}
		if testSyncCh != nil {
			<-testSyncCh
		}
		select {
		case bid := <-p.prefetchCancelCh.Out():
			blockID := bid.(kbfsblock.ID)
			pre, ok := p.prefetches[blockID]
			if !ok {
				continue
			}
			p.log.Debug("canceling prefetch for block %s", blockID)
			// Walk up the block tree and delete every parent.
			p.applyToParentsRecursive(p.cancelPrefetch, blockID, pre)
		case reqInt := <-p.prefetchRequestCh.Out():
			req := reqInt.(*prefetchRequest)
			pre, isPrefetchWaiting := p.prefetches[req.ptr.ID]
			if isPrefetchWaiting && pre.req == nil {
				// If this prefetch already appeared in the tree, ensure it
				// has a req associated with it.
				pre.req = req
			}
			if req.prefetchStatus == FinishedPrefetch {
				p.log.Debug("prefetch already finished for block %s",
					req.ptr.ID)
				// First we handle finished prefetches.
				if isPrefetchWaiting {
					if pre.subtreeBlockCount < 0 {
						panic("the subtreeBlockCount for a block should " +
							"never be < 0")
					}
					// Since we decrement by `pre.subtreeBlockCount`, we're
					// guaranteed that `pre` will be removed from the
					// prefetcher.
					p.log.Debug("completing prefetch for block %s", req.ptr.ID)
					p.applyToParentsRecursive(
						p.completePrefetch(pre.subtreeBlockCount),
						req.ptr.ID,
						pre)
				}
				// Always short circuit a finished prefetch.
				continue
			}
			if req.priority < lowestTriggerPrefetchPriority {
				p.log.Debug("skipping prefetch for block %s", req.ptr.ID)
				continue
			}
			if req.prefetchStatus == TriggeredPrefetch && !req.isDeepSync {
				p.log.Debug("prefetch already triggered for block ID %s",
					req.ptr.ID)
				continue
			}
			if isPrefetchWaiting {
				if pre.subtreeTriggered {
					p.log.Debug("prefetch subtree already triggered for "+
						"block ID %s", req.ptr.ID)
					// Redundant prefetch request.
					// We've already seen _this_ block, and already triggered
					// prefetches for its children. No use doing it again!
					if pre.subtreeBlockCount == 0 {
						// Only this block is left, and we didn't prefetch on a
						// previous prefetch through to the tail. So we cancel
						// up the tree. This still allows upgrades from an
						// unsynced block to a synced block, since p.prefetches
						// should be ephemeral.
						p.applyToParentsRecursive(p.cancelPrefetch, req.ptr.ID,
							pre)
					}
					if !pre.req.isDeepSync && req.isDeepSync {
						// The prefetcher doesn't know about a deep sync but
						// now one has been created.
						pre.req.isDeepSync = true
					} else {
						continue
					}
				} else {
					// This block was in the tree and thus was counted, but now
					// it has been successfully fetched. We need to percolate
					// that information up the tree.
					if pre.subtreeBlockCount == 0 {
						panic("prefetch was in the tree, wasn't triggered, " +
							"but had a block count of 0")
					}
					p.applyToParentsRecursive(p.decrementPrefetch, req.ptr.ID,
						pre)
					pre.subtreeTriggered = true
				}
			} else {
				// Ensure we have a prefetch to work with.
				// If the prefetch is to be tracked, then the 0
				// `subtreeBlockCount` will be incremented by `numBlocks`
				// below, once we've ensured that `numBlocks` is not 0.
				pre = &prefetch{0, true, req, make(map[kbfsblock.ID]bool)}
			}
			ctx, _ := context.WithTimeout(context.Background(),
				prefetchTimeout)
			// TODO: There is a potential optimization here that we can
			// consider: Currently every time a prefetch is triggered, we
			// iterate through all the block's child pointers. This is short
			// circuited in `TriggerPrefetch` and here in various conditions.
			// However, for synced trees we ignore that and prefetch anyway. So
			// here we would need to figure out a heuristic to avoid that
			// iteration.
			//
			// `numBlocks` now represents only the number of blocks to add
			// to the tree from `pre` to its roots, inclusive.
			numBlocks, isTail, err := p.handlePrefetch(ctx, pre)
			if err != nil {
				p.log.Debug("error handling prefetch for block %s", req.ptr.ID)
				// There's nothing for us to do when there's an error.
				continue
			}
			if isTail {
				p.log.Debug("completed prefetch for tail block %s ", req.ptr.ID)
				// This is a tail block with no children.  Parent blocks are
				// potentially waiting for this prefetch, so we percolate the
				// information up the tree that this prefetch is done.
				//
				// Note that only a tail block or cached block with
				// `FinishedPrefetch` can trigger a completed prefetch.
				//
				// We use 0 as our completion number because we've already
				// decremented above as appropriate. This just walks up the
				// tree removing blocks with a 0 subtree. We couldn't do that
				// above because `handlePrefetch` potentially adds blocks.
				// TODO: think about whether a refactor can be cleanly done to
				// only walk up the tree once. We'd track a `numBlocks` and
				// complete or decrement as appropriate.
				p.applyToParentsRecursive(
					p.completePrefetch(0), req.ptr.ID, pre)
				continue
			}
			// This is not a tail block.
			if numBlocks == 0 {
				p.log.Debug("no blocks to prefetch for block %s", req.ptr.ID)
				// All the blocks to be triggered have already done so. Do
				// nothing.  This is simply an optimization to avoid crawling
				// the tree.
				continue
			}
			if !isPrefetchWaiting {
				p.log.Debug("adding block %s to the prefetch tree", req.ptr.ID)
				// This block doesn't appear in the prefetch tree, so it's the
				// root of a new prefetch tree. Add it to the tree.
				p.prefetches[req.ptr.ID] = pre
				// One might think that since this block wasn't in the tree, we
				// need to `numBlocks++`. But since we're in this flow, the
				// block has already been fetched and is thus done.  So it
				// shouldn't block anything above it in the tree from
				// completing.
			}
			p.log.Debug("prefetching %d block(s) with parent block %s",
				numBlocks, req.ptr.ID)
			// Walk up the block tree and add numBlocks to every parent,
			// starting with this block.
			p.applyToParentsRecursive(func(_ kbfsblock.ID, pp *prefetch) {
				pp.subtreeBlockCount += numBlocks
			}, req.ptr.ID, pre)
		case <-p.almostDoneCh:
			isShuttingDown = true
		}
	}
}

// calculatePriority returns either a base priority for an unsynced TLF or a
// high priority for a synced TLF.
func (p *blockPrefetcher) calculatePriority(basePriority int,
	tlfID tlf.ID) int {
	if p.config.IsSyncedTlf(tlfID) {
		return defaultOnDemandRequestPriority - 1
	}
	return basePriority
}

func (p *blockPrefetcher) request(ctx context.Context, priority int,
	kmd KeyMetadata, ptr BlockPointer, block Block,
	lifetime BlockCacheLifetime) {
	ch := p.retriever.Request(ctx, priority, kmd, ptr, block, lifetime)
	p.inFlightFetches.In() <- ch
}

// recordPrefetchParent maintains prefetch accounting for a given block. This
// maps the parent->child block relationship. `numBlocks` represents the number
// of blocks that need to be accounted in `parentBlockID`. `needNewFetch`
// represents whether we need to fetch `childBlockID`, or whether such a fetch
// has already been triggered.
func (p *blockPrefetcher) recordPrefetchParent(childBlockID kbfsblock.ID,
	parentBlockID kbfsblock.ID) (numBlocks int, needNewFetch bool) {
	// If the prefetch is already waiting, don't make it wait again.
	// Add the parent, however.
	pre, isPrefetchWaiting := p.prefetches[childBlockID]
	if !isPrefetchWaiting {
		// If the block isn't in the tree, we add it with a block count of 1 (a
		// later TriggerPrefetch will come in and decrement it).
		pre = &prefetch{1, false, nil, make(map[kbfsblock.ID]bool)}
		p.prefetches[childBlockID] = pre
		needNewFetch = true
	}
	if !pre.parents[parentBlockID] {
		// The new parent needs its subtree block count increased.
		pre.parents[parentBlockID] = true
		return pre.subtreeBlockCount, needNewFetch
	}
	return 0, needNewFetch
}

func (p *blockPrefetcher) prefetchIndirectFileBlock(ctx context.Context,
	parentBlockID kbfsblock.ID, b *FileBlock, kmd KeyMetadata,
	lifetime BlockCacheLifetime) (numBlocks int, isTail bool) {
	// Prefetch indirect block pointers.
	startingPriority :=
		p.calculatePriority(fileIndirectBlockPrefetchPriority, kmd.TlfID())
	for i, ptr := range b.IPtrs {
		n, needNewFetch :=
			p.recordPrefetchParent(ptr.BlockPointer.ID, parentBlockID)
		numBlocks += n
		if needNewFetch {
			p.request(ctx, startingPriority-i, kmd,
				ptr.BlockPointer, b.NewEmpty(), lifetime)
		}
	}
	return numBlocks, len(b.IPtrs) == 0
}

func (p *blockPrefetcher) prefetchIndirectDirBlock(ctx context.Context,
	parentBlockID kbfsblock.ID, b *DirBlock, kmd KeyMetadata,
	lifetime BlockCacheLifetime) (numBlocks int, isTail bool) {
	// Prefetch indirect block pointers.
	startingPriority :=
		p.calculatePriority(fileIndirectBlockPrefetchPriority, kmd.TlfID())
	for i, ptr := range b.IPtrs {
		n, needNewFetch :=
			p.recordPrefetchParent(ptr.BlockPointer.ID, parentBlockID)
		numBlocks += n
		if needNewFetch {
			p.request(ctx, startingPriority-i, kmd,
				ptr.BlockPointer, b.NewEmpty(), lifetime)
		}
	}
	return numBlocks, len(b.IPtrs) == 0
}

func (p *blockPrefetcher) prefetchDirectDirBlock(ctx context.Context,
	parentBlockID kbfsblock.ID, b *DirBlock, kmd KeyMetadata,
	lifetime BlockCacheLifetime) (numBlocks int, isTail bool) {
	// Prefetch all DirEntry root blocks.
	dirEntries := dirEntriesBySizeAsc{dirEntryMapToDirEntries(b.Children)}
	sort.Sort(dirEntries)
	startingPriority :=
		p.calculatePriority(dirEntryPrefetchPriority, kmd.TlfID())
	totalNumBlocks := 0
	for i, entry := range dirEntries.dirEntries {
		// Prioritize small files
		priority := startingPriority - i
		var block Block
		switch entry.Type {
		case Dir:
			block = &DirBlock{}
		case File:
			block = &FileBlock{}
		case Exec:
			block = &FileBlock{}
		default:
			p.log.CDebugf(context.TODO(), "Skipping prefetch for entry of "+
				"unknown type %d", entry.Type)
			continue
		}
		totalNumBlocks++
		n, needNewFetch :=
			p.recordPrefetchParent(entry.BlockPointer.ID, parentBlockID)
		numBlocks += n
		if needNewFetch {
			p.request(ctx, priority, kmd, entry.BlockPointer,
				block, lifetime)
		}
	}
	if totalNumBlocks == 0 {
		isTail = true
	}
	return numBlocks, isTail
}

// handlePrefetch allows the prefetcher to trigger prefetches. Whichever
// component is responsible for retrieving blocks will call this method once
// it's done retrieving a block. Returns `numBlocks` which indicates how many
// blocks with a parent of `ptr.ID` must complete for `ptr.ID`'s prefetch to be
// complete.
func (p *blockPrefetcher) handlePrefetch(ctx context.Context, pre *prefetch) (
	numBlocks int, isTail bool, err error) {
	req := pre.req
	switch b := req.block.(type) {
	case *FileBlock:
		if b.IsInd {
			numBlocks, isTail = p.prefetchIndirectFileBlock(ctx,
				req.ptr.ID, b, req.kmd, req.lifetime)
		} else {
			isTail = true
		}
	case *DirBlock:
		if b.IsInd {
			numBlocks, isTail = p.prefetchIndirectDirBlock(ctx, req.ptr.ID,
				b, req.kmd, req.lifetime)
		} else {
			numBlocks, isTail = p.prefetchDirectDirBlock(ctx, req.ptr.ID,
				b, req.kmd, req.lifetime)
		}
	default:
		// Skipping prefetch for block of unknown type (likely CommonBlock)
		return 0, false, errors.New("Unknown block type")
	}
	return numBlocks, isTail, nil
}

func (p *blockPrefetcher) triggerPrefetch(req *prefetchRequest) {
	select {
	case p.prefetchRequestCh.In() <- req:
	case <-p.shutdownCh:
		p.log.Warning("Skipping prefetch for block %v since "+
			"the prefetcher is shutdown", req.ptr.ID)
	}
	return
}

func (p *blockPrefetcher) cacheOrCancelPrefetch(ctx context.Context,
	ptr BlockPointer, tlfID tlf.ID, block Block, lifetime BlockCacheLifetime,
	prefetchStatus PrefetchStatus) error {
	err := p.retriever.PutInCaches(ctx, ptr, tlfID, block, lifetime,
		prefetchStatus)
	if err != nil {
		p.log.CWarningf(ctx, "error prefetching block %s, canceling", ptr.ID)
		p.CancelPrefetch(ptr.ID)
	}
	return err
}

// TriggerPrefetch triggers a prefetch if appropriate.
func (p *blockPrefetcher) TriggerPrefetch(ctx context.Context,
	ptr BlockPointer, block Block, kmd KeyMetadata, priority int,
	lifetime BlockCacheLifetime, prefetchStatus PrefetchStatus) {
	isDeepSync := p.config.IsSyncedTlf(kmd.TlfID())
	req := &prefetchRequest{ptr, block, kmd, priority, lifetime,
		prefetchStatus, isDeepSync}
	if prefetchStatus == FinishedPrefetch {
		// Finished prefetches can always be short circuited.
		// If we're here, then FinishedPrefetch is already cached.
	} else if priority < lowestTriggerPrefetchPriority {
		// Only high priority requests can trigger prefetches. Leave the
		// prefetchStatus unchanged, but cache anyway.
		p.retriever.PutInCaches(ctx, ptr, kmd.TlfID(), block, lifetime,
			prefetchStatus)
	} else {
		// Note that here we are caching `TriggeredPrefetch`, but the request
		// will still reflect the passed-in `prefetchStatus`, since that's the
		// one the prefetching goroutine needs to decide what to do with.
		err := p.cacheOrCancelPrefetch(ctx, ptr, kmd.TlfID(), block, lifetime,
			TriggeredPrefetch)
		if err != nil {
			return
		}
	}
	p.triggerPrefetch(req)
}

func (p *blockPrefetcher) CancelPrefetch(blockID kbfsblock.ID) {
	select {
	// After `p.shutdownCh` is closed, we still need to receive prefetch
	// cancelation until all prefetching is done.
	case <-p.almostDoneCh:
	case p.prefetchCancelCh.In() <- blockID:
	}
}

// Shutdown implements the Prefetcher interface for blockPrefetcher.
func (p *blockPrefetcher) Shutdown() <-chan struct{} {
	select {
	case <-p.shutdownCh:
	default:
		close(p.shutdownCh)
	}
	return p.doneCh
}

// ShutdownCh implements the Prefetcher interface for blockPrefetcher.
func (p *blockPrefetcher) ShutdownCh() <-chan struct{} {
	return p.shutdownCh
}
