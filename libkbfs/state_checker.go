// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/keybase/client/go/logger"
	"golang.org/x/net/context"
)

// StateChecker verifies that the server-side state for KBFS is
// consistent.  Useful mostly for testing because it isn't scalable
// and loads all the state in memory.
type StateChecker struct {
	config IFCERFTConfig
	log    logger.Logger
}

// NewStateChecker returns a new StateChecker instance.
func NewStateChecker(config IFCERFTConfig) *StateChecker {
	return &StateChecker{config, config.MakeLogger("")}
}

// findAllFileBlocks adds all file blocks found under this block to
// the blocksFound map, if the given path represents an indirect
// block.
func (sc *StateChecker) findAllFileBlocks(ctx context.Context,
	lState *lockState, ops *folderBranchOps, md *IFCERFTRootMetadata, file path,
	blockSizes map[IFCERFTBlockPointer]uint32) error {
	fblock, err := ops.blocks.GetFileBlockForReading(ctx, lState, md,
		file.tailPointer(), file.Branch, file)
	if err != nil {
		return err
	}

	if !fblock.IsInd {
		return nil
	}

	parentPath := file.parentPath()
	for _, childPtr := range fblock.IPtrs {
		blockSizes[childPtr.IFCERFTBlockPointer] = childPtr.EncodedSize
		p := parentPath.ChildPath(file.tailName(), childPtr.IFCERFTBlockPointer)
		err := sc.findAllFileBlocks(ctx, lState, ops, md, p, blockSizes)
		if err != nil {
			return err
		}
	}
	return nil
}

// findAllBlocksInPath adds all blocks found within this directory to
// the blockSizes map, and then recursively checks all
// subdirectories.
func (sc *StateChecker) findAllBlocksInPath(ctx context.Context,
	lState *lockState, ops *folderBranchOps, md *IFCERFTRootMetadata, dir path,
	blockSizes map[IFCERFTBlockPointer]uint32) error {
	dblock, err := ops.blocks.GetDirBlockForReading(ctx, lState, md,
		dir.tailPointer(), dir.Branch, dir)
	if err != nil {
		return err
	}

	for name, de := range dblock.Children {
		if de.Type == IFCERFTSym {
			continue
		}

		blockSizes[de.IFCERFTBlockPointer] = de.EncodedSize
		p := dir.ChildPath(name, de.IFCERFTBlockPointer)

		if de.Type == IFCERFTDir {
			err := sc.findAllBlocksInPath(ctx, lState, ops, md, p, blockSizes)
			if err != nil {
				return err
			}
		} else {
			// If it's a file, check to see if it's indirect.
			err := sc.findAllFileBlocks(ctx, lState, ops, md, p, blockSizes)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (sc *StateChecker) getLastGCRevisionTime(ctx context.Context,
	tlf IFCERFTTlfID) time.Time {
	config, ok := sc.config.(*ConfigLocal)
	if !ok {
		return time.Time{}
	}

	var latestTime time.Time
	for _, c := range *config.allKnownConfigsForTesting {
		ops := c.KBFSOps().(*KBFSOpsStandard).getOpsNoAdd(
			IFCERFTFolderBranch{tlf, IFCERFTMasterBranch})
		rt := ops.fbm.getLastReclamationTime()
		if rt.After(latestTime) {
			latestTime = rt
		}
	}
	if latestTime == (time.Time{}) {
		return latestTime
	}

	sc.log.CDebugf(ctx, "Last reclamation time for TLF %s: %s",
		tlf, latestTime)
	return latestTime.Add(-sc.config.QuotaReclamationMinUnrefAge())
}

// CheckMergedState verifies that the state for the given tlf is
// consistent.
func (sc *StateChecker) CheckMergedState(ctx context.Context, tlf IFCERFTTlfID) error {
	// Blow away MD cache so we don't have any lingering re-embedded
	// block changes (otherwise we won't be able to learn their sizes).
	sc.config.SetMDCache(NewMDCacheStandard(5000))

	// Fetch all the MD updates for this folder, and use the block
	// change lists to build up the set of currently referenced blocks.
	rmds, err := getMergedMDUpdates(ctx, sc.config, tlf,
		MetadataRevisionInitial)
	if err != nil {
		return err
	}
	if len(rmds) == 0 {
		sc.log.CDebugf(ctx, "No state to check for folder %s", tlf)
		return nil
	}

	lState := makeFBOLockState()

	// Re-embed block changes.
	kbfsOps, ok := sc.config.KBFSOps().(*KBFSOpsStandard)
	if !ok {
		return errors.New("Unexpected KBFSOps type")
	}

	fb := IFCERFTFolderBranch{tlf, IFCERFTMasterBranch}
	ops := kbfsOps.getOpsNoAdd(fb)
	lastGCRevisionTime := sc.getLastGCRevisionTime(ctx, tlf)

	// Build the expected block list.
	expectedLiveBlocks := make(map[IFCERFTBlockPointer]bool)
	expectedRef := uint64(0)
	archivedBlocks := make(map[IFCERFTBlockPointer]bool)
	actualLiveBlocks := make(map[IFCERFTBlockPointer]uint32)

	// See what the last GC op revision is.  All unref'd pointers from
	// that revision or earlier should be deleted from the block
	// server.
	gcRevision := MetadataRevisionUninitialized
	for _, rmd := range rmds {
		// Don't process copies.
		if rmd.IsWriterMetadataCopiedSet() {
			continue
		}

		for _, op := range rmd.data.Changes.Ops {
			gcOp, ok := op.(*gcOp)
			if !ok {
				continue
			}
			gcRevision = gcOp.LatestRev
		}
	}

	for _, rmd := range rmds {
		// Don't process copies.
		if rmd.IsWriterMetadataCopiedSet() {
			continue
		}
		// Any unembedded block changes also count towards the actual size
		if info := rmd.data.cachedChanges.Info; info.IFCERFTBlockPointer != zeroPtr {
			sc.log.CDebugf(ctx, "Unembedded block change: %v, %d",
				info.IFCERFTBlockPointer, info.EncodedSize)
			actualLiveBlocks[info.IFCERFTBlockPointer] = info.EncodedSize
		}

		var hasGCOp bool
		for _, op := range rmd.data.Changes.Ops {
			_, isGCOp := op.(*gcOp)
			hasGCOp = hasGCOp || isGCOp

			opRefs := make(map[IFCERFTBlockPointer]bool)
			for _, ptr := range op.Refs() {
				if ptr != zeroPtr {
					expectedLiveBlocks[ptr] = true
					opRefs[ptr] = true
				}
			}
			if _, ok := op.(*gcOp); !ok {
				for _, ptr := range op.Unrefs() {
					delete(expectedLiveBlocks, ptr)
					if ptr != zeroPtr {
						// If the revision has been garbage-collected,
						// or if the pointer has been referenced and
						// unreferenced within the same op (which
						// indicates a failed and retried sync), the
						// corresponding block should already be
						// cleaned up.
						if rmd.Revision <= gcRevision || opRefs[ptr] {
							delete(archivedBlocks, ptr)
						} else {
							archivedBlocks[ptr] = true
						}
					}
				}
			}
			for _, update := range op.AllUpdates() {
				delete(expectedLiveBlocks, update.Unref)
				if update.Unref != zeroPtr && update.Ref != update.Unref {
					if rmd.Revision <= gcRevision {
						delete(archivedBlocks, update.Unref)
					} else {
						archivedBlocks[update.Unref] = true
					}
				}
				if update.Ref != zeroPtr {
					expectedLiveBlocks[update.Ref] = true
				}
			}
		}
		expectedRef += rmd.RefBytes
		expectedRef -= rmd.UnrefBytes

		if len(rmd.data.Changes.Ops) == 1 && hasGCOp {
			// Don't check GC status for GC revisions
			continue
		}

		// Make sure that if this revision should be covered by a GC
		// op, it is.  Note that this assumes that if QR is ever run,
		// it will be run completely and not left partially done due
		// to there being too many pointers to collect in one sweep.
		mtime := time.Unix(0, rmd.data.Dir.Mtime)
		if !lastGCRevisionTime.Before(mtime) {
			if rmd.Revision > gcRevision {
				return fmt.Errorf("Revision %d happened before the last "+
					"gc time %s, but was not included in the latest gc op "+
					"revision %d", rmd.Revision, lastGCRevisionTime, gcRevision)
			}
		}
	}
	sc.log.CDebugf(ctx, "Folder %v has %d expected live blocks, total %d bytes",
		tlf, len(expectedLiveBlocks), expectedRef)

	currMD := rmds[len(rmds)-1]
	expectedUsage := currMD.DiskUsage
	if expectedUsage != expectedRef {
		return fmt.Errorf("Expected ref bytes %d doesn't match latest disk "+
			"usage %d", expectedRef, expectedUsage)
	}

	// Then, using the current MD head, start at the root of the FS
	// and recursively walk the directory tree to find all the blocks
	// that are currently accessible.
	rootNode, _, _, err := ops.getRootNode(ctx)
	if err != nil {
		return err
	}
	rootPath := ops.nodeCache.PathFromNode(rootNode)
	if g, e := rootPath.tailPointer(), currMD.data.Dir.IFCERFTBlockPointer; g != e {
		return fmt.Errorf("Current MD root pointer %v doesn't match root "+
			"node pointer %v", e, g)
	}
	actualLiveBlocks[rootPath.tailPointer()] = currMD.data.Dir.EncodedSize
	if err := sc.findAllBlocksInPath(ctx, lState, ops, currMD, rootPath,
		actualLiveBlocks); err != nil {
		return err
	}
	sc.log.CDebugf(ctx, "Folder %v has %d actual live blocks",
		tlf, len(actualLiveBlocks))

	// Compare the two and see if there are any differences. Don't use
	// reflect.DeepEqual so we can print out exactly what's wrong.
	var extraBlocks []IFCERFTBlockPointer
	actualSize := uint64(0)
	for ptr, size := range actualLiveBlocks {
		actualSize += uint64(size)
		if !expectedLiveBlocks[ptr] {
			extraBlocks = append(extraBlocks, ptr)
		}
	}
	if len(extraBlocks) != 0 {
		sc.log.CWarningf(ctx, "%v: Extra live blocks found: %v",
			tlf, extraBlocks)
		return fmt.Errorf("Folder %v has inconsistent state", tlf)
	}
	var missingBlocks []IFCERFTBlockPointer
	for ptr := range expectedLiveBlocks {
		if _, ok := actualLiveBlocks[ptr]; !ok {
			missingBlocks = append(missingBlocks, ptr)
		}
	}
	if len(missingBlocks) != 0 {
		sc.log.CWarningf(ctx, "%v: Expected live blocks not found: %v",
			tlf, missingBlocks)
		return fmt.Errorf("Folder %v has inconsistent state", tlf)
	}

	if actualSize != expectedRef {
		return fmt.Errorf("Actual size %d doesn't match expected size %d",
			actualSize, expectedRef)
	}

	// Check that the set of referenced blocks matches exactly what
	// the block server knows about.
	bserverLocal, ok := sc.config.BlockServer().(blockServerLocal)
	if !ok {
		return errors.New("StateChecker only works against BlockServerLocal")
	}
	bserverKnownBlocks, err := bserverLocal.getAll(tlf)
	if err != nil {
		return err
	}

	blockRefsByID := make(map[BlockID]map[IFCERFTBlockRefNonce]blockRefLocalStatus)
	for ptr := range expectedLiveBlocks {
		if _, ok := blockRefsByID[ptr.ID]; !ok {
			blockRefsByID[ptr.ID] = make(map[IFCERFTBlockRefNonce]blockRefLocalStatus)
		}
		blockRefsByID[ptr.ID][ptr.RefNonce] = liveBlockRef
	}
	for ptr := range archivedBlocks {
		if _, ok := blockRefsByID[ptr.ID]; !ok {
			blockRefsByID[ptr.ID] = make(map[IFCERFTBlockRefNonce]blockRefLocalStatus)
		}
		blockRefsByID[ptr.ID][ptr.RefNonce] = archivedBlockRef
	}

	if g, e := bserverKnownBlocks, blockRefsByID; !reflect.DeepEqual(g, e) {
		for id, eRefs := range e {
			if gRefs := g[id]; !reflect.DeepEqual(gRefs, eRefs) {
				sc.log.CDebugf(ctx, "Refs for ID %v don't match.  "+
					"Got %v, expected %v", id, gRefs, eRefs)
			}
		}
		for id := range g {
			if eRefs, ok := e[id]; !ok {
				sc.log.CDebugf(ctx, "Did not find matching expected "+
					"ID for found block %v (with refs %v)", id, eRefs)
			}
		}

		return fmt.Errorf("Folder %v has inconsistent state", tlf)
	}

	// TODO: Check the archived and deleted blocks as well.
	return nil
}
