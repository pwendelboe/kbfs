package libkbfs

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/keybase/client/go/logger"
	keybase1 "github.com/keybase/client/go/protocol"
	"golang.org/x/net/context"
)

// mdReqType indicates whether an operation makes MD modifications or not
type mdReqType int

const (
	// A read request that doesn't need an identify to be
	// performed.
	mdReadNoIdentify mdReqType = iota
	// A read request that needs an identify to be performed (if
	// it hasn't been already).
	mdReadNeedIdentify
	// A write request.
	mdWrite
)

type branchType int

const (
	standard       branchType = iota // an online, read-write branch
	archive                          // an online, read-only branch
	offline                          // an offline, read-write branch
	archiveOffline                   // an offline, read-only branch
)

type state int

const (
	// cleanState: no outstanding local writes.
	cleanState state = iota
	// dirtyState: there are outstanding local writes that haven't yet been
	// synced.
	dirtyState
)

type syncInfo struct {
	oldInfo    BlockInfo
	op         *syncOp
	unrefs     []BlockInfo
	bps        *blockPutState
	refBytes   uint64
	unrefBytes uint64
}

// Constants used in this file.  TODO: Make these configurable?
const (
	maxParallelBlockPuts = 10
	// Max response size for a single DynamoDB query is 1MB.
	maxMDsAtATime = 10
	// Time between checks for dirty files to flush, in case Sync is
	// never called.
	secondsBetweenBackgroundFlushes = 10
)

type fboMutexLevel mutexLevel

// TODO: Give cacheLock a level of 4.

const (
	fboMDWriter fboMutexLevel = 1
	fboHead                   = 2
	fboBlock                  = 3
)

func (o fboMutexLevel) String() string {
	switch o {
	case fboMDWriter:
		return "mdWriterLock"
	case fboHead:
		return "headLock"
	case fboBlock:
		return "blockLock"
	default:
		return fmt.Sprintf("Invalid fboMutexLevel %d", int(o))
	}
}

func fboMutexLevelToString(o mutexLevel) string {
	return (fboMutexLevel(o)).String()
}

// Rules for working with lockState in FBO:
//
//   - Every "execution flow" (i.e., program flow that happens
//     sequentially) needs its own lockState object. This usually means
//     that each "public" FBO method does:
//
//       lState := makeFBOLockState()
//
//     near the top.
//
//   - Plumb lState through to all functions that hold any of the
//     relevant locks, or are called under those locks.
//
//   - TODO: Once lState supports AssertHeld(), use it as appropriate.
//
// This way, violations of the lock hierarchy will be detected at
// runtime.

func makeFBOLockState() *lockState {
	return makeLevelState(fboMutexLevelToString)
}

// blockLock is just like a sync.RWMutex, but with an extra operation
// (DoRUnlockedIfPossible).
type blockLock struct {
	mu     leveledRWMutex
	locked bool
}

func (bl *blockLock) Lock(lState *lockState) {
	bl.mu.Lock(lState)
	bl.locked = true
}

func (bl *blockLock) Unlock(lState *lockState) {
	bl.locked = false
	bl.mu.Unlock(lState)
}

func (bl *blockLock) RLock(lState *lockState) {
	bl.mu.RLock(lState)
}

func (bl *blockLock) RUnlock(lState *lockState) {
	bl.mu.RUnlock(lState)
}

// DoRUnlockedIfPossible must be called when r- or w-locked. If
// r-locked, r-unlocks, runs the given function, and r-locks after
// it's done. Otherwise, just runs the given function.
func (bl *blockLock) DoRUnlockedIfPossible(lState *lockState, f func(*lockState)) {
	if !bl.locked {
		bl.RUnlock(lState)
		defer bl.RLock(lState)
	}

	f(lState)
}

// syncBlockState represents that state of a block with respect to
// whether it's currently being synced.  There can be three states:
//  1) Not being synced
//  2) Being synced and not yet re-dirtied: any write needs to
//     make a copy of the block before dirtying it.  Also, all writes must be
//     deferred.
//  3) Being synced and already re-dirtied: no copies are needed, but all
//     writes must still be deferred.
type syncBlockState int

const (
	blockNotBeingSynced syncBlockState = iota
	blockSyncingNotDirty
	blockSyncingAndDirty
)

// folderBranchOps implements the KBFSOps interface for a specific
// branch of a specific folder.  It is go-routine safe for operations
// within the folder.
//
// We use locks to protect against multiple goroutines accessing the
// same folder-branch.  The goal with our locking strategy is maximize
// concurrent access whenever possible.  See design/state_machine.md
// for more details.  There are three important locks:
//
// 1) mdWriterLock: Any "remote-sync" operation (one which modifies the
//    folder's metadata) must take this lock during the entirety of
//    its operation, to avoid forking the MD.
//
// 2) headLock: This is a read/write mutex.  It must be taken for
//    reading before accessing any part of the current head MD.  It
//    should be taken for the shortest time possible -- that means in
//    general that it should be taken, and the MD copied to a
//    goroutine-local variable, and then it can be released.
//    Remote-sync operations should take it for writing after pushing
//    all of the blocks and MD to the KBFS servers (i.e., all network
//    accesses), and then hold it until after all notifications have
//    been fired, to ensure that no concurrent "local" operations ever
//    see inconsistent state locally.
//
// 3) blockLock: This too is a read/write mutex.  It must be taken for
//    reading before accessing any blocks in the block cache that
//    belong to this folder/branch.  This includes checking their
//    dirty status.  It should be taken for the shortest time possible
//    -- that means in general it should be taken, and then the blocks
//    that will be modified should be copied to local variables in the
//    goroutine, and then it should be released.  The blocks should
//    then be modified locally, and then readied and pushed out
//    remotely.  Only after the blocks have been pushed to the server
//    should a remote-sync operation take the lock again (this time
//    for writing) and put/finalize the blocks.  Write and Truncate
//    should take blockLock for their entire lifetime, since they
//    don't involve writes over the network.  Furthermore, if a block
//    is not in the cache and needs to be fetched, we should release
//    the mutex before doing the network operation, and lock it again
//    before writing the block back to the cache.
//
// We want to allow writes and truncates to a file that's currently
// being sync'd, like any good networked file system.  The tricky part
// is making sure the changes can both: a) be read while the sync is
// happening, and b) be applied to the new file path after the sync is
// done.
//
// For now, we just do the dumb, brute force thing for now: if a block
// is currently being sync'd, it copies the block and puts it back
// into the cache as modified.  Then, when the sync finishes, it
// throws away the modified blocks and re-applies the change to the
// new file path (which might have a completely different set of
// blocks, so we can't just reuse the blocks that were modified during
// the sync.)
type folderBranchOps struct {
	config       Config
	folderBranch FolderBranch
	bid          BranchID // protected by mdWriterLock
	bType        branchType
	head         *RootMetadata
	observers    []Observer
	// Which blocks are currently being synced, so that writes and
	// truncates can do copy-on-write to avoid messing up the ongoing
	// sync.  If it is blockSyncingNotDirty, then any write to the
	// block should result in a deep copy and those writes should be
	// deferred; if it is blockSyncingAndDirty, then just defer the
	// writes.
	fileBlockStates map[BlockPointer]syncBlockState
	// Writes and truncates for blocks that were being sync'd, and
	// need to be replayed after the sync finishes on top of the new
	// versions of the blocks.
	deferredWrites []func(context.Context, *RootMetadata, path) error
	// Blocks that need to be deleted from the dirty cache before any
	// deferred writes are replayed.
	deferredDirtyDeletes []BlockPointer
	// set to true if this write or truncate should be deferred
	doDeferWrite bool
	// For writes and truncates, track the unsynced to-be-unref'd
	// block infos, per-path.  Uses a stripped BlockPointer in case
	// the Writer has changed during the operation.
	unrefCache map[BlockPointer]*syncInfo
	// For writes and truncates, track the modified (but not yet
	// committed) directory entries.  The outer map maps the parent
	// BlockPointer to the inner map, which maps the entry
	// BlockPointer to a modified entry.  Uses stripped BlockPointers
	// in case the Writer changed during the operation.
	deCache map[BlockPointer]map[BlockPointer]DirEntry

	// these locks, when locked concurrently by the same goroutine,
	// should only be taken in the following order to avoid deadlock:
	mdWriterLock leveledMutex   // taken by any method making MD modifications
	headLock     leveledRWMutex // protects access to the MD

	// protects access to blocks in this folder, fileBlockStates,
	// and deferredWrites.
	blockLock blockLock

	obsLock   sync.RWMutex // protects access to observers
	cacheLock sync.Mutex   // protects unrefCache and deCache

	// nodeCache itself is goroutine-safe, but this object's use
	// of it has special requirements:
	//
	//   - Reads can call PathFromNode() unlocked, since there are
	//     no guarantees with concurrent reads.
	//
	//   - Operations that takes mdWriterLock always needs the
	//     most up-to-date paths, so those must call
	//     PathFromNode() under mdWriterLock.
	//
	//   - Block write operations (write/truncate/sync) need to
	//     coordinate. Those should call PathFromNode() under
	//     blockLock. Furthermore, calls to UpdatePointer() must
	//     happen before the copy-on-write mode induced by Sync()
	//     is finished.
	nodeCache NodeCache

	// Set to true when we have staged, unmerged commits for this
	// device.  This means the device has forked from the main branch
	// seen by other devices.  Protected by mdWriterLock.
	staged bool

	// The current state of this folder-branch.
	stateLock sync.Mutex
	state     state

	// Whether we've identified this TLF or not.
	identifyLock sync.Mutex
	identifyDone bool

	// The current status summary for this folder
	status *folderBranchStatusKeeper

	// How to log
	log logger.Logger

	// Closed on shutdown
	shutdownChan chan struct{}

	// Can be used to turn off notifications for a while (e.g., for testing)
	updatePauseChan chan (<-chan struct{})

	// A queue of MD updates for this folder that need to have their
	// unref's blocks archived
	archiveChan chan *RootMetadata
	// archiveGroup can be Wait'd on to ensure that all outstanding
	// archivals are completed.  Calls to Wait() and Add() should be
	// protected by mdWriterLock.
	archiveGroup sync.WaitGroup

	// How to resolve conflicts
	cr *ConflictResolver
}

var _ KBFSOps = (*folderBranchOps)(nil)

// newFolderBranchOps constructs a new folderBranchOps object.
func newFolderBranchOps(config Config, fb FolderBranch,
	bType branchType) *folderBranchOps {
	nodeCache := newNodeCacheStandard(fb)

	// make logger
	branchSuffix := ""
	if fb.Branch != MasterBranch {
		branchSuffix = " " + string(fb.Branch)
	}
	tlfStringFull := fb.Tlf.String()
	// Shorten the TLF ID for the module name.  8 characters should be
	// unique enough for a local node.
	log := config.MakeLogger(fmt.Sprintf("FBO %s%s", tlfStringFull[:8],
		branchSuffix))
	// But print it out once in full, just in case.
	log.CInfof(nil, "Created new folder-branch for %s", tlfStringFull)

	mdWriterLock := makeLeveledMutex(mutexLevel(fboMDWriter), &sync.Mutex{})
	headLock := makeLeveledRWMutex(mutexLevel(fboHead), &sync.RWMutex{})
	blockLockMu := makeLeveledRWMutex(mutexLevel(fboBlock), &sync.RWMutex{})

	fbo := &folderBranchOps{
		config:          config,
		folderBranch:    fb,
		bid:             BranchID{},
		bType:           bType,
		observers:       make([]Observer, 0),
		fileBlockStates: make(map[BlockPointer]syncBlockState),
		deferredWrites: make(
			[]func(context.Context, *RootMetadata, path) error, 0),
		unrefCache:   make(map[BlockPointer]*syncInfo),
		deCache:      make(map[BlockPointer]map[BlockPointer]DirEntry),
		status:       newFolderBranchStatusKeeper(config, nodeCache),
		mdWriterLock: mdWriterLock,
		headLock:     headLock,
		blockLock: blockLock{
			mu: blockLockMu,
		},
		nodeCache:       nodeCache,
		state:           cleanState,
		log:             log,
		shutdownChan:    make(chan struct{}),
		updatePauseChan: make(chan (<-chan struct{})),
		archiveChan:     make(chan *RootMetadata, 25),
	}
	fbo.cr = NewConflictResolver(config, fbo)
	if config.DoBackgroundFlushes() {
		go fbo.backgroundFlusher(secondsBetweenBackgroundFlushes * time.Second)
	}
	// Turn off block archiving for now: KBFS-641.
	//go fbo.archiveBlocksInBackground()
	return fbo
}

// Shutdown safely shuts down any background goroutines that may have
// been launched by folderBranchOps.
func (fbo *folderBranchOps) Shutdown() error {
	if fbo.config.CheckStateOnShutdown() {
		ctx := context.TODO()
		lState := makeFBOLockState()

		if fbo.getState() == dirtyState {
			fbo.log.CDebugf(ctx, "Skipping state-checking due to dirty state")
		} else if fbo.getStaged(lState) {
			fbo.log.CDebugf(ctx, "Skipping state-checking due to being staged")
		} else {
			// Make sure we're up to date first
			if err := fbo.SyncFromServer(ctx, fbo.folderBranch); err != nil {
				return err
			}

			// Check the state for consistency before shutting down.
			sc := NewStateChecker(fbo.config)
			if err := sc.CheckMergedState(ctx, fbo.id()); err != nil {
				return err
			}
		}
	}

	close(fbo.shutdownChan)
	fbo.cr.Shutdown()
	return nil
}

func (fbo *folderBranchOps) id() TlfID {
	return fbo.folderBranch.Tlf
}

func (fbo *folderBranchOps) branch() BranchName {
	return fbo.folderBranch.Branch
}

func (fbo *folderBranchOps) GetFavorites(ctx context.Context) ([]*Favorite, error) {
	return nil, errors.New("GetFavorites is not supported by folderBranchOps")
}

func (fbo *folderBranchOps) getState() state {
	fbo.stateLock.Lock()
	defer fbo.stateLock.Unlock()
	return fbo.state
}

// getStaged should not be called if mdWriterLock is already taken.
func (fbo *folderBranchOps) getStaged(lState *lockState) bool {
	fbo.mdWriterLock.Lock(lState)
	defer fbo.mdWriterLock.Unlock(lState)
	return fbo.staged
}

func (fbo *folderBranchOps) transitionState(newState state) {
	fbo.stateLock.Lock()
	defer fbo.stateLock.Unlock()
	switch newState {
	case cleanState:
		if len(fbo.deCache) > 0 {
			// if we still have writes outstanding, don't allow the
			// transition into the clean state
			return
		}
	default:
		// no specific checks needed
	}
	fbo.state = newState
}

// The caller must hold mdWriterLock.
func (fbo *folderBranchOps) setStagedLocked(
	lState *lockState, staged bool, bid BranchID) {
	fbo.staged = staged
	fbo.bid = bid
	if !staged {
		fbo.status.setCRChains(nil, nil)
	}
}

func (fbo *folderBranchOps) checkDataVersion(p path, ptr BlockPointer) error {
	if ptr.DataVer < FirstValidDataVer {
		return InvalidDataVersionError{ptr.DataVer}
	}
	if ptr.DataVer > fbo.config.DataVersion() {
		return NewDataVersionError{p, ptr.DataVer}
	}
	return nil
}

// headLock must be taken by caller
func (fbo *folderBranchOps) setHeadLocked(ctx context.Context,
	lState *lockState, md *RootMetadata) error {
	isFirstHead := fbo.head == nil
	if !isFirstHead {
		mdID, err := md.MetadataID(fbo.config)
		if err != nil {
			return err
		}

		headID, err := fbo.head.MetadataID(fbo.config)
		if err != nil {
			return err
		}

		if headID == mdID {
			// only save this new MD if the MDID has changed
			return nil
		}
	}

	fbo.log.CDebugf(ctx, "Setting head revision to %d", md.Revision)
	err := fbo.config.MDCache().Put(md)
	if err != nil {
		return err
	}

	// If this is the first time the MD is being set, and we are
	// operating on unmerged data, initialize the state properly and
	// kick off conflict resolution.
	if isFirstHead && md.MergedStatus() == Unmerged {
		// no need to take the writer lock here since is the first
		// time the folder is being used
		fbo.setStagedLocked(lState, true, md.BID)
		// Use uninitialized for the merged branch; the unmerged
		// revision is enough to trigger conflict resolution.
		fbo.cr.Resolve(md.Revision, MetadataRevisionUninitialized)
	}

	fbo.head = md
	fbo.status.setRootMetadata(md)
	if isFirstHead {
		// Start registering for updates right away, using this MD
		// as a starting point. For now only the master branch can
		// get updates
		if fbo.branch() == MasterBranch {
			go fbo.registerForUpdates()
		}
	}
	return nil
}

func (fbo *folderBranchOps) identifyOnce(
	ctx context.Context, md *RootMetadata) error {
	fbo.identifyLock.Lock()
	defer fbo.identifyLock.Unlock()
	if fbo.identifyDone {
		return nil
	}

	h := md.GetTlfHandle()
	fbo.log.CDebugf(ctx, "Running identifies on %s", h.ToString(ctx, fbo.config))
	err := identifyHandle(ctx, fbo.config, h)
	if err != nil {
		fbo.log.CDebugf(ctx, "Identify finished with error: %v", err)
		// For now, if the identify fails, let the
		// next function to hit this code path retry.
		return err
	}

	fbo.log.CDebugf(ctx, "Identify finished successfully")
	fbo.identifyDone = true
	return nil
}

// if rtype == write, then mdWriterLock must be taken
func (fbo *folderBranchOps) getMDLocked(
	ctx context.Context, lState *lockState, rtype mdReqType) (
	md *RootMetadata, err error) {
	defer func() {
		if err != nil || rtype == mdReadNoIdentify {
			return
		}
		err = fbo.identifyOnce(ctx, md)
	}()

	md = func() *RootMetadata {
		fbo.headLock.RLock(lState)
		defer fbo.headLock.RUnlock(lState)
		return fbo.head
	}()
	if md != nil {
		return md, nil
	}

	// Unless we're in mdWrite mode, we can't safely fetch the new
	// MD without causing races, so bail.
	if rtype != mdWrite {
		return nil, MDWriteNeededInRequest{}
	}

	// Not in cache, fetch from server and add to cache.  First, see
	// if this device has any unmerged commits -- take the latest one.
	mdops := fbo.config.MDOps()

	// get the head of the unmerged branch for this device (if any)
	md, err = mdops.GetUnmergedForTLF(ctx, fbo.id(), NullBranchID)
	if err != nil {
		return nil, err
	}
	if md == nil {
		// no unmerged MDs for this device, so just get the current head
		md, err = mdops.GetForTLF(ctx, fbo.id())
		if err != nil {
			return nil, err
		}
	}

	if md.data.Dir.Type != Dir && (!md.IsInitialized() || md.IsReadable()) {
		err = fbo.initMDLocked(ctx, lState, md)
		if err != nil {
			return nil, err
		}
	} else {
		fbo.headLock.Lock(lState)
		defer fbo.headLock.Unlock(lState)
		err = fbo.setHeadLocked(ctx, lState, md)
		if err != nil {
			return nil, err
		}
	}

	return md, err
}

func (fbo *folderBranchOps) getMDForReadHelper(
	ctx context.Context, lState *lockState, rtype mdReqType) (*RootMetadata, error) {
	md, err := fbo.getMDLocked(ctx, lState, rtype)
	if err != nil {
		return nil, err
	}

	uid, err := fbo.config.KBPKI().GetCurrentUID(ctx)
	if err != nil {
		return nil, err
	}
	if !md.GetTlfHandle().IsReader(uid) {
		return nil, NewReadAccessError(ctx, fbo.config, md.GetTlfHandle(), uid)
	}
	return md, nil
}

func (fbo *folderBranchOps) getMDForReadNoIdentify(
	ctx context.Context, lState *lockState) (*RootMetadata, error) {
	return fbo.getMDForReadHelper(ctx, lState, mdReadNoIdentify)
}

func (fbo *folderBranchOps) getMDForReadNeedIdentify(
	ctx context.Context, lState *lockState) (*RootMetadata, error) {
	return fbo.getMDForReadHelper(ctx, lState, mdReadNeedIdentify)
}

// mdWriterLock must be taken by the caller.
func (fbo *folderBranchOps) getMDForWriteLocked(
	ctx context.Context, lState *lockState) (*RootMetadata, error) {
	md, err := fbo.getMDLocked(ctx, lState, mdWrite)
	if err != nil {
		return nil, err
	}

	uid, err := fbo.config.KBPKI().GetCurrentUID(ctx)
	if err != nil {
		return nil, err
	}
	if !md.GetTlfHandle().IsWriter(uid) {
		return nil,
			NewWriteAccessError(ctx, fbo.config, md.GetTlfHandle(), uid)
	}

	// Make a new successor of the current MD to hold the coming
	// writes.  The caller must pass this into syncBlockAndCheckEmbed
	// or the changes will be lost.
	newMd, err := md.MakeSuccessor(fbo.config)
	if err != nil {
		return nil, err
	}
	return &newMd, nil
}

// mdWriterLock must be taken by the caller.
func (fbo *folderBranchOps) getMDForRekeyWriteLocked(
	ctx context.Context, lState *lockState) (*RootMetadata, bool, error) {
	md, err := fbo.getMDLocked(ctx, lState, mdWrite)
	if err != nil {
		return nil, false, err
	}

	uid, err := fbo.config.KBPKI().GetCurrentUID(ctx)
	if err != nil {
		return nil, false, err
	}

	// must be a reader or writer (it checks both.)
	if !md.GetTlfHandle().IsReader(uid) {
		return nil, false,
			NewRekeyPermissionError(ctx, fbo.config, md.GetTlfHandle(), uid)
	}

	newMd, err := md.MakeSuccessor(fbo.config)
	if err != nil {
		return nil, false, err
	}

	if !md.GetTlfHandle().IsWriter(uid) {
		// readers shouldn't modify writer metadata
		if !newMd.IsWriterMetadataCopiedSet() {
			return nil, false,
				NewRekeyPermissionError(ctx, fbo.config, md.GetTlfHandle(), uid)
		}
		// readers are currently only allowed to set the rekey bit
		// TODO: allow readers to fully rekey only themself.
		if !newMd.IsRekeySet() {
			return nil, false,
				NewRekeyPermissionError(ctx, fbo.config, md.GetTlfHandle(), uid)
		}
	}

	return &newMd, md.IsRekeySet(), nil
}

func (fbo *folderBranchOps) nowUnixNano() int64 {
	return fbo.config.Clock().Now().UnixNano()
}

// mdWriterLock must be taken
func (fbo *folderBranchOps) initMDLocked(
	ctx context.Context, lState *lockState, md *RootMetadata) error {
	// create a dblock since one doesn't exist yet
	uid, err := fbo.config.KBPKI().GetCurrentUID(ctx)
	if err != nil {
		return err
	}

	handle := md.GetTlfHandle()

	if !handle.IsWriter(uid) {
		return NewWriteAccessError(ctx, fbo.config, handle, uid)
	}

	newDblock := &DirBlock{
		Children: make(map[string]DirEntry),
	}

	var expectedKeyGen KeyGen
	if md.ID.IsPublic() {
		md.Writers = make([]keybase1.UID, len(handle.Writers))
		copy(md.Writers, handle.Writers)
		expectedKeyGen = PublicKeyGen
	} else {
		// create a new set of keys for this metadata
		if _, err := fbo.config.KeyManager().Rekey(ctx, md); err != nil {
			return err
		}
		expectedKeyGen = FirstValidKeyGen
	}
	keyGen := md.LatestKeyGeneration()
	if keyGen != expectedKeyGen {
		return InvalidKeyGenerationError{handle, keyGen}
	}
	info, plainSize, readyBlockData, err :=
		fbo.readyBlock(ctx, md, newDblock, uid)
	if err != nil {
		return err
	}

	now := fbo.nowUnixNano()
	md.data.Dir = DirEntry{
		BlockInfo: info,
		EntryInfo: EntryInfo{
			Type:  Dir,
			Size:  uint64(plainSize),
			Mtime: now,
			Ctime: now,
		},
	}
	md.AddOp(newCreateOp("", BlockPointer{}, Dir))
	md.AddRefBlock(md.data.Dir.BlockInfo)
	md.UnrefBytes = 0

	// make sure we're a writer before putting any blocks
	if !handle.IsWriter(uid) {
		return NewWriteAccessError(ctx, fbo.config, handle, uid)
	}

	if err = fbo.config.BlockOps().Put(ctx, md, info.BlockPointer,
		readyBlockData); err != nil {
		return err
	}
	if err = fbo.config.BlockCache().Put(
		info.BlockPointer, fbo.id(), newDblock, TransientEntry); err != nil {
		return err
	}

	// finally, write out the new metadata
	if err = fbo.config.MDOps().Put(ctx, md); err != nil {
		return err
	}

	fbo.headLock.Lock(lState)
	defer fbo.headLock.Unlock(lState)
	if fbo.head != nil {
		headID, _ := fbo.head.MetadataID(fbo.config)
		return fmt.Errorf(
			"%v: Unexpected MD ID during new MD initialization: %v",
			md.ID, headID)
	}
	err = fbo.setHeadLocked(ctx, lState, md)
	if err != nil {
		return err
	}
	return nil
}

func (fbo *folderBranchOps) GetOrCreateRootNode(
	ctx context.Context, name string, public bool, branch BranchName) (
	node Node, ei EntryInfo, err error) {
	err = errors.New("GetOrCreateRootNode is not supported by " +
		"folderBranchOps")
	return
}

func (fbo *folderBranchOps) checkNode(node Node) error {
	fb := node.GetFolderBranch()
	if fb != fbo.folderBranch {
		return WrongOpsError{fbo.folderBranch, fb}
	}
	return nil
}

// CheckForNewMDAndInit sees whether the given MD object has been
// initialized yet; if not, it does so.
func (fbo *folderBranchOps) CheckForNewMDAndInit(
	ctx context.Context, md *RootMetadata) (err error) {
	fbo.log.CDebugf(ctx, "CheckForNewMDAndInit, revision=%d (%s)",
		md.Revision, md.MergedStatus())
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	fb := FolderBranch{md.ID, MasterBranch}
	if fb != fbo.folderBranch {
		return WrongOpsError{fbo.folderBranch, fb}
	}

	lState := makeFBOLockState()

	if md.data.Dir.Type == Dir {
		// this MD is already initialized
		fbo.headLock.Lock(lState)
		defer fbo.headLock.Unlock(lState)
		// Only update the head the first time; later it will be
		// updated either directly via writes or through the
		// background update processor.
		if fbo.head == nil {
			err := fbo.setHeadLocked(ctx, lState, md)
			if err != nil {
				return err
			}
		}
		return nil
	}

	// otherwise, initialize
	fbo.mdWriterLock.Lock(lState)
	defer fbo.mdWriterLock.Unlock(lState)
	return fbo.initMDLocked(ctx, lState, md)
}

// execMDReadNoIdentifyThenMDWrite first tries to execute the
// passed-in method in mdReadNoIdentify mode.  If it fails with an
// MDWriteNeededInRequest error, it re-executes the method as in
// mdWrite mode.  The passed-in method must note whether or not this
// is an mdWrite call.
//
// This must only be used by getRootNode().
func (fbo *folderBranchOps) execMDReadNoIdentifyThenMDWrite(
	lState *lockState, f func(*lockState, mdReqType) error) error {
	err := f(lState, mdReadNoIdentify)

	// Redo as an MD write request if needed
	if _, ok := err.(MDWriteNeededInRequest); ok {
		fbo.mdWriterLock.Lock(lState)
		defer fbo.mdWriterLock.Unlock(lState)
		err = f(lState, mdWrite)
	}
	return err
}

func (fbo *folderBranchOps) getRootNode(ctx context.Context) (
	node Node, ei EntryInfo, handle *TlfHandle, err error) {
	fbo.log.CDebugf(ctx, "getRootNode")
	defer func() {
		if err != nil {
			fbo.log.CDebugf(ctx, "Error: %v", err)
		} else {
			// node may still be nil if we're unwinding
			// from a panic.
			fbo.log.CDebugf(ctx, "Done: %v", node)
		}
	}()

	lState := makeFBOLockState()

	var md *RootMetadata
	err = fbo.execMDReadNoIdentifyThenMDWrite(lState,
		func(lState *lockState, rtype mdReqType) error {
			md, err = fbo.getMDLocked(ctx, lState, rtype)
			return err
		})
	if err != nil {
		return nil, EntryInfo{}, nil, err
	}

	// we may be an unkeyed client
	if err := md.isReadableOrError(ctx, fbo.config); err != nil {
		return nil, EntryInfo{}, nil, err
	}

	handle = md.GetTlfHandle()
	node, err = fbo.nodeCache.GetOrCreate(md.data.Dir.BlockPointer,
		handle.ToString(ctx, fbo.config), nil)
	if err != nil {
		return nil, EntryInfo{}, nil, err
	}

	return node, md.Data().Dir.EntryInfo, handle, nil
}

type makeNewBlock func() Block

// getBlockHelperLocked retrieves the block pointed to by ptr, which
// must be valid, either from the cache or from the server.
//
// This must be called only by get{File,Dir}BlockHelperLocked().
//
// blockLock should be taken for reading by the caller.
func (fbo *folderBranchOps) getBlockHelperLocked(ctx context.Context,
	lState *lockState, md *RootMetadata, ptr BlockPointer, branch BranchName,
	newBlock makeNewBlock, doCache bool) (
	Block, error) {
	if !ptr.IsValid() {
		return nil, InvalidBlockPointerError{ptr}
	}

	bcache := fbo.config.BlockCache()
	if block, err := bcache.Get(ptr, branch); err == nil {
		return block, nil
	}

	// TODO: add an optimization here that will avoid fetching the
	// same block twice from over the network

	// fetch the block, and add to cache
	block := newBlock()

	bops := fbo.config.BlockOps()

	// Unlock the blockLock while we wait for the network, only if
	// it's locked for reading.  If it's locked for writing, that
	// indicates we are performing an atomic write operation, and we
	// need to ensure that nothing else comes in and modifies the
	// blocks, so don't unlock.
	var err error
	fbo.blockLock.DoRUnlockedIfPossible(lState, func(*lockState) {
		err = bops.Get(ctx, md, ptr, block)
	})
	if err != nil {
		return nil, err
	}

	if doCache {
		if err := bcache.Put(ptr, fbo.id(), block, TransientEntry); err != nil {
			return nil, err
		}
	}
	return block, nil
}

// getFileBlockHelperLocked retrieves the block pointed to by ptr,
// which must be valid, either from an internal cache, the block
// cache, or from the server. An error is returned if the retrieved
// block is not a file block.
//
// This must be called only by getFileBlockForReading(),
// getFileBlockLocked(), and getFileLocked(). And unrefEntry(), I
// guess.
//
// p is used only when reporting errors, and can be empty.
//
// blockLock should be taken for reading by the caller.
func (fbo *folderBranchOps) getFileBlockHelperLocked(ctx context.Context,
	lState *lockState, md *RootMetadata, ptr BlockPointer,
	branch BranchName, p path) (
	*FileBlock, error) {
	block, err := fbo.getBlockHelperLocked(
		ctx, lState, md, ptr, branch, NewFileBlock, true)
	if err != nil {
		return nil, err
	}

	fblock, ok := block.(*FileBlock)
	if !ok {
		return nil, NotFileBlockError{ptr, branch, p}
	}

	return fblock, nil
}

// getBlockForReading retrieves the block pointed to by ptr, which
// must be valid, either from the cache or from the server.  The
// returned block may have a generic type (not DirBlock or FileBlock).
//
// This should be called for "internal" operations, like conflict
// resolution and state checking, which don't know what kind of block
// the pointer refers to.  The block will not be cached, if it wasn't
// in the cache already.
func (fbo *folderBranchOps) getBlockForReading(ctx context.Context,
	lState *lockState, md *RootMetadata, ptr BlockPointer, branch BranchName) (
	Block, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	return fbo.getBlockHelperLocked(ctx, lState, md, ptr, branch,
		NewCommonBlock, false)
}

// getDirBlockHelperLocked retrieves the block pointed to by ptr, which
// must be valid, either from the cache or from the server. An error
// is returned if the retrieved block is not a dir block.
//
// This must be called only by getDirBlockForReading() and
// getDirLocked().
//
// p is used only when reporting errors, and can be empty.
//
// blockLock should be taken for reading by the caller.
func (fbo *folderBranchOps) getDirBlockHelperLocked(ctx context.Context,
	lState *lockState, md *RootMetadata, ptr BlockPointer,
	branch BranchName, p path) (*DirBlock, error) {
	block, err := fbo.getBlockHelperLocked(
		ctx, lState, md, ptr, branch, NewDirBlock, true)
	if err != nil {
		return nil, err
	}

	dblock, ok := block.(*DirBlock)
	if !ok {
		return nil, NotDirBlockError{ptr, branch, p}
	}

	return dblock, nil
}

// getFileBlockForReading retrieves the block pointed to by ptr, which
// must be valid, either from the cache or from the server. An error
// is returned if the retrieved block is not a file block.
//
// This should be called for "internal" operations, like conflict
// resolution and state checking. "Real" operations should use
// getFileBlockLocked() and getFileLocked() instead.
//
// p is used only when reporting errors, and can be empty.
func (fbo *folderBranchOps) getFileBlockForReading(ctx context.Context,
	lState *lockState, md *RootMetadata, ptr BlockPointer,
	branch BranchName, p path) (*FileBlock, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	return fbo.getFileBlockHelperLocked(ctx, lState, md, ptr, branch, p)
}

// getDirBlockForReading retrieves the block pointed to by ptr, which
// must be valid, either from the cache or from the server. An error
// is returned if the retrieved block is not a dir block.
//
// This should be called for "internal" operations, like conflict
// resolution and state checking. "Real" operations should use
// getDirLocked() instead.
//
// p is used only when reporting errors, and can be empty.
func (fbo *folderBranchOps) getDirBlockForReading(ctx context.Context,
	lState *lockState, md *RootMetadata, ptr BlockPointer,
	branch BranchName, p path) (*DirBlock, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	return fbo.getDirBlockHelperLocked(ctx, lState, md, ptr, branch, p)
}

// getFileBlockLocked retrieves the block pointed to by ptr, which
// must be valid, either from the cache or from the server. An error
// is returned if the retrieved block is not a file block.
//
// The given path must be valid, and the given pointer must be its
// tail pointer or an indirect pointer from it. A read notification is
// triggered for the given path.
//
// This shouldn't be called for "internal" operations, like conflict
// resolution and state checking -- use getFileBlockForReading() for
// those instead.
//
// When rType == mdWrite and the cached version of the block is
// currently clean, or the block is currently being synced, this
// method makes a copy of the file block and returns it.  If this
// method might be called again for the same block within a single
// operation, it is the caller's responsibility to write that block
// back to the cache as dirty.
//
// blockLock should be taken for reading by the caller.
func (fbo *folderBranchOps) getFileBlockLocked(ctx context.Context,
	lState *lockState, md *RootMetadata, ptr BlockPointer,
	file path, rtype mdReqType) (*FileBlock, error) {
	// Callers should have already done this check, but it doesn't
	// hurt to do it again.
	if !file.isValid() {
		return nil, InvalidPathError{file}
	}

	fblock, err := fbo.getFileBlockHelperLocked(
		ctx, lState, md, ptr, file.Branch, file)
	if err != nil {
		return nil, err
	}

	fbo.config.Reporter().Notify(ctx, readNotification(file, false))
	defer fbo.config.Reporter().Notify(ctx, readNotification(file, true))

	if rtype == mdWrite {
		// Copy the block if it's for writing, and either the
		// block is not yet dirty or the block is currently
		// being sync'd and needs a copy even though it's
		// already dirty.
		if !fbo.config.BlockCache().IsDirty(ptr, file.Branch) ||
			fbo.fileBlockStates[ptr] == blockSyncingNotDirty {
			fblock = fblock.DeepCopy()
		}
	}
	return fblock, nil
}

// getFileLocked is getFileBlockLocked called with file.tailPointer().
func (fbo *folderBranchOps) getFileLocked(ctx context.Context,
	lState *lockState, md *RootMetadata, file path,
	rtype mdReqType) (*FileBlock, error) {
	return fbo.getFileBlockLocked(
		ctx, lState, md, file.tailPointer(), file, rtype)
}

// getDirLocked retrieves the block pointed to by the tail pointer of
// the given path, which must be valid, either from the cache or from
// the server. An error is returned if the retrieved block is not a
// dir block.
//
// This shouldn't be called for "internal" operations, like conflict
// resolution and state checking -- use getDirBlockForReading() for
// those instead.
//
// When rType == mdWrite and the cached version of the block is
// currently clean, this method makes a copy of the directory block and
// returns it.  If this method might be called again for the same
// block within a single operation, it is the caller's responsibility
// to write that block back to the cache as dirty.
//
// blockLock should be taken for reading by the caller.
//
// TODO: Audit for calls of getDirLocked that pass in write when read
// suffices (e.g., getEntry always passes in write).
func (fbo *folderBranchOps) getDirLocked(ctx context.Context,
	lState *lockState, md *RootMetadata, dir path, rtype mdReqType) (
	*DirBlock, error) {
	// Callers should have already done this check, but it doesn't
	// hurt to do it again.
	if !dir.isValid() {
		return nil, InvalidPathError{dir}
	}

	// Get the block for the last element in the path.
	dblock, err := fbo.getDirBlockHelperLocked(
		ctx, lState, md, dir.tailPointer(), dir.Branch, dir)
	if err != nil {
		return nil, err
	}

	if rtype == mdWrite && !fbo.config.BlockCache().IsDirty(
		dir.tailPointer(), dir.Branch) {
		// Copy the block if it's for writing and the block is
		// not yet dirty.
		dblock = dblock.DeepCopy()
	}
	return dblock, nil
}

// stripBP removes the Writer from the BlockPointer, in case it
// changes as part of a write/truncate operation before the blocks are
// sync'd.
func stripBP(ptr BlockPointer) BlockPointer {
	return BlockPointer{
		ID:       ptr.ID,
		RefNonce: ptr.RefNonce,
		KeyGen:   ptr.KeyGen,
		DataVer:  ptr.DataVer,
		Creator:  ptr.Creator,
	}
}

func (fbo *folderBranchOps) updateDirBlock(ctx context.Context,
	lState *lockState, dir path, block *DirBlock) *DirBlock {
	// see if this directory has any outstanding writes/truncates that
	// require an updated DirEntry
	fbo.cacheLock.Lock()
	defer fbo.cacheLock.Unlock()
	deMap, ok := fbo.deCache[stripBP(dir.tailPointer())]
	if ok {
		// do a deep copy, replacing direntries as we go
		dblockCopy := NewDirBlock().(*DirBlock)
		*dblockCopy = *block
		dblockCopy.Children = make(map[string]DirEntry)
		for k, v := range block.Children {
			if de, ok := deMap[stripBP(v.BlockPointer)]; ok {
				// We have a local copy update to the block, so set
				// ourselves to be writer, if possible.  If there's an
				// error, just log it and keep going because having
				// the correct Writer is not important enough to fail
				// the whole lookup.
				uid, err := fbo.config.KBPKI().GetCurrentUID(ctx)
				if err != nil {
					fbo.log.CDebugf(ctx, "Ignoring error while getting "+
						"logged-in user during directory entry lookup: %v", err)
				} else {
					de.SetWriter(uid)
				}

				dblockCopy.Children[k] = de
			} else {
				dblockCopy.Children[k] = v
			}
		}
		return dblockCopy
	}
	return block
}

// pathFromNodeHelper() shouldn't be called except by the helper
// functions below.
func (fbo *folderBranchOps) pathFromNodeHelper(n Node) (path, error) {
	p := fbo.nodeCache.PathFromNode(n)
	if !p.isValid() {
		return path{}, InvalidPathError{p}
	}
	return p, nil
}

// Helper functions to clarify uses of pathFromNodeHelper() (see
// nodeCache comments).

func (fbo *folderBranchOps) pathFromNodeForRead(n Node) (path, error) {
	return fbo.pathFromNodeHelper(n)
}

// blockLock must be held by the caller.
func (fbo *folderBranchOps) pathFromNodeForWriteLocked(n Node) (path, error) {
	return fbo.pathFromNodeHelper(n)
}

// mdWriterLock must be held by the caller.
func (fbo *folderBranchOps) pathFromNodeForMDWriteLocked(n Node) (path, error) {
	return fbo.pathFromNodeHelper(n)
}

func (fbo *folderBranchOps) GetDirChildren(ctx context.Context, dir Node) (
	children map[string]EntryInfo, err error) {
	fbo.log.CDebugf(ctx, "GetDirChildren %p", dir.GetID())
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	err = fbo.checkNode(dir)
	if err != nil {
		return
	}

	lState := makeFBOLockState()

	md, err := fbo.getMDForReadNeedIdentify(ctx, lState)
	if err != nil {
		return nil, err
	}

	dirPath, err := fbo.pathFromNodeForRead(dir)
	if err != nil {
		return
	}

	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	dblock, err := fbo.getDirLocked(ctx, lState, md, dirPath, mdReadNeedIdentify)
	if err != nil {
		return
	}

	dblock = fbo.updateDirBlock(ctx, lState, dirPath, dblock)

	children = make(map[string]EntryInfo)
	for k, de := range dblock.Children {
		children[k] = de.EntryInfo
	}
	return
}

// blockLock must be taken for reading by the caller. file must have
// a valid parent.
func (fbo *folderBranchOps) getEntryLocked(ctx context.Context,
	lState *lockState, md *RootMetadata, file path) (
	*DirBlock, DirEntry, error) {
	if !file.hasValidParent() {
		return nil, DirEntry{}, InvalidParentPathError{file}
	}

	parentPath := file.parentPath()
	dblock, err := fbo.getDirLocked(ctx, lState, md, *parentPath, mdWrite)
	if err != nil {
		return nil, DirEntry{}, err
	}

	dblock = fbo.updateDirBlock(ctx, lState, *parentPath, dblock)

	// make sure it exists
	name := file.tailName()
	de, ok := dblock.Children[name]
	if !ok {
		return nil, DirEntry{}, NoSuchNameError{name}
	}

	return dblock, de, err
}

func (fbo *folderBranchOps) getEntry(ctx context.Context, lState *lockState,
	md *RootMetadata, file path) (*DirBlock, DirEntry, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	return fbo.getEntryLocked(ctx, lState, md, file)
}

func (fbo *folderBranchOps) Lookup(ctx context.Context, dir Node, name string) (
	node Node, ei EntryInfo, err error) {
	fbo.log.CDebugf(ctx, "Lookup %p %s", dir.GetID(), name)
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	err = fbo.checkNode(dir)
	if err != nil {
		return nil, EntryInfo{}, err
	}

	lState := makeFBOLockState()

	md, err := fbo.getMDForReadNeedIdentify(ctx, lState)
	if err != nil {
		return nil, EntryInfo{}, err
	}

	dirPath, err := fbo.pathFromNodeForRead(dir)
	if err != nil {
		return nil, EntryInfo{}, err
	}

	childPath := dirPath.ChildPathNoPtr(name)

	_, de, err := fbo.getEntry(ctx, lState, md, childPath)
	if err != nil {
		return nil, EntryInfo{}, err
	}

	if de.Type == Sym {
		node = nil
	} else {
		err = fbo.checkDataVersion(childPath, de.BlockPointer)
		if err != nil {
			return nil, EntryInfo{}, err
		}

		node, err = fbo.nodeCache.GetOrCreate(de.BlockPointer, name, dir)
		if err != nil {
			return nil, EntryInfo{}, err
		}

	}

	return node, de.EntryInfo, nil
}

func (fbo *folderBranchOps) Stat(ctx context.Context, node Node) (
	ei EntryInfo, err error) {
	fbo.log.CDebugf(ctx, "Stat %p", node.GetID())
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	de, err := fbo.statEntry(ctx, node)
	if err != nil {
		return EntryInfo{}, err
	}
	return de.EntryInfo, nil
}

// statEntry is like Stat, but it returns a DirEntry. This is used by
// tests.
func (fbo *folderBranchOps) statEntry(ctx context.Context, node Node) (
	de DirEntry, err error) {
	err = fbo.checkNode(node)
	if err != nil {
		return DirEntry{}, err
	}

	lState := makeFBOLockState()

	nodePath, err := fbo.pathFromNodeForRead(node)
	if err != nil {
		return DirEntry{}, err
	}

	var md *RootMetadata
	if nodePath.hasValidParent() {
		md, err = fbo.getMDForReadNeedIdentify(ctx, lState)
	} else {
		// If nodePath has no valid parent, it's just the TLF
		// root, so we don't need an identify in this case.
		md, err = fbo.getMDForReadNoIdentify(ctx, lState)
	}
	if err != nil {
		return DirEntry{}, err
	}

	if nodePath.hasValidParent() {
		_, de, err = fbo.getEntry(ctx, lState, md, nodePath)
		if err != nil {
			return DirEntry{}, err
		}

	} else {
		// nodePath is just the root.
		de = md.data.Dir
	}

	return de, nil
}

var zeroPtr BlockPointer

type blockState struct {
	blockPtr       BlockPointer
	block          Block
	readyBlockData ReadyBlockData
}

// blockPutState is an internal structure to track data when putting blocks
type blockPutState struct {
	blockStates []blockState
}

func newBlockPutState(length int) *blockPutState {
	bps := &blockPutState{}
	bps.blockStates = make([]blockState, 0, length)
	return bps
}

func (bps *blockPutState) addNewBlock(blockPtr BlockPointer, block Block,
	readyBlockData ReadyBlockData) {
	bps.blockStates = append(bps.blockStates,
		blockState{blockPtr, block, readyBlockData})
}

func (bps *blockPutState) mergeOtherBps(other *blockPutState) {
	bps.blockStates = append(bps.blockStates, other.blockStates...)
}

func (fbo *folderBranchOps) readyBlock(ctx context.Context, md *RootMetadata,
	block Block, uid keybase1.UID) (
	info BlockInfo, plainSize int, readyBlockData ReadyBlockData, err error) {
	var ptr BlockPointer
	if fBlock, ok := block.(*FileBlock); ok && !fBlock.IsInd {
		// first see if we are duplicating any known blocks in this folder
		ptr, err = fbo.config.BlockCache().CheckForKnownPtr(fbo.id(), fBlock)
		if err != nil {
			return
		}
	}

	// Ready the block, even in the case where we can reuse an
	// existing block, just so that we know what the size of the
	// encrypted data will be.
	id, plainSize, readyBlockData, err :=
		fbo.config.BlockOps().Ready(ctx, md, block)
	if err != nil {
		return
	}

	if ptr.IsInitialized() {
		ptr.RefNonce, err = fbo.config.Crypto().MakeBlockRefNonce()
		if err != nil {
			return
		}
		ptr.SetWriter(uid)
	} else {
		ptr = BlockPointer{
			ID:       id,
			KeyGen:   md.LatestKeyGeneration(),
			DataVer:  fbo.config.DataVersion(),
			Creator:  uid,
			RefNonce: zeroBlockRefNonce,
		}
	}

	info = BlockInfo{
		BlockPointer: ptr,
		EncodedSize:  uint32(readyBlockData.GetEncodedSize()),
	}
	return
}

func (fbo *folderBranchOps) readyBlockMultiple(ctx context.Context,
	md *RootMetadata, currBlock Block, uid keybase1.UID, bps *blockPutState) (
	info BlockInfo, plainSize int, err error) {
	info, plainSize, readyBlockData, err :=
		fbo.readyBlock(ctx, md, currBlock, uid)
	if err != nil {
		return
	}

	bps.addNewBlock(info.BlockPointer, currBlock, readyBlockData)
	return
}

func (fbo *folderBranchOps) unembedBlockChanges(
	ctx context.Context, bps *blockPutState, md *RootMetadata,
	changes *BlockChanges, uid keybase1.UID) (err error) {
	buf, err := fbo.config.Codec().Encode(changes)
	if err != nil {
		return
	}
	block := NewFileBlock().(*FileBlock)
	block.Contents = buf
	info, _, err := fbo.readyBlockMultiple(ctx, md, block, uid, bps)
	if err != nil {
		return
	}
	md.data.cachedChanges = *changes
	changes.Info = info
	changes.Ops = nil
	md.RefBytes += uint64(info.EncodedSize)
	md.DiskUsage += uint64(info.EncodedSize)
	return
}

// cacheBlockIfNotYetDirtyLocked puts a block into the cache, but only
// does so if the block isn't already marked as dirty in the cache.
// This is useful when operating on a dirty copy of a block that may
// already be in the cache.
//
// blockLock should be taken by the caller for writing.
func (fbo *folderBranchOps) cacheBlockIfNotYetDirtyLocked(
	ptr BlockPointer, branch BranchName, block Block) error {
	if !fbo.config.BlockCache().IsDirty(ptr, branch) {
		return fbo.config.BlockCache().PutDirty(ptr, branch, block)
	}

	switch fbo.fileBlockStates[ptr] {
	case blockNotBeingSynced:
		// Nothing to do
	case blockSyncingNotDirty:
		// Overwrite the dirty block if this is a copy-on-write during
		// a sync.  Don't worry, the old dirty block is safe in the
		// sync goroutine (and also probably saved to the cache under
		// its new ID already.
		err := fbo.config.BlockCache().PutDirty(ptr, branch, block)
		if err != nil {
			return err
		}
		// Future writes can use this same block.
		fbo.fileBlockStates[ptr] = blockSyncingAndDirty
		fbo.doDeferWrite = true
	case blockSyncingAndDirty:
		fbo.doDeferWrite = true
	}

	return nil
}

type localBcache map[BlockPointer]*DirBlock

// syncBlock updates, and readies, the blocks along the path for the
// given write, up to the root of the tree or stopAt (if specified).
// When it updates the root of the tree, it also modifies the given
// head object with a new revision number and root block ID.  It first
// checks the provided lbc for blocks that may have been modified by
// previous syncBlock calls or the FS calls themselves.  It returns
// the updated path to the changed directory, the new or updated
// directory entry created as part of the call, and a summary of all
// the blocks that now must be put to the block server.
//
// entryType must not be Sym.
//
// TODO: deal with multiple nodes for indirect blocks
func (fbo *folderBranchOps) syncBlock(
	ctx context.Context, lState *lockState, uid keybase1.UID,
	md *RootMetadata, newBlock Block, dir path, name string,
	entryType EntryType, mtime bool, ctime bool, stopAt BlockPointer,
	lbc localBcache) (
	path, DirEntry, *blockPutState, error) {
	// now ready each dblock and write the DirEntry for the next one
	// in the path
	currBlock := newBlock
	currName := name
	newPath := path{
		FolderBranch: dir.FolderBranch,
		path:         make([]pathNode, 0, len(dir.path)),
	}
	bps := newBlockPutState(len(dir.path))
	refPath := dir.ChildPathNoPtr(name)
	var newDe DirEntry
	doSetTime := true
	now := fbo.nowUnixNano()
	for len(newPath.path) < len(dir.path)+1 {
		info, plainSize, err :=
			fbo.readyBlockMultiple(ctx, md, currBlock, uid, bps)
		if err != nil {
			return path{}, DirEntry{}, nil, err
		}

		// prepend to path and setup next one
		newPath.path = append([]pathNode{{info.BlockPointer, currName}},
			newPath.path...)

		// get the parent block
		prevIdx := len(dir.path) - len(newPath.path)
		var prevDblock *DirBlock
		var de DirEntry
		var nextName string
		nextDoSetTime := false
		if prevIdx < 0 {
			// root dir, update the MD instead
			de = md.data.Dir
		} else {
			prevDir := path{
				FolderBranch: dir.FolderBranch,
				path:         dir.path[:prevIdx+1],
			}

			// First, check the localBcache, which could contain
			// blocks that were modified across multiple calls to
			// syncBlock.
			var ok bool
			prevDblock, ok = lbc[prevDir.tailPointer()]
			if !ok {
				prevDblock, err = func() (*DirBlock, error) {
					// If the block isn't in the local bcache, we have to
					// fetch it, possibly from the network.  Take
					// blockLock to make this safe, but we don't need to
					// hold it throughout the entire syncBlock execution
					// because we are only fetching directory blocks.
					// Directory blocks are only ever modified while
					// holding mdWriterLock, so it's safe to release the
					// blockLock in between fetches.
					fbo.blockLock.RLock(lState)
					defer fbo.blockLock.RUnlock(lState)
					return fbo.getDirLocked(ctx, lState, md, prevDir, mdWrite)
				}()
				if err != nil {
					return path{}, DirEntry{}, nil, err
				}
			}

			// modify the direntry for currName; make one
			// if it doesn't exist (which should only
			// happen the first time around).
			//
			// TODO: Pull the creation out of here and
			// into createEntryLocked().
			if de, ok = prevDblock.Children[currName]; !ok {
				// If this isn't the first time
				// around, we have an error.
				if len(newPath.path) > 1 {
					return path{}, DirEntry{}, nil, NoSuchNameError{currName}
				}

				// If this is a file, the size should be 0. (TODO:
				// Ensure this.) If this is a directory, the size will
				// be filled in below.  The times will be filled in
				// below as well, since we should only be creating a
				// new directory entry when doSetTime is true.
				de = DirEntry{
					EntryInfo: EntryInfo{
						Type: entryType,
						Size: 0,
					},
				}
				// If we're creating a new directory entry, the
				// parent's times must be set as well.
				nextDoSetTime = true
			}

			currBlock = prevDblock
			nextName = prevDir.tailName()
		}

		if de.Type == Dir {
			// TODO: When we use indirect dir blocks,
			// we'll have to calculate the size some other
			// way.
			de.Size = uint64(plainSize)
		}

		if prevIdx < 0 {
			md.AddUpdate(md.data.Dir.BlockInfo, info)
		} else if prevDe, ok := prevDblock.Children[currName]; ok {
			md.AddUpdate(prevDe.BlockInfo, info)
		} else {
			// this is a new block
			md.AddRefBlock(info)
		}

		if len(refPath.path) > 1 {
			refPath = *refPath.parentPath()
		}
		de.BlockInfo = info

		if doSetTime {
			if mtime {
				de.Mtime = now
			}
			if ctime {
				de.Ctime = now
			}
		}
		if !newDe.IsInitialized() {
			newDe = de
		}

		if prevIdx < 0 {
			md.data.Dir = de
		} else {
			prevDblock.Children[currName] = de
		}
		currName = nextName

		// Stop before we get to the common ancestor; it will be taken care of
		// on the next sync call
		if prevIdx >= 0 && dir.path[prevIdx].BlockPointer == stopAt {
			// Put this back into the cache as dirty -- the next
			// syncBlock call will ready it.
			dblock, ok := currBlock.(*DirBlock)
			if !ok {
				return path{}, DirEntry{}, nil, BadDataError{stopAt.ID}
			}
			lbc[stopAt] = dblock
			break
		}
		doSetTime = nextDoSetTime
	}

	return newPath, newDe, bps, nil
}

// entryType must not be Sym.
func (fbo *folderBranchOps) syncBlockAndCheckEmbed(ctx context.Context,
	lState *lockState, md *RootMetadata, newBlock Block, dir path,
	name string, entryType EntryType, mtime bool, ctime bool,
	stopAt BlockPointer, lbc localBcache) (
	path, DirEntry, *blockPutState, error) {
	uid, err := fbo.config.KBPKI().GetCurrentUID(ctx)
	if err != nil {
		return path{}, DirEntry{}, nil, err
	}

	newPath, newDe, bps, err := fbo.syncBlock(
		ctx, lState, uid, md, newBlock, dir, name, entryType, mtime,
		ctime, stopAt, lbc)
	if err != nil {
		return path{}, DirEntry{}, nil, err
	}

	// do the block changes need their own blocks?
	bsplit := fbo.config.BlockSplitter()
	if !bsplit.ShouldEmbedBlockChanges(&md.data.Changes) {
		err = fbo.unembedBlockChanges(ctx, bps, md, &md.data.Changes,
			uid)
		if err != nil {
			return path{}, DirEntry{}, nil, err
		}
	}

	return newPath, newDe, bps, nil
}

func (fbo *folderBranchOps) doOneBlockPut(ctx context.Context,
	md *RootMetadata, blockState blockState,
	errChan chan error) {
	err := fbo.config.BlockOps().
		Put(ctx, md, blockState.blockPtr, blockState.readyBlockData)
	if err != nil {
		// one error causes everything else to cancel
		select {
		case errChan <- err:
		default:
			return
		}
	}
}

// doBlockPuts writes all the pending block puts to the cache and
// server.
func (fbo *folderBranchOps) doBlockPuts(ctx context.Context,
	md *RootMetadata, bps blockPutState) error {
	errChan := make(chan error, 1)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	blocks := make(chan blockState, len(bps.blockStates))
	var wg sync.WaitGroup

	numWorkers := len(bps.blockStates)
	if numWorkers > maxParallelBlockPuts {
		numWorkers = maxParallelBlockPuts
	}
	wg.Add(numWorkers)

	worker := func() {
		defer wg.Done()
		for blockState := range blocks {
			fbo.doOneBlockPut(ctx, md, blockState, errChan)
			select {
			// return early if the context has been canceled
			case <-ctx.Done():
				return
			default:
			}
		}
	}
	for i := 0; i < numWorkers; i++ {
		go worker()
	}

	for _, blockState := range bps.blockStates {
		blocks <- blockState
	}
	close(blocks)

	go func() {
		wg.Wait()
		close(errChan)
	}()
	return <-errChan
}

func (fbo *folderBranchOps) finalizeBlocks(bps *blockPutState) error {
	bcache := fbo.config.BlockCache()
	for _, blockState := range bps.blockStates {
		newPtr := blockState.blockPtr
		// only cache this block if we made a brand new block, not if
		// we just incref'd some other block.
		if !newPtr.IsFirstRef() {
			continue
		}
		if err := bcache.Put(newPtr, fbo.id(), blockState.block,
			TransientEntry); err != nil {
			return err
		}
	}
	return nil
}

// Returns true if the passed error indicates a reviion conflict.
func (fbo *folderBranchOps) isRevisionConflict(err error) bool {
	if err == nil {
		return false
	}
	_, isConflictRevision := err.(MDServerErrorConflictRevision)
	_, isConflictPrevRoot := err.(MDServerErrorConflictPrevRoot)
	_, isConflictDiskUsage := err.(MDServerErrorConflictDiskUsage)
	_, isConditionFailed := err.(MDServerErrorConditionFailed)
	return isConflictRevision || isConflictPrevRoot ||
		isConflictDiskUsage || isConditionFailed
}

// mdWriterLock must be held by the caller.
func (fbo *folderBranchOps) archiveLocked(md *RootMetadata) {
	// Turn off block archiving temporarily: KBFS-641
	/**
	fbo.archiveGroup.Add(1)
	fbo.archiveChan <- md
	*/
}

// mdWriterLock must be taken by the caller.
func (fbo *folderBranchOps) finalizeMDWriteLocked(ctx context.Context,
	lState *lockState, md *RootMetadata, bps *blockPutState) (err error) {

	// finally, write out the new metadata
	mdops := fbo.config.MDOps()

	doUnmergedPut, wasStaged := true, fbo.staged
	mergedRev := MetadataRevisionUninitialized

	if !fbo.staged {
		// only do a normal Put if we're not already staged.
		err = mdops.Put(ctx, md)
		doUnmergedPut = fbo.isRevisionConflict(err)
		if err != nil && !doUnmergedPut {
			return err
		}
		// The first time we transition, our last known MD revision is
		// the same (at least) as what we thought our new revision
		// should be.  Otherwise, just leave it at uninitialized and
		// let the resolver sort it out.
		if doUnmergedPut {
			fbo.log.CDebugf(ctx, "Conflict: %v", err)
			mergedRev = md.Revision
		}
	}

	if doUnmergedPut {
		// We're out of date, so put it as an unmerged MD.
		var bid BranchID
		if !wasStaged {
			// new branch ID
			crypto := fbo.config.Crypto()
			if bid, err = crypto.MakeRandomBranchID(); err != nil {
				return err
			}
		} else {
			bid = fbo.bid
		}
		err := mdops.PutUnmerged(ctx, md, bid)
		if err != nil {
			return nil
		}
		fbo.setStagedLocked(lState, true, bid)
		fbo.cr.Resolve(md.Revision, mergedRev)
	} else {
		if fbo.staged {
			// If we were staged, prune all unmerged history now
			err = fbo.config.MDServer().PruneBranch(ctx, fbo.id(), fbo.bid)
			if err != nil {
				return err
			}
		}

		fbo.setStagedLocked(lState, false, NullBranchID)

		if md.IsRekeySet() && !md.IsWriterMetadataCopiedSet() {
			// Queue this folder for rekey if the bit was set and it's not a copy.
			// This is for the case where we're coming out of conflict resolution.
			// So why don't we do this in finalizeResolution? Well, we do but we don't
			// want to block on a rekey so we queue it. Because of that it may fail
			// due to a conflict with some subsequent write. By also handling it here
			// we'll always retry if we notice we haven't been successful in clearing
			// the bit yet. Note that I haven't actually seen this happen but it seems
			// theoretically possible.
			defer fbo.config.RekeyQueue().Enqueue(md.ID)
		}
	}
	// Swap any cached block changes so that future local accesses to
	// this MD (from the cache) can directly access the ops without
	// needing to re-embed the block changes.
	if md.data.Changes.Ops == nil {
		md.data.Changes, md.data.cachedChanges =
			md.data.cachedChanges, md.data.Changes
		md.data.Changes.Ops[0].
			AddRefBlock(md.data.cachedChanges.Info.BlockPointer)
	}
	fbo.transitionState(cleanState)

	err = fbo.finalizeBlocks(bps)
	if err != nil {
		return err
	}

	fbo.headLock.Lock(lState)
	defer fbo.headLock.Unlock(lState)
	err = fbo.setHeadLocked(ctx, lState, md)
	if err != nil {
		return err
	}

	// Archive the old, unref'd blocks
	fbo.archiveLocked(md)

	fbo.notifyBatchLocked(ctx, lState, md)
	return nil
}

// mdWriterLock must be taken by the caller, but not blockLock
func (fbo *folderBranchOps) syncBlockAndFinalizeLocked(ctx context.Context,
	lState *lockState, md *RootMetadata, newBlock Block, dir path,
	name string, entryType EntryType, mtime bool, ctime bool,
	stopAt BlockPointer) (DirEntry, error) {
	_, de, bps, err := fbo.syncBlockAndCheckEmbed(
		ctx, lState, md, newBlock, dir, name, entryType, mtime,
		ctime, zeroPtr, nil)
	if err != nil {
		return DirEntry{}, err
	}
	err = fbo.doBlockPuts(ctx, md, *bps)
	if err != nil {
		// TODO: in theory we could recover from a
		// IncrementMissingBlockError.  We would have to delete the
		// offending block from our cache and re-doing ALL of the
		// block ready calls.
		return DirEntry{}, err
	}
	err = fbo.finalizeMDWriteLocked(ctx, lState, md, bps)
	if err != nil {
		return DirEntry{}, err
	}
	return de, nil
}

func checkDisallowedPrefixes(name string) error {
	for _, prefix := range disallowedPrefixes {
		if strings.HasPrefix(name, prefix) {
			return DisallowedPrefixError{name, prefix}
		}
	}
	return nil
}

func (fbo *folderBranchOps) checkNewDirSize(ctx context.Context,
	lState *lockState, md *RootMetadata, dirPath path, newName string) error {
	// Check that the directory isn't past capacity already.
	var currSize uint64
	if dirPath.hasValidParent() {
		_, de, err := fbo.getEntry(ctx, lState, md, dirPath)
		if err != nil {
			return err
		}
		currSize = de.Size
	} else {
		// dirPath is just the root.
		currSize = md.data.Dir.Size
	}
	// Just an approximation since it doesn't include the size of the
	// directory entry itself, but that's ok -- at worst it'll be an
	// off-by-one-entry error, and since there's a maximum name length
	// we can't get in too much trouble.
	if currSize+uint64(len(newName)) > fbo.config.MaxDirBytes() {
		return DirTooBigError{dirPath, currSize + uint64(len(newName)),
			fbo.config.MaxDirBytes()}
	}
	return nil
}

// entryType must not by Sym.  mdWriterLock must be taken by caller.
func (fbo *folderBranchOps) createEntryLocked(
	ctx context.Context, lState *lockState, dir Node, name string,
	entryType EntryType) (Node, DirEntry, error) {
	if err := checkDisallowedPrefixes(name); err != nil {
		return nil, DirEntry{}, err
	}

	if uint32(len(name)) > fbo.config.MaxNameBytes() {
		return nil, DirEntry{},
			NameTooLongError{name, fbo.config.MaxNameBytes()}
	}

	// verify we have permission to write
	md, err := fbo.getMDForWriteLocked(ctx, lState)
	if err != nil {
		return nil, DirEntry{}, err
	}

	dirPath, err := fbo.pathFromNodeForMDWriteLocked(dir)
	if err != nil {
		return nil, DirEntry{}, err
	}

	dblock, err := func() (*DirBlock, error) {
		fbo.blockLock.RLock(lState)
		defer fbo.blockLock.RUnlock(lState)

		dblock, err := fbo.getDirLocked(ctx, lState, md, dirPath, mdWrite)
		if err != nil {
			return nil, err
		}
		return dblock, nil
	}()
	if err != nil {
		return nil, DirEntry{}, err
	}

	// does name already exist?
	if _, ok := dblock.Children[name]; ok {
		return nil, DirEntry{}, NameExistsError{name}
	}

	if err := fbo.checkNewDirSize(ctx, lState, md, dirPath, name); err != nil {
		return nil, DirEntry{}, err
	}

	md.AddOp(newCreateOp(name, dirPath.tailPointer(), entryType))
	// create new data block
	var newBlock Block
	// XXX: for now, put a unique ID in every new block, to make sure it
	// has a unique block ID. This may not be needed once we have encryption.
	if entryType == Dir {
		newBlock = &DirBlock{
			Children: make(map[string]DirEntry),
		}
	} else {
		newBlock = &FileBlock{}
	}

	de, err := fbo.syncBlockAndFinalizeLocked(
		ctx, lState, md, newBlock, dirPath, name, entryType,
		true, true, zeroPtr)
	if err != nil {
		return nil, DirEntry{}, err
	}
	node, err := fbo.nodeCache.GetOrCreate(de.BlockPointer, name, dir)
	if err != nil {
		return nil, DirEntry{}, err
	}
	return node, de, nil
}

func (fbo *folderBranchOps) CreateDir(
	ctx context.Context, dir Node, path string) (
	n Node, ei EntryInfo, err error) {
	fbo.log.CDebugf(ctx, "CreateDir %p %s", dir.GetID(), path)
	defer func() {
		if err != nil {
			fbo.log.CDebugf(ctx, "Error: %v", err)
		} else {
			fbo.log.CDebugf(ctx, "Done: %p", n.GetID())
		}
	}()

	err = fbo.checkNode(dir)
	if err != nil {
		return nil, EntryInfo{}, err
	}

	lState := makeFBOLockState()

	fbo.mdWriterLock.Lock(lState)
	defer fbo.mdWriterLock.Unlock(lState)
	n, de, err := fbo.createEntryLocked(ctx, lState, dir, path, Dir)
	if err != nil {
		return nil, EntryInfo{}, err
	}

	return n, de.EntryInfo, nil
}

func (fbo *folderBranchOps) CreateFile(
	ctx context.Context, dir Node, path string, isExec bool) (
	n Node, ei EntryInfo, err error) {
	fbo.log.CDebugf(ctx, "CreateFile %p %s", dir.GetID(), path)
	defer func() {
		if err != nil {
			fbo.log.CDebugf(ctx, "Error: %v", err)
		} else {
			fbo.log.CDebugf(ctx, "Done: %p", n.GetID())
		}
	}()

	err = fbo.checkNode(dir)
	if err != nil {
		return nil, EntryInfo{}, err
	}

	var entryType EntryType
	if isExec {
		entryType = Exec
	} else {
		entryType = File
	}

	lState := makeFBOLockState()

	fbo.mdWriterLock.Lock(lState)
	defer fbo.mdWriterLock.Unlock(lState)
	n, de, err := fbo.createEntryLocked(ctx, lState, dir, path, entryType)
	if err != nil {
		return nil, EntryInfo{}, err
	}

	return n, de.EntryInfo, nil
}

// mdWriterLock must be taken by caller.
func (fbo *folderBranchOps) createLinkLocked(
	ctx context.Context, lState *lockState, dir Node, fromName string,
	toPath string) (DirEntry, error) {
	if err := checkDisallowedPrefixes(fromName); err != nil {
		return DirEntry{}, err
	}

	if uint32(len(fromName)) > fbo.config.MaxNameBytes() {
		return DirEntry{},
			NameTooLongError{fromName, fbo.config.MaxNameBytes()}
	}

	// verify we have permission to write
	md, err := fbo.getMDForWriteLocked(ctx, lState)
	if err != nil {
		return DirEntry{}, err
	}

	dirPath, err := fbo.pathFromNodeForMDWriteLocked(dir)
	if err != nil {
		return DirEntry{}, err
	}

	dblock, err := func() (*DirBlock, error) {
		fbo.blockLock.RLock(lState)
		defer fbo.blockLock.RUnlock(lState)
		return fbo.getDirLocked(ctx, lState, md, dirPath, mdWrite)
	}()
	if err != nil {
		return DirEntry{}, err
	}

	// TODO: validate inputs

	// does name already exist?
	if _, ok := dblock.Children[fromName]; ok {
		return DirEntry{}, NameExistsError{fromName}
	}

	if err := fbo.checkNewDirSize(ctx, lState, md,
		dirPath, fromName); err != nil {
		return DirEntry{}, err
	}

	md.AddOp(newCreateOp(fromName, dirPath.tailPointer(), Sym))

	// Create a direntry for the link, and then sync
	now := fbo.nowUnixNano()
	dblock.Children[fromName] = DirEntry{
		EntryInfo: EntryInfo{
			Type:    Sym,
			Size:    uint64(len(toPath)),
			SymPath: toPath,
			Mtime:   now,
			Ctime:   now,
		},
	}

	_, err = fbo.syncBlockAndFinalizeLocked(
		ctx, lState, md, dblock, *dirPath.parentPath(),
		dirPath.tailName(), Dir, true, true, zeroPtr)
	if err != nil {
		return DirEntry{}, err
	}
	return dblock.Children[fromName], nil
}

func (fbo *folderBranchOps) CreateLink(
	ctx context.Context, dir Node, fromName string, toPath string) (
	ei EntryInfo, err error) {
	fbo.log.CDebugf(ctx, "CreateLink %p %s -> %s",
		dir.GetID(), fromName, toPath)
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	err = fbo.checkNode(dir)
	if err != nil {
		return EntryInfo{}, err
	}

	lState := makeFBOLockState()

	fbo.mdWriterLock.Lock(lState)
	defer fbo.mdWriterLock.Unlock(lState)
	de, err := fbo.createLinkLocked(ctx, lState, dir, fromName, toPath)
	if err != nil {
		return EntryInfo{}, err
	}
	return de.EntryInfo, nil
}

// unrefEntry modifies md to unreference all relevant blocks for the
// given entry.
func (fbo *folderBranchOps) unrefEntry(ctx context.Context,
	lState *lockState, md *RootMetadata, dir path, de DirEntry,
	name string) error {
	md.AddUnrefBlock(de.BlockInfo)
	// construct a path for the child so we can unlink with it.
	childPath := dir.ChildPath(name, de.BlockPointer)

	// If this is an indirect block, we need to delete all of its
	// children as well. (TODO: handle multiple levels of
	// indirection.)  NOTE: non-empty directories can't be removed, so
	// no need to check for indirect directory blocks here.
	if de.Type == File || de.Type == Exec {
		fBlock, err := func() (*FileBlock, error) {
			fbo.blockLock.RLock(lState)
			defer fbo.blockLock.RUnlock(lState)
			return fbo.getFileBlockHelperLocked(
				ctx, lState, md, childPath.tailPointer(),
				childPath.Branch, childPath)
		}()
		if err != nil {
			return NoSuchBlockError{de.ID}
		}
		if fBlock.IsInd {
			for _, ptr := range fBlock.IPtrs {
				md.AddUnrefBlock(ptr.BlockInfo)
			}
		}
	}
	return nil
}

// mdWriterLock must be taken by caller.
func (fbo *folderBranchOps) removeEntryLocked(ctx context.Context,
	lState *lockState, md *RootMetadata, dir path, name string) error {
	pblock, err := func() (*DirBlock, error) {
		fbo.blockLock.RLock(lState)
		defer fbo.blockLock.RUnlock(lState)
		return fbo.getDirLocked(ctx, lState, md, dir, mdWrite)
	}()
	if err != nil {
		return err
	}

	// make sure the entry exists
	de, ok := pblock.Children[name]
	if !ok {
		return NoSuchNameError{name}
	}

	md.AddOp(newRmOp(name, dir.tailPointer()))
	err = fbo.unrefEntry(ctx, lState, md, dir, de, name)
	if err != nil {
		return err
	}

	// the actual unlink
	delete(pblock.Children, name)

	// sync the parent directory
	_, err = fbo.syncBlockAndFinalizeLocked(
		ctx, lState, md, pblock, *dir.parentPath(), dir.tailName(),
		Dir, true, true, zeroPtr)
	if err != nil {
		return err
	}
	return nil
}

func (fbo *folderBranchOps) RemoveDir(
	ctx context.Context, dir Node, dirName string) (err error) {
	fbo.log.CDebugf(ctx, "RemoveDir %p %s", dir.GetID(), dirName)
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	err = fbo.checkNode(dir)
	if err != nil {
		return
	}

	lState := makeFBOLockState()

	fbo.mdWriterLock.Lock(lState)
	defer fbo.mdWriterLock.Unlock(lState)

	// verify we have permission to write
	md, err := fbo.getMDForWriteLocked(ctx, lState)
	if err != nil {
		return err
	}

	dirPath, err := fbo.pathFromNodeForMDWriteLocked(dir)
	if err != nil {
		return err
	}

	err = func() error {
		fbo.blockLock.RLock(lState)
		defer fbo.blockLock.RUnlock(lState)
		pblock, err := fbo.getDirLocked(ctx, lState, md, dirPath, mdReadNeedIdentify)
		de, ok := pblock.Children[dirName]
		if !ok {
			return NoSuchNameError{dirName}
		}

		// construct a path for the child so we can check for an empty dir
		childPath := dirPath.ChildPath(dirName, de.BlockPointer)

		childBlock, err := fbo.getDirLocked(ctx, lState, md, childPath, mdReadNeedIdentify)
		if err != nil {
			return err
		}

		if len(childBlock.Children) > 0 {
			return DirNotEmptyError{dirName}
		}
		return nil
	}()
	if err != nil {
		return err
	}

	return fbo.removeEntryLocked(ctx, lState, md, dirPath, dirName)
}

func (fbo *folderBranchOps) RemoveEntry(ctx context.Context, dir Node,
	name string) (err error) {
	fbo.log.CDebugf(ctx, "RemoveEntry %p %s", dir.GetID(), name)
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	err = fbo.checkNode(dir)
	if err != nil {
		return err
	}

	lState := makeFBOLockState()

	fbo.mdWriterLock.Lock(lState)
	defer fbo.mdWriterLock.Unlock(lState)

	// verify we have permission to write
	md, err := fbo.getMDForWriteLocked(ctx, lState)
	if err != nil {
		return err
	}

	dirPath, err := fbo.pathFromNodeForMDWriteLocked(dir)
	if err != nil {
		return err
	}

	return fbo.removeEntryLocked(ctx, lState, md, dirPath, name)
}

// mdWriterLock must be taken by caller.
func (fbo *folderBranchOps) renameLocked(
	ctx context.Context, lState *lockState, oldParent path,
	oldName string, newParent path, newName string,
	newParentNode Node) error {
	// verify we have permission to write
	md, err := fbo.getMDForWriteLocked(ctx, lState)
	if err != nil {
		return err
	}

	doUnlock := true
	fbo.blockLock.RLock(lState)
	defer func() {
		if doUnlock {
			fbo.blockLock.RUnlock(lState)
		}
	}()

	// look up in the old path
	oldPBlock, err := fbo.getDirLocked(ctx, lState, md, oldParent, mdWrite)
	if err != nil {
		return err
	}
	newDe, ok := oldPBlock.Children[oldName]
	// does the name exist?
	if !ok {
		return NoSuchNameError{oldName}
	}

	md.AddOp(newRenameOp(oldName, oldParent.tailPointer(), newName,
		newParent.tailPointer(), newDe.BlockPointer, newDe.Type))

	lbc := make(localBcache)
	// look up in the old path
	var newPBlock *DirBlock
	// TODO: Write a SameBlock() function that can deal properly with
	// dedup'd blocks that share an ID but can be updated separately.
	if oldParent.tailPointer().ID == newParent.tailPointer().ID {
		newPBlock = oldPBlock
	} else {
		newPBlock, err = fbo.getDirLocked(
			ctx, lState, md, newParent, mdWrite)
		if err != nil {
			return err
		}
		now := fbo.nowUnixNano()

		oldGrandparent := *oldParent.parentPath()
		if len(oldGrandparent.path) > 0 {
			// Update the old parent's mtime/ctime, unless the
			// oldGrandparent is the same as newParent (in which case, the
			// syncBlockAndCheckEmbed call will take care of it).
			if oldGrandparent.tailPointer().ID != newParent.tailPointer().ID {
				b, err := fbo.getDirLocked(ctx, lState, md, oldGrandparent, mdWrite)
				if err != nil {
					return err
				}
				if de, ok := b.Children[oldParent.tailName()]; ok {
					de.Ctime = now
					de.Mtime = now
					b.Children[oldParent.tailName()] = de
					// Put this block back into the local cache as dirty
					lbc[oldGrandparent.tailPointer()] = b
				}
			}
		} else {
			md.data.Dir.Ctime = now
			md.data.Dir.Mtime = now
		}
	}
	doUnlock = false
	fbo.blockLock.RUnlock(lState)

	// does name exist?
	if de, ok := newPBlock.Children[newName]; ok {
		if de.Type == Dir {
			fbo.log.CWarningf(ctx, "Renaming over a directory (%s/%s) is not "+
				"allowed.", newParent, newName)
			return NotFileError{newParent.ChildPathNoPtr(newName)}
		}

		// Delete the old block pointed to by this direntry.
		err := fbo.unrefEntry(ctx, lState, md, newParent, de, newName)
		if err != nil {
			return err
		}
	}

	// only the ctime changes
	newDe.Ctime = fbo.nowUnixNano()
	newPBlock.Children[newName] = newDe
	delete(oldPBlock.Children, oldName)

	// find the common ancestor
	var i int
	found := false
	// the root block will always be the same, so start at number 1
	for i = 1; i < len(oldParent.path) && i < len(newParent.path); i++ {
		if oldParent.path[i].ID != newParent.path[i].ID {
			found = true
			i--
			break
		}
	}
	if !found {
		// if we couldn't find one, then the common ancestor is the
		// last node in the shorter path
		if len(oldParent.path) < len(newParent.path) {
			i = len(oldParent.path) - 1
		} else {
			i = len(newParent.path) - 1
		}
	}
	commonAncestor := oldParent.path[i].BlockPointer
	oldIsCommon := oldParent.tailPointer() == commonAncestor
	newIsCommon := newParent.tailPointer() == commonAncestor

	newOldPath := path{FolderBranch: oldParent.FolderBranch}
	var oldBps *blockPutState
	if oldIsCommon {
		if newIsCommon {
			// if old and new are both the common ancestor, there is
			// nothing to do (syncBlock will take care of everything)
		} else {
			// If the old one is common and the new one is not, then
			// the last syncBlockAndCheckEmbed call will need to access
			// the old one.
			lbc[oldParent.tailPointer()] = oldPBlock
		}
	} else {
		if newIsCommon {
			// If the new one is common, then the first
			// syncBlockAndCheckEmbed call will need to access it.
			lbc[newParent.tailPointer()] = newPBlock
		}

		// The old one is not the common ancestor, so we need to sync it.
		// TODO: optimize by pushing blocks from both paths in parallel
		newOldPath, _, oldBps, err = fbo.syncBlockAndCheckEmbed(
			ctx, lState, md, oldPBlock, *oldParent.parentPath(), oldParent.tailName(),
			Dir, true, true, commonAncestor, lbc)
		if err != nil {
			return err
		}
	}

	newNewPath, _, newBps, err := fbo.syncBlockAndCheckEmbed(
		ctx, lState, md, newPBlock, *newParent.parentPath(), newParent.tailName(),
		Dir, true, true, zeroPtr, lbc)
	if err != nil {
		return err
	}

	// newOldPath is really just a prefix now.  A copy is necessary as an
	// append could cause the new path to contain nodes from the old path.
	newOldPath.path = append(make([]pathNode, i+1, i+1), newOldPath.path...)
	copy(newOldPath.path[:i+1], newNewPath.path[:i+1])

	// merge and finalize the blockPutStates
	if oldBps != nil {
		newBps.mergeOtherBps(oldBps)
	}

	err = fbo.doBlockPuts(ctx, md, *newBps)
	if err != nil {
		return err
	}

	return fbo.finalizeMDWriteLocked(ctx, lState, md, newBps)
}

func (fbo *folderBranchOps) Rename(
	ctx context.Context, oldParent Node, oldName string, newParent Node,
	newName string) (err error) {
	fbo.log.CDebugf(ctx, "Rename %p/%s -> %p/%s", oldParent.GetID(),
		oldName, newParent.GetID(), newName)
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	err = fbo.checkNode(newParent)
	if err != nil {
		return err
	}

	lState := makeFBOLockState()

	fbo.mdWriterLock.Lock(lState)
	defer fbo.mdWriterLock.Unlock(lState)

	oldParentPath, err := fbo.pathFromNodeForMDWriteLocked(oldParent)
	if err != nil {
		return err
	}

	newParentPath, err := fbo.pathFromNodeForMDWriteLocked(newParent)
	if err != nil {
		return err
	}

	// only works for paths within the same topdir
	if oldParentPath.FolderBranch != newParentPath.FolderBranch {
		return RenameAcrossDirsError{}
	}

	return fbo.renameLocked(ctx, lState, oldParentPath, oldName, newParentPath,
		newName, newParent)
}

// blockLock must be taken for reading by caller.
func (fbo *folderBranchOps) getFileBlockAtOffsetLocked(ctx context.Context,
	lState *lockState, md *RootMetadata, file path, topBlock *FileBlock,
	off int64, rtype mdReqType) (
	ptr BlockPointer, parentBlock *FileBlock, indexInParent int,
	block *FileBlock, more bool, startOff int64, err error) {
	// find the block matching the offset, if it exists
	ptr = file.tailPointer()
	block = topBlock
	more = false
	startOff = 0
	// search until it's not an indirect block
	for block.IsInd {
		nextIndex := len(block.IPtrs) - 1
		for i, ptr := range block.IPtrs {
			if ptr.Off == off {
				// small optimization to avoid iterating past the right ptr
				nextIndex = i
				break
			} else if ptr.Off > off {
				// i can never be 0, because the first ptr always has
				// an offset at the beginning of the range
				nextIndex = i - 1
				break
			}
		}
		nextPtr := block.IPtrs[nextIndex]
		parentBlock = block
		indexInParent = nextIndex
		startOff = nextPtr.Off
		// there is more to read if we ever took a path through a
		// ptr that wasn't the final ptr in its respective list
		more = more || (nextIndex != len(block.IPtrs)-1)
		ptr = nextPtr.BlockPointer
		if block, err = fbo.getFileBlockLocked(ctx, lState, md, ptr, file, rtype); err != nil {
			return
		}
	}

	return
}

// blockLock must be taken for reading by the caller
func (fbo *folderBranchOps) readLocked(
	ctx context.Context, lState *lockState, md *RootMetadata, file path,
	dest []byte, off int64) (int64, error) {
	// getFileLocked already checks read permissions
	fblock, err := fbo.getFileLocked(ctx, lState, md, file, mdReadNeedIdentify)
	if err != nil {
		return 0, err
	}

	nRead := int64(0)
	n := int64(len(dest))

	for nRead < n {
		nextByte := nRead + off
		toRead := n - nRead
		_, _, _, block, _, startOff, err := fbo.getFileBlockAtOffsetLocked(
			ctx, lState, md, file, fblock, nextByte, mdReadNeedIdentify)
		if err != nil {
			return 0, err
		}
		blockLen := int64(len(block.Contents))
		lastByteInBlock := startOff + blockLen

		if nextByte >= lastByteInBlock {
			return nRead, nil
		} else if toRead > lastByteInBlock-nextByte {
			toRead = lastByteInBlock - nextByte
		}

		firstByteToRead := nextByte - startOff
		copy(dest[nRead:nRead+toRead],
			block.Contents[firstByteToRead:toRead+firstByteToRead])
		nRead += toRead
	}

	return n, nil
}

func (fbo *folderBranchOps) Read(
	ctx context.Context, file Node, dest []byte, off int64) (
	n int64, err error) {
	fbo.log.CDebugf(ctx, "Read %p %d %d", file.GetID(), len(dest), off)
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	err = fbo.checkNode(file)
	if err != nil {
		return 0, err
	}

	lState := makeFBOLockState()

	// verify we have permission to read
	md, err := fbo.getMDForReadNeedIdentify(ctx, lState)
	if err != nil {
		return 0, err
	}

	filePath, err := fbo.pathFromNodeForRead(file)
	if err != nil {
		return 0, err
	}

	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	return fbo.readLocked(ctx, lState, md, filePath, dest, off)
}

// blockLock must be taken by the caller.
func (fbo *folderBranchOps) newRightBlockLocked(
	ctx context.Context, ptr BlockPointer, branch BranchName, pblock *FileBlock,
	off int64, md *RootMetadata) (BlockPointer, error) {
	newRID, err := fbo.config.Crypto().MakeTemporaryBlockID()
	if err != nil {
		return zeroPtr, err
	}
	uid, err := fbo.config.KBPKI().GetCurrentUID(ctx)
	if err != nil {
		return zeroPtr, err
	}
	rblock := &FileBlock{}

	newPtr := BlockPointer{
		ID:       newRID,
		KeyGen:   md.LatestKeyGeneration(),
		DataVer:  fbo.config.DataVersion(),
		Creator:  uid,
		RefNonce: zeroBlockRefNonce,
	}

	pblock.IPtrs = append(pblock.IPtrs, IndirectFilePtr{
		BlockInfo: BlockInfo{
			BlockPointer: newPtr,
			EncodedSize:  0,
		},
		Off: off,
	})

	if err := fbo.config.BlockCache().PutDirty(
		newPtr, branch, rblock); err != nil {
		return zeroPtr, err
	}

	if err = fbo.cacheBlockIfNotYetDirtyLocked(
		ptr, branch, pblock); err != nil {
		return zeroPtr, err
	}
	return newPtr, nil
}

// cacheLock must be taken by the caller
func (fbo *folderBranchOps) getOrCreateSyncInfoLocked(de DirEntry) *syncInfo {
	ptr := stripBP(de.BlockPointer)
	si, ok := fbo.unrefCache[ptr]
	if !ok {
		si = &syncInfo{
			oldInfo: de.BlockInfo,
			op:      newSyncOp(de.BlockPointer),
		}
		fbo.unrefCache[ptr] = si
	}
	return si
}

// blockLock must be taken for writing by the caller.  Returns the set
// of newly-ID'd blocks created during this write that might need to
// be cleaned up if the write is deferred.
func (fbo *folderBranchOps) writeDataLocked(
	ctx context.Context, lState *lockState, md *RootMetadata, file path,
	data []byte, off int64, doNotify bool) ([]BlockPointer, error) {
	if sz := off + int64(len(data)); uint64(sz) > fbo.config.MaxFileBytes() {
		return nil, FileTooBigError{file, sz, fbo.config.MaxFileBytes()}
	}

	// check writer status explicitly
	uid, err := fbo.config.KBPKI().GetCurrentUID(ctx)
	if err != nil {
		return nil, err
	}
	if !md.GetTlfHandle().IsWriter(uid) {
		return nil, NewWriteAccessError(ctx, fbo.config, md.GetTlfHandle(), uid)
	}

	fblock, err := fbo.getFileLocked(ctx, lState, md, file, mdWrite)
	if err != nil {
		return nil, err
	}

	bcache := fbo.config.BlockCache()
	bsplit := fbo.config.BlockSplitter()
	n := int64(len(data))
	nCopied := int64(0)

	_, de, err := fbo.getEntryLocked(ctx, lState, md, file)
	if err != nil {
		return nil, err
	}

	fbo.cacheLock.Lock()
	defer fbo.cacheLock.Unlock()
	si := fbo.getOrCreateSyncInfoLocked(de)
	var newPtrs []BlockPointer
	for nCopied < n {
		ptr, parentBlock, indexInParent, block, more, startOff, err :=
			fbo.getFileBlockAtOffsetLocked(
				ctx, lState, md, file, fblock,
				off+nCopied, mdWrite)
		if err != nil {
			return nil, err
		}

		oldLen := len(block.Contents)
		nCopied += bsplit.CopyUntilSplit(block, !more, data[nCopied:],
			off+nCopied-startOff)

		// the block splitter could only have copied to the end of the
		// existing block (or appended to the end of the final block), so
		// we shouldn't ever hit this case:
		if more && oldLen < len(block.Contents) {
			return nil, BadSplitError{}
		}

		// TODO: support multiple levels of indirection.  Right now the
		// code only does one but it should be straightforward to
		// generalize, just annoying

		// if we need another block but there are no more, then make one
		if nCopied < n && !more {
			// If the block doesn't already have a parent block, make one.
			if ptr == file.tailPointer() {
				// pick a new id for this block, and use this block's ID for
				// the parent
				newID, err := fbo.config.Crypto().MakeTemporaryBlockID()
				if err != nil {
					return nil, err
				}
				fblock = &FileBlock{
					CommonBlock: CommonBlock{
						IsInd: true,
					},
					IPtrs: []IndirectFilePtr{
						{
							BlockInfo: BlockInfo{
								BlockPointer: BlockPointer{
									ID:       newID,
									KeyGen:   md.LatestKeyGeneration(),
									DataVer:  fbo.config.DataVersion(),
									Creator:  uid,
									RefNonce: zeroBlockRefNonce,
								},
								EncodedSize: 0,
							},
							Off: 0,
						},
					},
				}
				if err := bcache.PutDirty(
					file.tailPointer(), file.Branch, fblock); err != nil {
					return nil, err
				}
				ptr = fblock.IPtrs[0].BlockPointer
				newPtrs = append(newPtrs, ptr)
			}

			// Make a new right block and update the parent's
			// indirect block list
			newPtr, err := fbo.newRightBlockLocked(ctx, file.tailPointer(),
				file.Branch, fblock, startOff+int64(len(block.Contents)), md)
			if err != nil {
				return nil, err
			}
			newPtrs = append(newPtrs, newPtr)
		}

		if oldLen != len(block.Contents) || de.Writer != uid {
			de.EncodedSize = 0
			// update the file info
			de.Size += uint64(len(block.Contents) - oldLen)
			parentPtr := stripBP(file.parentPath().tailPointer())
			if _, ok := fbo.deCache[parentPtr]; !ok {
				fbo.deCache[parentPtr] = make(map[BlockPointer]DirEntry)
			}
			fbo.deCache[parentPtr][stripBP(file.tailPointer())] = de
		}

		if parentBlock != nil {
			// remember how many bytes it was
			si.unrefs = append(si.unrefs,
				parentBlock.IPtrs[indexInParent].BlockInfo)
			parentBlock.IPtrs[indexInParent].EncodedSize = 0
		}
		// keep the old block ID while it's dirty
		if err = fbo.cacheBlockIfNotYetDirtyLocked(ptr, file.Branch,
			block); err != nil {
			return nil, err
		}
	}

	if fblock.IsInd {
		// Always make the top block dirty, so we will sync its
		// indirect blocks.  This has the added benefit of ensuring
		// that any write to a file while it's being sync'd will be
		// deferred, even if it's to a block that's not currently
		// being sync'd, since this top-most block will always be in
		// the fileBlockStates map.
		if err = fbo.cacheBlockIfNotYetDirtyLocked(
			file.tailPointer(), file.Branch, fblock); err != nil {
			return nil, err
		}
		newPtrs = append(newPtrs, file.tailPointer())
	}
	si.op.addWrite(uint64(off), uint64(len(data)))

	if doNotify {
		fbo.notifyLocal(ctx, file, si.op)
	}
	fbo.transitionState(dirtyState)
	return newPtrs, nil
}

// cacheLock must be taken by caller.
func (fbo *folderBranchOps) clearDeCacheEntryLocked(parentPtr BlockPointer,
	filePtr BlockPointer) {
	// Clear the old deCache entry
	if deMap, ok := fbo.deCache[parentPtr]; ok {
		if _, ok := deMap[filePtr]; ok {
			delete(deMap, filePtr)
			if len(deMap) == 0 {
				delete(fbo.deCache, parentPtr)
			} else {
				fbo.deCache[parentPtr] = deMap
			}
		}
	}
}

func (fbo *folderBranchOps) Write(
	ctx context.Context, file Node, data []byte, off int64) (err error) {
	fbo.log.CDebugf(ctx, "Write %p %d %d", file.GetID(), len(data), off)
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	err = fbo.checkNode(file)
	if err != nil {
		return err
	}

	lState := makeFBOLockState()

	// Get the MD for reading.  We won't modify it; we'll track the
	// unref changes on the side, and put them into the MD during the
	// sync.
	md, err := fbo.getMDLocked(ctx, lState, mdReadNeedIdentify)
	if err != nil {
		return err
	}

	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)
	filePath, err := fbo.pathFromNodeForWriteLocked(file)
	if err != nil {
		return err
	}

	defer func() {
		fbo.doDeferWrite = false
	}()

	newPtrs, err :=
		fbo.writeDataLocked(ctx, lState, md, filePath, data, off, true)
	if err != nil {
		return err
	}

	if fbo.doDeferWrite {
		// There's an ongoing sync, and this write altered dirty
		// blocks that are in the process of syncing.  So, we have to
		// redo this write once the sync is complete, using the new
		// file path.
		//
		// There is probably a less terrible of doing this that
		// doesn't involve so much copying and rewriting, but this is
		// the most obviously correct way.
		dataCopy := make([]byte, len(data))
		copy(dataCopy, data)
		fbo.log.CDebugf(ctx, "Deferring a write to file %v off=%d len=%d",
			filePath.tailPointer(), off, len(data))
		fbo.deferredDirtyDeletes = append(fbo.deferredDirtyDeletes,
			newPtrs...)
		fbo.deferredWrites = append(fbo.deferredWrites,
			func(ctx context.Context, rmd *RootMetadata, f path) error {
				// Write the data again.  We know this won't be
				// deferred, so no need to check the new ptrs.
				_, err := fbo.writeDataLocked(
					ctx, lState, rmd, f, dataCopy, off, false)
				return err
			})
	}

	fbo.status.addDirtyNode(file)
	return nil
}

// blockLock must be held for writing by the caller.  Returns the set
// of newly-ID'd blocks created during this truncate that might need
// to be cleaned up if the truncate is deferred.
func (fbo *folderBranchOps) truncateLocked(
	ctx context.Context, lState *lockState, md *RootMetadata,
	file path, size uint64, doNotify bool) ([]BlockPointer, error) {
	// check writer status explicitly
	uid, err := fbo.config.KBPKI().GetCurrentUID(ctx)
	if err != nil {
		return nil, err
	}
	if !md.GetTlfHandle().IsWriter(uid) {
		return nil, NewWriteAccessError(ctx, fbo.config, md.GetTlfHandle(), uid)
	}

	fblock, err := fbo.getFileLocked(ctx, lState, md, file, mdWrite)
	if err != nil {
		return nil, err
	}

	// find the block where the file should now end
	iSize := int64(size) // TODO: deal with overflow
	ptr, parentBlock, indexInParent, block, more, startOff, err :=
		fbo.getFileBlockAtOffsetLocked(ctx, lState, md, file, fblock, iSize, mdWrite)

	currLen := int64(startOff) + int64(len(block.Contents))
	if currLen < iSize {
		// if we need to extend the file, let's just do a write
		moreNeeded := iSize - currLen
		return fbo.writeDataLocked(ctx, lState, md, file,
			make([]byte, moreNeeded, moreNeeded), currLen, doNotify)
	} else if currLen == iSize {
		// same size!
		return nil, nil
	}

	// update the local entry size
	_, de, err := fbo.getEntryLocked(ctx, lState, md, file)
	if err != nil {
		return nil, err
	}

	// otherwise, we need to delete some data (and possibly entire blocks)
	block.Contents = append([]byte(nil), block.Contents[:iSize-startOff]...)
	fbo.cacheLock.Lock()
	doCacheUnlock := true
	defer func() {
		if doCacheUnlock {
			fbo.cacheLock.Unlock()
		}
	}()

	si := fbo.getOrCreateSyncInfoLocked(de)
	if more {
		// TODO: if indexInParent == 0, we can remove the level of indirection
		for _, ptr := range parentBlock.IPtrs[indexInParent+1:] {
			si.unrefs = append(si.unrefs, ptr.BlockInfo)
		}
		parentBlock.IPtrs = parentBlock.IPtrs[:indexInParent+1]
		// always make the parent block dirty, so we will sync it
		if err = fbo.cacheBlockIfNotYetDirtyLocked(
			file.tailPointer(), file.Branch, parentBlock); err != nil {
			return nil, err
		}
	}

	if fblock.IsInd {
		// Always make the top block dirty, so we will sync its
		// indirect blocks.  This has the added benefit of ensuring
		// that any truncate to a file while it's being sync'd will be
		// deferred, even if it's to a block that's not currently
		// being sync'd, since this top-most block will always be in
		// the fileBlockStates map.
		if err = fbo.cacheBlockIfNotYetDirtyLocked(
			file.tailPointer(), file.Branch, fblock); err != nil {
			return nil, err
		}
	}

	if parentBlock != nil {
		// TODO: When we implement more than one level of indirection,
		// make sure that the pointer to parentBlock in the grandparent block
		// has EncodedSize 0.
		si.unrefs = append(si.unrefs,
			parentBlock.IPtrs[indexInParent].BlockInfo)
		parentBlock.IPtrs[indexInParent].EncodedSize = 0
	}

	doCacheUnlock = false
	si.op.addTruncate(size)
	fbo.cacheLock.Unlock()

	de.EncodedSize = 0
	de.Size = size
	parentPtr := stripBP(file.parentPath().tailPointer())
	if _, ok := fbo.deCache[parentPtr]; !ok {
		fbo.deCache[parentPtr] = make(map[BlockPointer]DirEntry)
	}
	fbo.deCache[parentPtr][stripBP(file.tailPointer())] = de

	// Keep the old block ID while it's dirty.
	if err = fbo.cacheBlockIfNotYetDirtyLocked(
		ptr, file.Branch, block); err != nil {
		return nil, err
	}

	if doNotify {
		fbo.notifyLocal(ctx, file, si.op)
	}
	fbo.transitionState(dirtyState)
	return nil, nil
}

func (fbo *folderBranchOps) Truncate(
	ctx context.Context, file Node, size uint64) (err error) {
	fbo.log.CDebugf(ctx, "Truncate %p %d", file.GetID(), size)
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	err = fbo.checkNode(file)
	if err != nil {
		return err
	}

	lState := makeFBOLockState()

	// Get the MD for reading.  We won't modify it; we'll track the
	// unref changes on the side, and put them into the MD during the
	// sync.
	md, err := fbo.getMDLocked(ctx, lState, mdReadNeedIdentify)
	if err != nil {
		return err
	}

	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)
	filePath, err := fbo.pathFromNodeForWriteLocked(file)
	if err != nil {
		return err
	}

	defer func() {
		fbo.doDeferWrite = false
	}()

	newPtrs, err := fbo.truncateLocked(ctx, lState, md, filePath, size, true)
	if err != nil {
		return err
	}

	if fbo.doDeferWrite {
		// There's an ongoing sync, and this truncate altered
		// dirty blocks that are in the process of syncing.  So,
		// we have to redo this truncate once the sync is complete,
		// using the new file path.
		fbo.log.CDebugf(ctx, "Deferring a truncate to file %v",
			filePath.tailPointer())
		fbo.deferredDirtyDeletes = append(fbo.deferredDirtyDeletes,
			newPtrs...)
		fbo.deferredWrites = append(fbo.deferredWrites,
			func(ctx context.Context, rmd *RootMetadata, f path) error {
				// Truncate the file again.  We know this won't be
				// deferred, so no need to check the new ptrs.
				_, err := fbo.truncateLocked(ctx, lState, rmd, f, size, false)
				return err
			})
	}

	fbo.status.addDirtyNode(file)
	return nil
}

// mdWriterLock must be taken by caller.
func (fbo *folderBranchOps) setExLocked(
	ctx context.Context, lState *lockState, file path,
	ex bool) (err error) {
	// verify we have permission to write
	md, err := fbo.getMDForWriteLocked(ctx, lState)
	if err != nil {
		return
	}

	dblock, de, err := fbo.getEntry(ctx, lState, md, file)
	if err != nil {
		return
	}

	// If the file is a symlink, do nothing (to match ext4
	// behavior).
	if de.Type == Sym {
		return
	}

	if ex && (de.Type == File) {
		de.Type = Exec
	} else if !ex && (de.Type == Exec) {
		de.Type = File
	}

	parentPath := file.parentPath()
	md.AddOp(newSetAttrOp(file.tailName(), parentPath.tailPointer(), exAttr,
		file.tailPointer()))

	// If the type isn't File or Exec, there's nothing to do, but
	// change the ctime anyway (to match ext4 behavior).
	de.Ctime = fbo.nowUnixNano()
	dblock.Children[file.tailName()] = de
	_, err = fbo.syncBlockAndFinalizeLocked(
		ctx, lState, md, dblock, *parentPath.parentPath(), parentPath.tailName(),
		Dir, false, false, zeroPtr)
	return err
}

func (fbo *folderBranchOps) SetEx(
	ctx context.Context, file Node, ex bool) (err error) {
	fbo.log.CDebugf(ctx, "SetEx %p %t", file.GetID(), ex)
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	err = fbo.checkNode(file)
	if err != nil {
		return
	}

	lState := makeFBOLockState()

	fbo.mdWriterLock.Lock(lState)
	defer fbo.mdWriterLock.Unlock(lState)
	filePath, err := fbo.pathFromNodeForMDWriteLocked(file)
	if err != nil {
		return
	}

	return fbo.setExLocked(ctx, lState, filePath, ex)
}

// mdWriterLock must be taken by caller.
func (fbo *folderBranchOps) setMtimeLocked(
	ctx context.Context, lState *lockState, file path,
	mtime *time.Time) error {
	// verify we have permission to write
	md, err := fbo.getMDForWriteLocked(ctx, lState)
	if err != nil {
		return err
	}

	dblock, de, err := fbo.getEntry(ctx, lState, md, file)
	if err != nil {
		return err
	}

	parentPath := file.parentPath()
	md.AddOp(newSetAttrOp(file.tailName(), parentPath.tailPointer(), mtimeAttr,
		file.tailPointer()))

	de.Mtime = mtime.UnixNano()
	// setting the mtime counts as changing the file MD, so must set ctime too
	de.Ctime = fbo.nowUnixNano()
	dblock.Children[file.tailName()] = de
	_, err = fbo.syncBlockAndFinalizeLocked(
		ctx, lState, md, dblock, *parentPath.parentPath(), parentPath.tailName(),
		Dir, false, false, zeroPtr)
	return err
}

func (fbo *folderBranchOps) SetMtime(
	ctx context.Context, file Node, mtime *time.Time) (err error) {
	fbo.log.CDebugf(ctx, "SetMtime %p %v", file.GetID(), mtime)
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	if mtime == nil {
		// Can happen on some OSes (e.g. OSX) when trying to set the atime only
		return nil
	}

	err = fbo.checkNode(file)
	if err != nil {
		return
	}

	lState := makeFBOLockState()

	fbo.mdWriterLock.Lock(lState)
	defer fbo.mdWriterLock.Unlock(lState)
	filePath, err := fbo.pathFromNodeForMDWriteLocked(file)
	if err != nil {
		return err
	}

	return fbo.setMtimeLocked(ctx, lState, filePath, mtime)
}

// cacheLock should be taken by the caller
func (fbo *folderBranchOps) mergeUnrefCacheLocked(file path, md *RootMetadata) {
	filePtr := stripBP(file.tailPointer())
	for _, info := range fbo.unrefCache[filePtr].unrefs {
		// it's ok if we push the same ptr.ID/RefNonce multiple times,
		// because the subsequent ones should have a QuotaSize of 0.
		md.AddUnrefBlock(info)
	}
}

// mdWriterLock must be taken by the caller.
func (fbo *folderBranchOps) syncLocked(ctx context.Context,
	lState *lockState, file path) (stillDirty bool, err error) {
	// if the cache for this file isn't dirty, we're done
	fbo.blockLock.RLock(lState)
	bcache := fbo.config.BlockCache()
	if !bcache.IsDirty(file.tailPointer(), file.Branch) {
		fbo.blockLock.RUnlock(lState)
		return false, nil
	}
	fbo.blockLock.RUnlock(lState)

	// Verify we have permission to write.  We do this after the dirty
	// check because otherwise readers who sync clean files on close
	// would get an error.
	md, err := fbo.getMDForWriteLocked(ctx, lState)
	if err != nil {
		return true, err
	}

	// If the MD doesn't match the MD expected by the path, that
	// implies we are using a cached path, which implies the node has
	// been unlinked.  In that case, we can safely ignore this sync.
	if md.data.Dir.BlockPointer != file.path[0].BlockPointer {
		fbo.log.CDebugf(ctx, "Skipping sync for a removed file %v",
			file.tailPointer())
		// Unfortunately the parent pointer in the path is probably
		// wrong now, so we have to iterate through the decache to
		// find the entry to remove.
		fbo.cacheLock.Lock()
		defer fbo.cacheLock.Unlock()
		for parentPtr, deMap := range fbo.deCache {
			for filePtr := range deMap {
				if filePtr == file.tailPointer() {
					fbo.clearDeCacheEntryLocked(parentPtr, filePtr)
				}
			}
		}
		fbo.transitionState(cleanState)
		return true, nil
	}

	uid, err := fbo.config.KBPKI().GetCurrentUID(ctx)
	if err != nil {
		return true, err
	}

	// TODO: deferredDirtyDeletes can just be an array of
	// ptrs (with fbo.branch() implicit).
	//
	// TODO: Clean up deferredDirtyDeletes on return, like
	// syncIndirectFileBlockPtrs.
	var deferredDirtyDeletes []func() error

	// A list of permanent entries added to the block cache, which
	// should be removed after the blocks have been sent to the
	// server.  They are not removed on an error, because in that case
	// the file is still dirty locally and may get another chance to
	// be sync'd.
	var syncIndirectFileBlockPtrs []BlockPointer

	// notify the daemon that a write is being performed
	fbo.config.Reporter().Notify(ctx, writeNotification(file, false))
	defer fbo.config.Reporter().Notify(ctx, writeNotification(file, true))

	doUnlock := true
	fbo.blockLock.Lock(lState)
	defer func() {
		if doUnlock {
			fbo.blockLock.Unlock(lState)
		}
	}()

	// update the parent directories, and write all the new blocks out
	// to disk
	fblock, err := fbo.getFileLocked(ctx, lState, md, file, mdWrite)
	if err != nil {
		return true, err
	}

	filePtr := stripBP(file.tailPointer())
	si, ok := func() (*syncInfo, bool) {
		fbo.cacheLock.Lock()
		defer fbo.cacheLock.Unlock()
		si, ok := fbo.unrefCache[filePtr]
		return si, ok
	}()
	if !ok {
		return true, fmt.Errorf("No syncOp found for file pointer %v", filePtr)
	}
	md.AddOp(si.op)
	defer func() {
		if err != nil {
			// If there was an error, we need to back out any changes
			// that might have been filled into the sync op, because
			// it could get reused again in a later Sync call.
			si.op.resetUpdateState()
		}
	}()
	if si.bps == nil {
		si.bps = newBlockPutState(1)
	} else {
		// reinstate byte accounting from the previous Sync
		md.RefBytes = si.refBytes
		md.DiskUsage += si.refBytes
		md.UnrefBytes = si.unrefBytes
		md.DiskUsage -= si.unrefBytes
		syncIndirectFileBlockPtrs = append(syncIndirectFileBlockPtrs,
			si.op.Refs()...)
	}
	doSaveBytes := true
	defer func() {
		if doSaveBytes {
			si.refBytes = md.RefBytes
			si.unrefBytes = md.UnrefBytes
		}
	}()

	// Note: below we add possibly updated file blocks as "unref" and
	// "ref" blocks.  This is fine, since conflict resolution or
	// notifications will never happen within a file.

	// if this is an indirect block:
	//   1) check if each dirty block is split at the right place.
	//   2) if it needs fewer bytes, prepend the extra bytes to the next
	//      block (making a new one if it doesn't exist), and the next block
	//      gets marked dirty
	//   3) if it needs more bytes, then use copyUntilSplit() to fetch bytes
	//      from the next block (if there is one), remove the copied bytes
	//      from the next block and mark it dirty
	//   4) Then go through once more, and ready and finalize each
	//      dirty block, updating its ID in the indirect pointer list
	bsplit := fbo.config.BlockSplitter()
	if fblock.IsInd {
		// TODO: Verify that any getFileBlock... calls here
		// only use the dirty cache and not the network, since
		// the blocks are be dirty.
		for i := 0; i < len(fblock.IPtrs); i++ {
			ptr := fblock.IPtrs[i]
			isDirty := bcache.IsDirty(ptr.BlockPointer, file.Branch)
			if (ptr.EncodedSize > 0) && isDirty {
				return true, InconsistentEncodedSizeError{ptr.BlockInfo}
			}
			if isDirty {
				_, _, _, block, more, _, err :=
					fbo.getFileBlockAtOffsetLocked(ctx, lState, md, file, fblock,
						ptr.Off, mdWrite)
				if err != nil {
					return true, err
				}

				splitAt := bsplit.CheckSplit(block)
				switch {
				case splitAt == 0:
					continue
				case splitAt > 0:
					endOfBlock := ptr.Off + int64(len(block.Contents))
					extraBytes := block.Contents[splitAt:]
					block.Contents = block.Contents[:splitAt]
					// put the extra bytes in front of the next block
					if !more {
						// need to make a new block
						if _, err := fbo.newRightBlockLocked(
							ctx, file.tailPointer(), file.Branch, fblock,
							endOfBlock, md); err != nil {
							return true, err
						}
					}
					rPtr, _, _, rblock, _, _, err :=
						fbo.getFileBlockAtOffsetLocked(ctx, lState, md, file, fblock,
							endOfBlock, mdWrite)
					if err != nil {
						return true, err
					}
					rblock.Contents = append(extraBytes, rblock.Contents...)
					if err = fbo.cacheBlockIfNotYetDirtyLocked(
						rPtr, file.Branch, rblock); err != nil {
						return true, err
					}
					fblock.IPtrs[i+1].Off = ptr.Off + int64(len(block.Contents))
					md.AddUnrefBlock(fblock.IPtrs[i+1].BlockInfo)
					fblock.IPtrs[i+1].EncodedSize = 0
				case splitAt < 0:
					if !more {
						// end of the line
						continue
					}

					endOfBlock := ptr.Off + int64(len(block.Contents))
					rPtr, _, _, rblock, _, _, err :=
						fbo.getFileBlockAtOffsetLocked(ctx, lState, md, file, fblock,
							endOfBlock, mdWrite)
					if err != nil {
						return true, err
					}
					// copy some of that block's data into this block
					nCopied := bsplit.CopyUntilSplit(block, false,
						rblock.Contents, int64(len(block.Contents)))
					rblock.Contents = rblock.Contents[nCopied:]
					if len(rblock.Contents) > 0 {
						if err = fbo.cacheBlockIfNotYetDirtyLocked(
							rPtr, file.Branch, rblock); err != nil {
							return true, err
						}
						fblock.IPtrs[i+1].Off =
							ptr.Off + int64(len(block.Contents))
						md.AddUnrefBlock(fblock.IPtrs[i+1].BlockInfo)
						fblock.IPtrs[i+1].EncodedSize = 0
					} else {
						// TODO: delete the block, and if we're down
						// to just one indirect block, remove the
						// layer of indirection
						//
						// TODO: When we implement more than one level
						// of indirection, make sure that the pointer
						// to the parent block in the grandparent
						// block has EncodedSize 0.
						md.AddUnrefBlock(fblock.IPtrs[i+1].BlockInfo)
						fblock.IPtrs =
							append(fblock.IPtrs[:i+1], fblock.IPtrs[i+2:]...)
					}
				}
			}
		}

		for i, ptr := range fblock.IPtrs {
			isDirty := bcache.IsDirty(ptr.BlockPointer, file.Branch)
			if (ptr.EncodedSize > 0) && isDirty {
				return true, InconsistentEncodedSizeError{ptr.BlockInfo}
			}
			if isDirty {
				_, _, _, block, _, _, err := fbo.getFileBlockAtOffsetLocked(
					ctx, lState, md, file, fblock, ptr.Off, mdWrite)
				if err != nil {
					return true, err
				}

				newInfo, _, readyBlockData, err :=
					fbo.readyBlock(ctx, md, block, uid)
				if err != nil {
					return true, err
				}

				syncIndirectFileBlockPtrs = append(syncIndirectFileBlockPtrs, newInfo.BlockPointer)
				err = bcache.Put(newInfo.BlockPointer, fbo.id(), block, PermanentEntry)
				if err != nil {
					return true, err
				}

				// Defer the DeleteDirty until after the new path is
				// ready, in case anyone tries to read the dirty file
				// in the meantime.
				localPtr := ptr.BlockPointer
				deferredDirtyDeletes =
					append(deferredDirtyDeletes, func() error {
						return bcache.DeleteDirty(localPtr, file.Branch)
					})

				fblock.IPtrs[i].BlockInfo = newInfo
				md.AddRefBlock(newInfo)
				si.bps.addNewBlock(newInfo.BlockPointer, block, readyBlockData)
				fbo.fileBlockStates[localPtr] = blockSyncingNotDirty
			}
		}
	}

	fbo.fileBlockStates[file.tailPointer()] = blockSyncingNotDirty
	doUnlock = false
	fbo.blockLock.Unlock(lState)

	parentPath := file.parentPath()
	parentPtr := stripBP(parentPath.tailPointer())
	lbc := make(localBcache)
	doDeleteDe := false
	err = func() error {
		fbo.blockLock.RLock(lState)
		defer fbo.blockLock.RUnlock(lState)

		dblock, err := fbo.getDirLocked(ctx, lState, md, *parentPath, mdWrite)
		if err != nil {
			return err
		}

		// add in the cached unref pieces and fixup the dir entry
		fbo.cacheLock.Lock()
		defer fbo.cacheLock.Unlock()

		fbo.mergeUnrefCacheLocked(file, md)

		// update the file's directory entry to the cached copy
		if deMap, ok := fbo.deCache[parentPtr]; ok {
			if de, ok := deMap[filePtr]; ok {
				// remember the old info
				de.EncodedSize = si.oldInfo.EncodedSize
				dblock.Children[file.tailName()] = de
				lbc[parentPath.tailPointer()] = dblock
				doDeleteDe = true
				fbo.clearDeCacheEntryLocked(parentPtr, filePtr)
			}
		}

		return nil
	}()
	if err != nil {
		return true, err
	}

	// All bytes past this point don't need to be saved, since they
	// are specific to this sync.
	doSaveBytes = false
	si.refBytes = md.RefBytes
	si.unrefBytes = md.UnrefBytes

	newPath, _, newBps, err :=
		fbo.syncBlockAndCheckEmbed(ctx, lState, md, fblock, *parentPath,
			file.tailName(), File, true, true, zeroPtr, lbc)
	if err != nil {
		return true, err
	}
	newBps.mergeOtherBps(si.bps)

	err = fbo.doBlockPuts(ctx, md, *newBps)
	if err != nil {
		return true, err
	}

	deferredDirtyDeletes = append(deferredDirtyDeletes, func() error {
		return bcache.DeleteDirty(file.tailPointer(), file.Branch)
	})

	err = fbo.finalizeMDWriteLocked(ctx, lState, md, newBps)
	if err != nil {
		return true, err
	}

	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)

	for _, f := range deferredDirtyDeletes {
		// This will also clear any dirty blocks that resulted from a
		// write/truncate happening during the sync.  But that's ok,
		// because we will redo them below.
		err := f()
		if err != nil {
			return true, err
		}
	}

	for _, ptr := range syncIndirectFileBlockPtrs {
		err := bcache.DeletePermanent(ptr.ID)
		if err != nil {
			fbo.log.CWarningf(ctx, "Error when deleting %v from cache: %v", ptr.ID, err)
		}
	}
	syncIndirectFileBlockPtrs = nil

	err = func() error {
		fbo.cacheLock.Lock()
		defer fbo.cacheLock.Unlock()

		// Clear the updated de from the cache.  We are guaranteed that
		// any concurrent write to this file was deferred, even if it was
		// to a block that wasn't currently being sync'd, since the
		// top-most block is always in fileBlockStates and is always
		// dirtied during a write/truncate.
		if doDeleteDe {
			fbo.clearDeCacheEntryLocked(parentPtr, filePtr)
		}

		// we can get rid of all the sync state that might have
		// happened during the sync, since we will replay the writes
		// below anyway.
		delete(fbo.unrefCache, filePtr)
		return nil
	}()
	if err != nil {
		return true, err
	}

	fbo.fileBlockStates = make(map[BlockPointer]syncBlockState)
	// Redo any writes or truncates that happened to our file while
	// the sync was happening.
	deletes := fbo.deferredDirtyDeletes
	writes := fbo.deferredWrites
	stillDirty = len(fbo.deferredWrites) != 0
	fbo.deferredDirtyDeletes = nil
	fbo.deferredWrites = nil

	for _, ptr := range deletes {
		if err := bcache.DeleteDirty(ptr, fbo.branch()); err != nil {
			return true, err
		}
	}
	for _, f := range writes {
		// Clear the old deCache entry
		func() {
			fbo.cacheLock.Lock()
			defer fbo.cacheLock.Unlock()
			fbo.clearDeCacheEntryLocked(
				newPath.parentPath().tailPointer(), file.tailPointer())
		}()
		// we can safely read head here because we hold mdWriterLock
		err = f(ctx, fbo.head, newPath)
		if err != nil {
			// It's a little weird to return an error from a deferred
			// write here. Hopefully that will never happen.
			return true, err
		}
	}

	return stillDirty, nil
}

func (fbo *folderBranchOps) Sync(ctx context.Context, file Node) (err error) {
	fbo.log.CDebugf(ctx, "Sync %p", file.GetID())
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	err = fbo.checkNode(file)
	if err != nil {
		return
	}

	lState := makeFBOLockState()

	fbo.mdWriterLock.Lock(lState)
	defer fbo.mdWriterLock.Unlock(lState)
	filePath, err := fbo.pathFromNodeForMDWriteLocked(file)
	if err != nil {
		return err
	}

	stillDirty, err := fbo.syncLocked(ctx, lState, filePath)
	if err != nil {
		return err
	}

	if !stillDirty {
		fbo.status.rmDirtyNode(file)
	}
	return nil
}

func (fbo *folderBranchOps) Status(
	ctx context.Context, folderBranch FolderBranch) (
	fbs FolderBranchStatus, updateChan <-chan StatusUpdate, err error) {
	fbo.log.CDebugf(ctx, "Status")
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	if folderBranch != fbo.folderBranch {
		return FolderBranchStatus{}, nil,
			WrongOpsError{fbo.folderBranch, folderBranch}
	}

	// Wait for conflict resolution to settle down, if necessary.
	fbo.cr.Wait(ctx)

	return fbo.status.getStatus(ctx)
}

// RegisterForChanges registers a single Observer to receive
// notifications about this folder/branch.
func (fbo *folderBranchOps) RegisterForChanges(obs Observer) error {
	fbo.obsLock.Lock()
	defer fbo.obsLock.Unlock()
	// It's the caller's responsibility to make sure
	// RegisterForChanges isn't called twice for the same Observer
	fbo.observers = append(fbo.observers, obs)
	return nil
}

// UnregisterFromChanges stops an Observer from getting notifications
// about the folder/branch.
func (fbo *folderBranchOps) UnregisterFromChanges(obs Observer) error {
	fbo.obsLock.Lock()
	defer fbo.obsLock.Unlock()
	for i, oldObs := range fbo.observers {
		if oldObs == obs {
			fbo.observers = append(fbo.observers[:i], fbo.observers[i+1:]...)
			break
		}
	}
	return nil
}

func (fbo *folderBranchOps) notifyLocal(ctx context.Context,
	file path, so *syncOp) {
	node := fbo.nodeCache.Get(file.tailPointer())
	if node == nil {
		return
	}
	// notify about the most recent write op
	latestWrite := so.Writes[len(so.Writes)-1]

	fbo.obsLock.RLock()
	defer fbo.obsLock.RUnlock()
	for _, obs := range fbo.observers {
		obs.LocalChange(ctx, node, latestWrite)
	}
}

// notifyBatchLocked sends out a notification for the most recent op
// in md. headLock must be held by the caller.
func (fbo *folderBranchOps) notifyBatchLocked(
	ctx context.Context, lState *lockState, md *RootMetadata) {
	lastOp := md.data.Changes.Ops[len(md.data.Changes.Ops)-1]
	fbo.notifyOneOpLocked(ctx, lState, lastOp, md)
}

// searchForNodesInDirLocked recursively tries to find a path, and
// ultimately a node, to ptr, given the set of pointers that were
// updated in a particular operation.  The keys in nodeMap make up the
// set of BlockPointers that are being searched for, and nodeMap is
// updated in place to include the corresponding discovered nodes.
//
// Returns the number of nodes found by this invocation.
//
// blockLock must be taken for reading
func (fbo *folderBranchOps) searchForNodesInDirLocked(ctx context.Context,
	lState *lockState, cache NodeCache, newPtrs map[BlockPointer]bool,
	md *RootMetadata, currDir path, nodeMap map[BlockPointer]Node,
	numNodesFoundSoFar int) (int, error) {
	dirBlock, err := fbo.getDirLocked(ctx, lState, md, currDir, mdReadNeedIdentify)
	if err != nil {
		return 0, err
	}

	if numNodesFoundSoFar >= len(nodeMap) {
		return 0, nil
	}

	numNodesFound := 0
	for name, de := range dirBlock.Children {
		if _, ok := nodeMap[de.BlockPointer]; ok {
			childPath := currDir.ChildPath(name, de.BlockPointer)
			// make a node for every pathnode
			var n Node
			for _, pn := range childPath.path {
				n, err = cache.GetOrCreate(pn.BlockPointer, pn.Name, n)
				if err != nil {
					return 0, err
				}
			}
			nodeMap[de.BlockPointer] = n
			numNodesFound++
			if numNodesFoundSoFar+numNodesFound >= len(nodeMap) {
				return numNodesFound, nil
			}
		}

		// otherwise, recurse if this represents an updated block
		if _, ok := newPtrs[de.BlockPointer]; de.Type == Dir && ok {
			childPath := currDir.ChildPath(name, de.BlockPointer)
			n, err := fbo.searchForNodesInDirLocked(ctx, lState, cache, newPtrs, md,
				childPath, nodeMap, numNodesFoundSoFar+numNodesFound)
			if err != nil {
				return 0, err
			}
			numNodesFound += n
			if numNodesFoundSoFar+numNodesFound >= len(nodeMap) {
				return numNodesFound, nil
			}
		}
	}

	return numNodesFound, nil
}

// searchForNodes tries to resolve all the given pointers to a Node
// object, using only the updated pointers specified in newPtrs.  Does
// an error if any subset of the pointer paths do not exist; it is the
// caller's responsibility to decide to error on particular unresolved
// nodes.
func (fbo *folderBranchOps) searchForNodes(ctx context.Context,
	cache NodeCache, ptrs []BlockPointer, newPtrs map[BlockPointer]bool,
	md *RootMetadata) (map[BlockPointer]Node, error) {
	lState := makeFBOLockState()
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)

	nodeMap := make(map[BlockPointer]Node)
	for _, ptr := range ptrs {
		nodeMap[ptr] = nil
	}

	if len(ptrs) == 0 {
		return nodeMap, nil
	}

	// Start with the root node
	rootPtr := md.data.Dir.BlockPointer
	node := cache.Get(rootPtr)
	if node == nil {
		return nil, fmt.Errorf("Cannot find root node corresponding to %v",
			rootPtr)
	}

	// are they looking for the root directory?
	numNodesFound := 0
	if _, ok := nodeMap[rootPtr]; ok {
		nodeMap[rootPtr] = node
		numNodesFound++
		if numNodesFound >= len(nodeMap) {
			return nodeMap, nil
		}
	}

	rootPath := cache.PathFromNode(node)
	if len(rootPath.path) != 1 {
		return nil, fmt.Errorf("Invalid root path for %v: %s",
			md.data.Dir.BlockPointer, rootPath)
	}

	_, err := fbo.searchForNodesInDirLocked(ctx, lState, cache, newPtrs, md, rootPath,
		nodeMap, numNodesFound)
	if err != nil {
		return nil, err
	}

	// Return the whole map even if some nodes weren't found.
	return nodeMap, nil
}

// searchForNode tries to figure out the path to the given
// blockPointer, using only the block updates that happened as part of
// a given MD update operation.
func (fbo *folderBranchOps) searchForNode(ctx context.Context,
	ptr BlockPointer, op op, md *RootMetadata) (Node, error) {
	// Record which pointers are new to this update, and thus worth
	// searching.
	newPtrs := make(map[BlockPointer]bool)
	for _, update := range op.AllUpdates() {
		newPtrs[update.Ref] = true
	}

	nodeMap, err := fbo.searchForNodes(ctx, fbo.nodeCache, []BlockPointer{ptr},
		newPtrs, md)
	if err != nil {
		return nil, err
	}

	n, ok := nodeMap[ptr]
	if !ok {
		return nil, NodeNotFoundError{ptr}
	}

	return n, nil
}

func (fbo *folderBranchOps) unlinkFromCache(op op, oldDir BlockPointer,
	node Node, name string) error {
	// The entry could be under any one of the unref'd blocks, and
	// it's safe to perform this when the pointer isn't real, so just
	// try them all to avoid the overhead of looking up the right
	// pointer in the old version of the block.
	p, err := fbo.pathFromNodeForRead(node)
	if err != nil {
		return err
	}

	childPath := p.ChildPathNoPtr(name)

	// revert the parent pointer
	childPath.path[len(childPath.path)-2].BlockPointer = oldDir
	for _, ptr := range op.Unrefs() {
		childPath.path[len(childPath.path)-1].BlockPointer = ptr
		fbo.nodeCache.Unlink(ptr, childPath)
	}

	return nil
}

// cacheLock must be taken by the caller.
func (fbo *folderBranchOps) moveDeCacheEntryLocked(oldParent BlockPointer,
	newParent BlockPointer, moved BlockPointer) {
	if newParent == zeroPtr {
		// A rename within the same directory, so no need to move anything.
		return
	}

	oldPtr := stripBP(oldParent)
	if deMap, ok := fbo.deCache[oldPtr]; ok {
		dePtr := stripBP(moved)
		if de, ok := deMap[dePtr]; ok {
			newPtr := stripBP(newParent)
			if _, ok = fbo.deCache[newPtr]; !ok {
				fbo.deCache[newPtr] = make(map[BlockPointer]DirEntry)
			}
			fbo.deCache[newPtr][dePtr] = de
			delete(deMap, dePtr)
			if len(deMap) == 0 {
				delete(fbo.deCache, oldPtr)
			} else {
				fbo.deCache[oldPtr] = deMap
			}
		}
	}
}

func (fbo *folderBranchOps) updatePointers(op op) {
	fbo.cacheLock.Lock()
	defer fbo.cacheLock.Unlock()
	for _, update := range op.AllUpdates() {
		fbo.nodeCache.UpdatePointer(update.Unref, update.Ref)
		// move the deCache for this directory
		oldPtrStripped := stripBP(update.Unref)
		if deMap, ok := fbo.deCache[oldPtrStripped]; ok {
			fbo.deCache[stripBP(update.Ref)] = deMap
			delete(fbo.deCache, oldPtrStripped)
		}
	}

	// For renames, we need to update any outstanding writes as well.
	rop, ok := op.(*renameOp)
	if !ok {
		return
	}
	fbo.moveDeCacheEntryLocked(rop.OldDir.Ref, rop.NewDir.Ref, rop.Renamed)
}

// headLock must be held by the caller.
func (fbo *folderBranchOps) notifyOneOpLocked(ctx context.Context,
	lState *lockState, op op, md *RootMetadata) {
	fbo.updatePointers(op)

	var changes []NodeChange
	switch realOp := op.(type) {
	default:
		return
	case *createOp:
		node := fbo.nodeCache.Get(realOp.Dir.Ref)
		if node == nil {
			return
		}
		fbo.log.CDebugf(ctx, "notifyOneOp: create %s in node %p",
			realOp.NewName, node.GetID())
		changes = append(changes, NodeChange{
			Node:       node,
			DirUpdated: []string{realOp.NewName},
		})
	case *rmOp:
		node := fbo.nodeCache.Get(realOp.Dir.Ref)
		if node == nil {
			return
		}
		fbo.log.CDebugf(ctx, "notifyOneOp: remove %s in node %p",
			realOp.OldName, node.GetID())
		changes = append(changes, NodeChange{
			Node:       node,
			DirUpdated: []string{realOp.OldName},
		})

		// If this node exists, then the child node might exist too,
		// and we need to unlink it in the node cache.
		err := fbo.unlinkFromCache(op, realOp.Dir.Unref, node, realOp.OldName)
		if err != nil {
			fbo.log.CErrorf(ctx, "Couldn't unlink from cache: %v", err)
			return
		}
	case *renameOp:
		oldNode := fbo.nodeCache.Get(realOp.OldDir.Ref)
		if oldNode != nil {
			changes = append(changes, NodeChange{
				Node:       oldNode,
				DirUpdated: []string{realOp.OldName},
			})
		}
		var newNode Node
		if realOp.NewDir.Ref != zeroPtr {
			newNode = fbo.nodeCache.Get(realOp.NewDir.Ref)
			if newNode != nil {
				changes = append(changes, NodeChange{
					Node:       newNode,
					DirUpdated: []string{realOp.NewName},
				})
			}
		} else {
			newNode = oldNode
			if oldNode != nil {
				// Add another name to the existing NodeChange.
				changes[len(changes)-1].DirUpdated =
					append(changes[len(changes)-1].DirUpdated, realOp.NewName)
			}
		}

		if oldNode != nil {
			var newNodeID NodeID
			if newNode != nil {
				newNodeID = newNode.GetID()
			}
			fbo.log.CDebugf(ctx, "notifyOneOp: rename %v from %s/%p to %s/%p",
				realOp.Renamed, realOp.OldName, oldNode.GetID(), realOp.NewName,
				newNodeID)

			if newNode == nil {
				if childNode :=
					fbo.nodeCache.Get(realOp.Renamed); childNode != nil {
					// if the childNode exists, we still have to update
					// its path to go through the new node.  That means
					// creating nodes for all the intervening paths.
					// Unfortunately we don't have enough information to
					// know what the newPath is; we have to guess it from
					// the updates.
					var err error
					newNode, err =
						fbo.searchForNode(ctx, realOp.NewDir.Ref, realOp, md)
					if newNode == nil {
						fbo.log.CErrorf(ctx, "Couldn't find the new node: %v",
							err)
					}
				}
			}

			if newNode != nil {
				// If new node exists as well, unlink any previously
				// existing entry and move the node.
				var unrefPtr BlockPointer
				if oldNode != newNode {
					unrefPtr = realOp.NewDir.Unref
				} else {
					unrefPtr = realOp.OldDir.Unref
				}
				err := fbo.unlinkFromCache(op, unrefPtr, newNode, realOp.NewName)
				if err != nil {
					fbo.log.CErrorf(ctx, "Couldn't unlink from cache: %v", err)
					return
				}
				err = fbo.nodeCache.Move(realOp.Renamed, newNode, realOp.NewName)
				if err != nil {
					fbo.log.CErrorf(ctx, "Couldn't move node in cache: %v", err)
					return
				}
			}
		}
	case *syncOp:
		node := fbo.nodeCache.Get(realOp.File.Ref)
		if node == nil {
			return
		}
		fbo.log.CDebugf(ctx, "notifyOneOp: sync %d writes in node %p",
			len(realOp.Writes), node.GetID())

		changes = append(changes, NodeChange{
			Node:        node,
			FileUpdated: realOp.Writes,
		})
	case *setAttrOp:
		node := fbo.nodeCache.Get(realOp.Dir.Ref)
		if node == nil {
			return
		}
		fbo.log.CDebugf(ctx, "notifyOneOp: setAttr %s for file %s in node %p",
			realOp.Attr, realOp.Name, node.GetID())

		p, err := fbo.pathFromNodeForRead(node)
		if err != nil {
			return
		}
		childPath := p.ChildPathNoPtr(realOp.Name)

		// find the node for the actual change; requires looking up
		// the child entry to get the BlockPointer, unfortunately.
		_, de, err := fbo.getEntry(ctx, lState, md, childPath)
		if err != nil {
			return
		}

		childNode := fbo.nodeCache.Get(de.BlockPointer)
		if childNode == nil {
			return
		}

		// Fix up any cached de entry
		err = func() error {
			fbo.cacheLock.Lock()
			defer fbo.cacheLock.Unlock()
			dirEntry, ok := fbo.deCache[p.tailPointer()]
			if !ok {
				return nil
			}
			fileEntry, ok := dirEntry[de.BlockPointer]
			if !ok {
				return nil
			}
			// Get the real dir block; we can't use getEntry()
			// since it swaps in the cached dir entry.
			dblock, err := fbo.getDirLocked(ctx, lState, md,
				p, mdReadNeedIdentify)
			if err != nil {
				return err
			}

			realEntry, ok := dblock.Children[realOp.Name]
			if !ok {
				return nil
			}

			switch realOp.Attr {
			case exAttr:
				fileEntry.Type = realEntry.Type
			case mtimeAttr:
				fileEntry.Mtime = realEntry.Mtime
			}
			dirEntry[de.BlockPointer] = fileEntry
			return nil
		}()
		if err != nil {
			return
		}

		changes = append(changes, NodeChange{
			Node: childNode,
		})
	}

	fbo.obsLock.RLock()
	defer fbo.obsLock.RUnlock()
	for _, obs := range fbo.observers {
		obs.BatchChanges(ctx, changes)
	}
}

// headLock must be taken for reading, at least
func (fbo *folderBranchOps) getCurrMDRevisionLocked() MetadataRevision {
	if fbo.head != nil {
		return fbo.head.Revision
	}
	return MetadataRevisionUninitialized
}

func (fbo *folderBranchOps) getCurrMDRevision(
	lState *lockState) MetadataRevision {
	fbo.headLock.RLock(lState)
	defer fbo.headLock.RUnlock(lState)
	return fbo.getCurrMDRevisionLocked()
}

func (fbo *folderBranchOps) reembedBlockChanges(ctx context.Context,
	lState *lockState, rmds []*RootMetadata) error {
	// if any of the operations have unembedded block ops, fetch those
	// now and fix them up.  TODO: parallelize me.
	for _, rmd := range rmds {
		info := rmd.data.Changes.Info
		if info.BlockPointer == zeroPtr {
			continue
		}

		fblock, err := fbo.getFileBlockForReading(ctx, lState, rmd,
			info.BlockPointer, fbo.folderBranch.Branch, path{})
		if err != nil {
			return err
		}

		err = fbo.config.Codec().Decode(fblock.Contents, &rmd.data.Changes)
		if err != nil {
			return err
		}
		// The changes block pointer is an implicit ref block
		rmd.data.Changes.Ops[0].AddRefBlock(info.BlockPointer)
		rmd.data.cachedChanges.Info = info
	}
	return nil
}

type applyMDUpdatesFunc func(context.Context, *lockState, []*RootMetadata) error

// mdWriterLock must be held by the caller
func (fbo *folderBranchOps) applyMDUpdatesLocked(ctx context.Context,
	lState *lockState, rmds []*RootMetadata) error {
	fbo.headLock.Lock(lState)
	defer fbo.headLock.Unlock(lState)

	// if we have staged changes, ignore all updates until conflict
	// resolution kicks in.  TODO: cache these for future use.
	if fbo.staged {
		if len(rmds) > 0 {
			unmergedRev := MetadataRevisionUninitialized
			if fbo.head != nil {
				unmergedRev = fbo.head.Revision
			}
			fbo.cr.Resolve(unmergedRev, rmds[len(rmds)-1].Revision)
		}
		return errors.New("Ignoring MD updates while local updates are staged")
	}

	// Don't allow updates while we're in the dirty state; the next
	// sync will put us into an unmerged state anyway and we'll
	// require conflict resolution.
	if fbo.getState() != cleanState {
		return errors.New("Ignoring MD updates while writes are dirty")
	}

	fbo.reembedBlockChanges(ctx, lState, rmds)

	for _, rmd := range rmds {
		// check that we're applying the expected MD revision
		if rmd.Revision <= fbo.getCurrMDRevisionLocked() {
			// Already caught up!
			continue
		}
		if rmd.Revision != fbo.getCurrMDRevisionLocked()+1 {
			return MDUpdateApplyError{rmd.Revision,
				fbo.getCurrMDRevisionLocked()}
		}

		err := fbo.setHeadLocked(ctx, lState, rmd)
		if err != nil {
			return err
		}
		// No new operations in these.
		if rmd.IsWriterMetadataCopiedSet() {
			continue
		}
		for _, op := range rmd.data.Changes.Ops {
			fbo.notifyOneOpLocked(ctx, lState, op, rmd)
		}
	}
	return nil
}

// mdWriterLock must be held by the caller
func (fbo *folderBranchOps) undoMDUpdatesLocked(ctx context.Context,
	lState *lockState, rmds []*RootMetadata) error {
	fbo.headLock.Lock(lState)
	defer fbo.headLock.Unlock(lState)

	// Don't allow updates while we're in the dirty state; the next
	// sync will put us into an unmerged state anyway and we'll
	// require conflict resolution.
	if fbo.getState() != cleanState {
		return NotPermittedWhileDirtyError{}
	}

	fbo.reembedBlockChanges(ctx, lState, rmds)

	// go backwards through the updates
	for i := len(rmds) - 1; i >= 0; i-- {
		rmd := rmds[i]
		// on undo, it's ok to re-apply the current revision since you
		// need to invert all of its ops.
		if rmd.Revision != fbo.getCurrMDRevisionLocked() &&
			rmd.Revision != fbo.getCurrMDRevisionLocked()-1 {
			return MDUpdateInvertError{rmd.Revision,
				fbo.getCurrMDRevisionLocked()}
		}

		err := fbo.setHeadLocked(ctx, lState, rmd)
		if err != nil {
			return err
		}

		// iterate the ops in reverse and invert each one
		ops := rmd.data.Changes.Ops
		for j := len(ops) - 1; j >= 0; j-- {
			fbo.notifyOneOpLocked(ctx, lState, invertOpForLocalNotifications(ops[j]), rmd)
		}
	}
	return nil
}

func (fbo *folderBranchOps) applyMDUpdates(ctx context.Context,
	lState *lockState, rmds []*RootMetadata) error {
	fbo.mdWriterLock.Lock(lState)
	defer fbo.mdWriterLock.Unlock(lState)
	return fbo.applyMDUpdatesLocked(ctx, lState, rmds)
}

// Assumes all necessary locking is either already done by caller, or
// is done by applyFunc.
func (fbo *folderBranchOps) getAndApplyMDUpdates(ctx context.Context,
	lState *lockState, applyFunc applyMDUpdatesFunc) error {
	// first look up all MD revisions newer than my current head
	start := fbo.getCurrMDRevision(lState) + 1
	rmds, err := getMergedMDUpdates(ctx, fbo.config, fbo.id(), start)
	if err != nil {
		return err
	}

	err = applyFunc(ctx, lState, rmds)
	if err != nil {
		return err
	}
	return nil
}

func (fbo *folderBranchOps) getUnmergedMDUpdates(
	ctx context.Context, lState *lockState) (
	MetadataRevision, []*RootMetadata, error) {
	// acquire mdWriterLock to read the current branch ID.
	bid := func() BranchID {
		fbo.mdWriterLock.Lock(lState)
		defer fbo.mdWriterLock.Unlock(lState)
		return fbo.bid
	}()
	return getUnmergedMDUpdates(ctx, fbo.config, fbo.id(),
		bid, fbo.getCurrMDRevision(lState))
}

// mdWriterLock should be held by caller.
func (fbo *folderBranchOps) getUnmergedMDUpdatesLocked(
	ctx context.Context, lState *lockState) (
	MetadataRevision, []*RootMetadata, error) {
	return getUnmergedMDUpdates(ctx, fbo.config, fbo.id(),
		fbo.bid, fbo.getCurrMDRevision(lState))
}

// mdWriterLock should be held by caller.  Returns a list of block
// pointers that were created during the staged era.
func (fbo *folderBranchOps) undoUnmergedMDUpdatesLocked(
	ctx context.Context, lState *lockState) ([]BlockPointer, error) {
	currHead, unmergedRmds, err := fbo.getUnmergedMDUpdatesLocked(ctx, lState)
	if err != nil {
		return nil, err
	}

	err = fbo.undoMDUpdatesLocked(ctx, lState, unmergedRmds)
	if err != nil {
		return nil, err
	}

	// We have arrived at the branch point.  The new root is
	// the previous revision from the current head.  Find it
	// and apply.  TODO: somehow fake the current head into
	// being currHead-1, so that future calls to
	// applyMDUpdates will fetch this along with the rest of
	// the updates.
	fbo.setStagedLocked(lState, false, NullBranchID)

	rmds, err := getMDRange(ctx, fbo.config, fbo.id(), NullBranchID,
		currHead, currHead, Merged)
	if err != nil {
		return nil, err
	}
	if len(rmds) == 0 {
		return nil, fmt.Errorf("Couldn't find the branch point %d", currHead)
	}
	err = fbo.setHeadLocked(ctx, lState, rmds[0])
	if err != nil {
		return nil, err
	}

	// Now that we're back on the merged branch, forget about all the
	// unmerged updates
	mdcache := fbo.config.MDCache()
	for _, rmd := range unmergedRmds {
		mdcache.Delete(rmd)
	}

	// Return all new refs
	var unmergedPtrs []BlockPointer
	for _, rmd := range unmergedRmds {
		for _, op := range rmd.data.Changes.Ops {
			for _, ptr := range op.Refs() {
				if ptr != zeroPtr {
					unmergedPtrs = append(unmergedPtrs, ptr)
				}
			}
			for _, update := range op.AllUpdates() {
				if update.Ref != zeroPtr {
					unmergedPtrs = append(unmergedPtrs, update.Ref)
				}
			}
		}
	}

	return unmergedPtrs, nil
}

// TODO: remove once we have automatic conflict resolution
func (fbo *folderBranchOps) UnstageForTesting(
	ctx context.Context, folderBranch FolderBranch) (err error) {
	fbo.log.CDebugf(ctx, "UnstageForTesting")
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	if folderBranch != fbo.folderBranch {
		return WrongOpsError{fbo.folderBranch, folderBranch}
	}

	lState := makeFBOLockState()

	if !fbo.getStaged(lState) {
		// no-op
		return nil
	}

	if fbo.getState() != cleanState {
		return NotPermittedWhileDirtyError{}
	}

	// launch unstaging in a new goroutine, because we don't want to
	// use the provided context because upper layers might ignore our
	// notifications if we do.  But we still want to wait for the
	// context to cancel.
	c := make(chan error, 1)
	logTags := make(logger.CtxLogTags)
	logTags[CtxFBOIDKey] = CtxFBOOpID
	ctxWithTags := logger.NewContextWithLogTags(context.Background(), logTags)
	id, err := MakeRandomRequestID()
	if err != nil {
		fbo.log.Warning("Couldn't generate a random request ID: %v", err)
	} else {
		ctxWithTags = context.WithValue(ctxWithTags, CtxFBOIDKey, id)
	}
	freshCtx, cancel := context.WithCancel(ctxWithTags)
	defer cancel()
	fbo.log.CDebugf(freshCtx, "Launching new context for UnstageForTesting")
	go func() {
		lState := makeFBOLockState()
		fbo.mdWriterLock.Lock(lState)
		defer fbo.mdWriterLock.Unlock(lState)

		// fetch all of my unstaged updates, and undo them one at a time
		bid, wasStaged := fbo.bid, fbo.staged
		unmergedPtrs, err := fbo.undoUnmergedMDUpdatesLocked(freshCtx, lState)
		if err != nil {
			c <- err
			return
		}

		// let the server know we no longer have need
		if wasStaged {
			err = fbo.config.MDServer().PruneBranch(freshCtx, fbo.id(), bid)
			if err != nil {
				c <- err
				return
			}
		}

		// now go forward in time, if possible
		err = fbo.getAndApplyMDUpdates(freshCtx, lState,
			fbo.applyMDUpdatesLocked)
		if err != nil {
			c <- err
			return
		}

		md, err := fbo.getMDForWriteLocked(ctx, lState)
		if err != nil {
			c <- err
			return
		}

		// Finally, create a gcOp with the newly-unref'd pointers.
		gcOp := newGCOp()
		for _, ptr := range unmergedPtrs {
			gcOp.AddUnrefBlock(ptr)
		}
		md.AddOp(gcOp)
		c <- fbo.finalizeMDWriteLocked(ctx, lState, md, &blockPutState{})
	}()

	select {
	case err := <-c:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Rekey rekeys the given folder.
func (fbo *folderBranchOps) Rekey(ctx context.Context, tlf TlfID) (err error) {
	fbo.log.CDebugf(ctx, "Rekey")
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	fb := FolderBranch{tlf, MasterBranch}
	if fb != fbo.folderBranch {
		return WrongOpsError{fbo.folderBranch, fb}
	}

	if fbo.staged {
		return errors.New("Can't rekey while staged.")
	}

	uid, err := fbo.config.KBPKI().GetCurrentUID(ctx)

	if err != nil {
		return err
	}

	cryptKey, err := fbo.config.KBPKI().GetCurrentCryptPublicKey(ctx)
	if err != nil {
		return err
	}

	lState := makeFBOLockState()

	fbo.mdWriterLock.Lock(lState)
	defer fbo.mdWriterLock.Unlock(lState)

	md, rekeyWasSet, err := fbo.getMDForRekeyWriteLocked(ctx, lState)
	if err != nil {
		return err
	}

	if md.IsWriter(uid, cryptKey.kid) {
		// TODO: allow readers to rekey just themself
		rekeyDone, err := fbo.config.KeyManager().Rekey(ctx, md)
		if err != nil {
			return err
		}
		// TODO: implement a "forced" option that rekeys even when the
		// devices haven't changed?
		if !rekeyDone {
			fbo.log.CDebugf(ctx, "No rekey necessary")
			return nil
		}
		// clear the rekey bit
		md.Flags &= ^MetadataFlagRekey
	} else if rekeyWasSet {
		// Readers shouldn't re-set the rekey bit.
		fbo.log.CDebugf(ctx, "Rekey bit already set")
		return nil
	}

	// add an empty operation to satisfy assumptions elsewhere
	md.AddOp(newGCOp())

	// we still let readers push a new md block since it will simply be a rekey bit block.
	err = fbo.finalizeMDWriteLocked(ctx, lState, md, &blockPutState{})
	if err != nil {
		return err
	}

	// send rekey finish notification
	handle := md.GetTlfHandle()
	fbo.config.Reporter().Notify(ctx, rekeyNotification(ctx, fbo.config, handle, true))

	return nil
}

func (fbo *folderBranchOps) SyncFromServer(
	ctx context.Context, folderBranch FolderBranch) (err error) {
	fbo.log.CDebugf(ctx, "SyncFromServer")
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	if folderBranch != fbo.folderBranch {
		return WrongOpsError{fbo.folderBranch, folderBranch}
	}

	lState := makeFBOLockState()

	if fbo.getStaged(lState) {
		if err := fbo.cr.Wait(ctx); err != nil {
			return err
		}
		// If we are still staged after the wait, then we have a problem.
		if fbo.getStaged(lState) {
			return fmt.Errorf("Conflict resolution didn't take us out of " +
				"staging.")
		}
	}

	if fbo.getState() != cleanState {
		fbo.cacheLock.Lock()
		defer fbo.cacheLock.Unlock()
		for parent, deMap := range fbo.deCache {
			for file := range deMap {
				fbo.log.CDebugf(ctx, "DeCache entry left: %v -> %v", parent, file)
			}
		}
		return errors.New("Can't sync from server while dirty.")
	}

	if err := fbo.getAndApplyMDUpdates(ctx, lState, fbo.applyMDUpdates); err != nil {
		if applyErr, ok := err.(MDUpdateApplyError); ok {
			if applyErr.rev == applyErr.curr {
				fbo.log.CDebugf(ctx, "Already up-to-date with server")
				return nil
			}
		}
		return err
	}

	// Wait for all the asynchronous block archiving to hit the block
	// server.
	fbo.mdWriterLock.Lock(lState)
	defer fbo.mdWriterLock.Unlock(lState)
	fbo.archiveGroup.Wait()

	return nil
}

// CtxFBOTagKey is the type used for unique context tags within folderBranchOps
type CtxFBOTagKey int

const (
	// CtxFBOIDKey is the type of the tag for unique operation IDs
	// within folderBranchOps.
	CtxFBOIDKey CtxFBOTagKey = iota
)

// CtxFBOOpID is the display name for the unique operation
// folderBranchOps ID tag.
const CtxFBOOpID = "FBOID"

// Run the passed function with a context that's canceled on shutdown.
func (fbo *folderBranchOps) runUnlessShutdown(fn func(ctx context.Context) error) error {
	// Tag each request with a unique ID
	logTags := make(logger.CtxLogTags)
	logTags[CtxFBOIDKey] = CtxFBOOpID
	ctx := logger.NewContextWithLogTags(context.Background(), logTags)
	id, err := MakeRandomRequestID()
	if err != nil {
		fbo.log.Warning("Couldn't generate a random request ID: %v", err)
	} else {
		ctx = context.WithValue(ctx, CtxFBOIDKey, id)
	}

	ctx, cancelFunc := context.WithCancel(ctx)
	defer cancelFunc()
	errChan := make(chan error, 1)
	go func() {
		errChan <- fn(ctx)
	}()

	select {
	case err := <-errChan:
		return err
	case <-fbo.shutdownChan:
		return errors.New("shutdown received")
	}
}

func (fbo *folderBranchOps) registerForUpdates() {
	var err error
	var updateChan <-chan error

	err = fbo.runUnlessShutdown(func(ctx context.Context) (err error) {
		lState := makeFBOLockState()

		currRev := fbo.getCurrMDRevision(lState)
		fbo.log.CDebugf(ctx, "Registering for updates (curr rev = %d)", currRev)
		defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

		// this will retry on connectivity issues. TODO: backoff on explicit
		// throttle errors from the back-end inside MDServer.
		updateChan, err = fbo.config.MDServer().RegisterForUpdate(ctx, fbo.id(),
			currRev)
		return err
	})

	if err != nil {
		// TODO: we should probably display something or put us in some error
		// state obvious to the user.
		return
	}

	// successful registration; now, wait for an update or a shutdown
	go fbo.runUnlessShutdown(func(ctx context.Context) (err error) {
		fbo.log.CDebugf(ctx, "Waiting for updates")
		defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

		lState := makeFBOLockState()

		for {
			select {
			case err := <-updateChan:
				fbo.log.CDebugf(ctx, "Got an update: %v", err)
				defer fbo.registerForUpdates()
				if err != nil {
					return err
				}
				err = fbo.getAndApplyMDUpdates(ctx, lState, fbo.applyMDUpdates)
				if err != nil {
					fbo.log.CDebugf(ctx, "Got an error while applying "+
						"updates: %v", err)
					if _, ok := err.(NotPermittedWhileDirtyError); ok {
						// If this fails because of outstanding dirty
						// files, delay a bit to avoid wasting RPCs
						// and CPU.
						time.Sleep(1 * time.Second)
					}
					return err
				}
				return nil
			case unpause := <-fbo.updatePauseChan:
				fbo.log.CInfof(ctx, "Updates paused")
				// wait to be unpaused
				select {
				case <-unpause:
					fbo.log.CInfof(ctx, "Updates unpaused")
				case <-ctx.Done():
					return ctx.Err()
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})
}

func (fbo *folderBranchOps) getDirtyPointers() []BlockPointer {
	fbo.cacheLock.Lock()
	defer fbo.cacheLock.Unlock()
	var dirtyPtrs []BlockPointer
	for _, entries := range fbo.deCache {
		for ptr := range entries {
			dirtyPtrs = append(dirtyPtrs, ptr)
		}
	}
	return dirtyPtrs
}

func (fbo *folderBranchOps) backgroundFlusher(betweenFlushes time.Duration) {
	ticker := time.NewTicker(betweenFlushes)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			dirtyPtrs := fbo.getDirtyPointers()
			fbo.runUnlessShutdown(func(ctx context.Context) (err error) {
				for _, ptr := range dirtyPtrs {
					node := fbo.nodeCache.Get(ptr)
					if node == nil {
						continue
					}
					err := fbo.Sync(ctx, node)
					if err != nil {
						// Just log the warning and keep trying to
						// sync the rest of the dirty files.
						p := fbo.nodeCache.PathFromNode(node)
						fbo.log.CWarningf(ctx, "Couldn't sync dirty file with ptr=%v, nodeID=%v, and path=%v: %v",
							ptr, node.GetID(), p, err)
					}
				}
				return nil
			})
		case <-fbo.shutdownChan:
			return
		}
	}
}

// finalizeResolution caches all the blocks, and writes the new MD to
// the merged branch, failing if there is a conflict.  It also sends
// out the given newOps notifications locally.  This is used for
// completing conflict resolution.
func (fbo *folderBranchOps) finalizeResolution(ctx context.Context,
	lState *lockState, md *RootMetadata, bps *blockPutState,
	newOps []op) error {

	// Take the writer lock.
	fbo.mdWriterLock.Lock(lState)
	defer fbo.mdWriterLock.Unlock(lState)

	// Put the blocks into the cache so that, even if we fail below,
	// future attempts may reuse the blocks.
	err := fbo.finalizeBlocks(bps)
	if err != nil {
		return err
	}

	// Last chance to get pre-empted.
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Put the MD.  If there's a conflict, abort the whole process and
	// let CR restart itself.
	err = fbo.config.MDOps().Put(ctx, md)
	doUnmergedPut := fbo.isRevisionConflict(err)
	if doUnmergedPut {
		fbo.log.CDebugf(ctx, "Got a conflict after resolution; aborting CR")
		return err
	}
	if err != nil {
		return err
	}
	err = fbo.config.MDServer().PruneBranch(ctx, fbo.id(), fbo.bid)
	if err != nil {
		return err
	}

	// Queue a rekey if the bit was set.
	if md.IsRekeySet() {
		defer fbo.config.RekeyQueue().Enqueue(md.ID)
	}

	// Set the head to the new MD.
	fbo.headLock.Lock(lState)
	defer fbo.headLock.Unlock(lState)
	err = fbo.setHeadLocked(ctx, lState, md)
	if err != nil {
		fbo.log.CWarningf(ctx, "Couldn't set local MD head after a "+
			"successful put: %v", err)
		return err
	}
	fbo.setStagedLocked(lState, false, NullBranchID)

	// Archive the old, unref'd blocks
	fbo.archiveLocked(md)

	// notifyOneOp for every fixed-up merged op.
	for _, op := range newOps {
		fbo.notifyOneOpLocked(ctx, lState, op, md)
	}
	return nil
}

func (fbo *folderBranchOps) archiveBlocksInBackground() {
	for {
		select {
		case md := <-fbo.archiveChan:
			var ptrs []BlockPointer
			for _, op := range md.data.Changes.Ops {
				ptrs = append(ptrs, op.Unrefs()...)
				for _, update := range op.AllUpdates() {
					ptrs = append(ptrs, update.Unref)
				}
			}
			fbo.runUnlessShutdown(func(ctx context.Context) (err error) {
				defer fbo.archiveGroup.Done()
				fbo.log.CDebugf(ctx, "Archiving %d block pointers as a result "+
					"of revision %d", len(ptrs), md.Revision)
				err = fbo.config.BlockOps().Archive(ctx, md, ptrs)
				if err != nil {
					fbo.log.CWarningf(ctx, "Couldn't archive blocks: %v", err)
				}
				return err
			})
		case <-fbo.shutdownChan:
			return
		}
	}
}

// GetUpdateHistory implements the KBFSOps interface for folderBranchOps
func (fbo *folderBranchOps) GetUpdateHistory(ctx context.Context,
	folderBranch FolderBranch) (history TLFUpdateHistory, err error) {
	fbo.log.CDebugf(ctx, "GetUpdateHistory")
	defer func() { fbo.log.CDebugf(ctx, "Done: %v", err) }()

	if folderBranch != fbo.folderBranch {
		return TLFUpdateHistory{}, WrongOpsError{fbo.folderBranch, folderBranch}
	}

	lState := makeFBOLockState()

	rmds, err := getMergedMDUpdates(ctx, fbo.config, fbo.id(),
		MetadataRevisionInitial)
	if err != nil {
		return TLFUpdateHistory{}, err
	}
	err = fbo.reembedBlockChanges(ctx, lState, rmds)
	if err != nil {
		return TLFUpdateHistory{}, err
	}

	if len(rmds) > 0 {
		rmd := rmds[len(rmds)-1]
		history.ID = rmd.ID.String()
		history.Name = rmd.GetTlfHandle().ToString(ctx, fbo.config)
	}
	history.Updates = make([]UpdateSummary, 0, len(rmds))
	writerNames := make(map[keybase1.UID]string)
	for _, rmd := range rmds {
		writer, ok := writerNames[rmd.LastModifyingWriter]
		if !ok {
			name, err := fbo.config.KBPKI().
				GetNormalizedUsername(ctx, rmd.LastModifyingWriter)
			if err != nil {
				return TLFUpdateHistory{}, err
			}
			writer = string(name)
			writerNames[rmd.LastModifyingWriter] = writer
		}
		updateSummary := UpdateSummary{
			Revision:  rmd.Revision,
			Date:      time.Unix(0, rmd.data.Dir.Mtime),
			Writer:    writer,
			LiveBytes: rmd.DiskUsage,
			Ops:       make([]OpSummary, 0, len(rmd.data.Changes.Ops)),
		}
		for _, op := range rmd.data.Changes.Ops {
			opSummary := OpSummary{
				Op:      op.String(),
				Refs:    make([]string, 0, len(op.Refs())),
				Unrefs:  make([]string, 0, len(op.Unrefs())),
				Updates: make(map[string]string),
			}
			for _, ptr := range op.Refs() {
				opSummary.Refs = append(opSummary.Refs, ptr.String())
			}
			for _, ptr := range op.Unrefs() {
				opSummary.Unrefs = append(opSummary.Unrefs, ptr.String())
			}
			for _, update := range op.AllUpdates() {
				opSummary.Updates[update.Unref.String()] = update.Ref.String()
			}
			updateSummary.Ops = append(updateSummary.Ops, opSummary)
		}
		history.Updates = append(history.Updates, updateSummary)
	}
	return history, nil
}
