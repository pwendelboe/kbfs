// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"

	"github.com/keybase/client/go/logger"
	"golang.org/x/net/context"
)

// BlockServerDisk implements the BlockServer interface by just
// storing blocks in a local leveldb instance.
type BlockServerDisk struct {
	codec        IFCERFTCodec
	crypto       IFCERFTCrypto
	log          logger.Logger
	dirPath      string
	shutdownFunc func(logger.Logger)

	diskJournalLock sync.RWMutex
	// diskJournal is nil after Shutdown() is called.
	diskJournal map[IFCERFTTlfID]*bserverTlfJournal
}

var _ IFCERFTBlockServer = (*BlockServerDisk)(nil)

// newBlockServerDisk constructs a new BlockServerDisk that stores
// its data in the given directory.
func newBlockServerDisk(
	config IFCERFTConfig, dirPath string, shutdownFunc func(logger.Logger)) *BlockServerDisk {
	bserv := &BlockServerDisk{
		config.Codec(),
		config.Crypto(),
		config.MakeLogger("BSD"),
		dirPath,
		shutdownFunc,
		sync.RWMutex{},
		make(map[IFCERFTTlfID]*bserverTlfJournal),
	}
	return bserv
}

// NewBlockServerDir constructs a new BlockServerDisk that stores
// its data in the given directory.
func NewBlockServerDir(config IFCERFTConfig, dirPath string) *BlockServerDisk {
	return newBlockServerDisk(config, dirPath, nil)
}

// NewBlockServerTempDir constructs a new BlockServerDisk that stores its
// data in a temp directory which is cleaned up on shutdown.
func NewBlockServerTempDir(config IFCERFTConfig) (*BlockServerDisk, error) {
	tempdir, err := ioutil.TempDir(os.TempDir(), "kbfs_bserver_tmp")
	if err != nil {
		return nil, err
	}
	return newBlockServerDisk(config, tempdir, func(log logger.Logger) {
		err := os.RemoveAll(tempdir)
		if err != nil {
			log.Warning("error removing %s: %s", tempdir, err)
		}
	}), nil
}

var errBlockServerDiskShutdown = errors.New("BlockServerDisk is shutdown")

func (b *BlockServerDisk) getJournal(tlfID IFCERFTTlfID) (*bserverTlfJournal, error) {
	storage, err := func() (*bserverTlfJournal, error) {
		b.diskJournalLock.RLock()
		defer b.diskJournalLock.RUnlock()
		if b.diskJournal == nil {
			return nil, errBlockServerDiskShutdown
		}
		return b.diskJournal[tlfID], nil
	}()

	if err != nil {
		return nil, err
	}

	if storage != nil {
		return storage, nil
	}

	b.diskJournalLock.Lock()
	defer b.diskJournalLock.Unlock()
	if b.diskJournal == nil {
		return nil, errBlockServerDiskShutdown
	}

	storage = b.diskJournal[tlfID]
	if storage != nil {
		return storage, nil
	}

	path := filepath.Join(b.dirPath, tlfID.String())
	storage, err = makeBserverTlfJournal(b.codec, b.crypto, path)
	if err != nil {
		return nil, err
	}

	b.diskJournal[tlfID] = storage
	return storage, nil
}

// Get implements the BlockServer interface for BlockServerDisk.
func (b *BlockServerDisk) Get(ctx context.Context, id BlockID, tlfID IFCERFTTlfID, context IFCERFTBlockContext) ([]byte, IFCERFTBlockCryptKeyServerHalf, error) {
	b.log.CDebugf(ctx, "BlockServerDisk.Get id=%s tlfID=%s context=%s",
		id, tlfID, context)
	diskJournal, err := b.getJournal(tlfID)
	if err != nil {
		return nil, IFCERFTBlockCryptKeyServerHalf{}, err
	}
	data, keyServerHalf, err := diskJournal.getData(id, context)
	if err != nil {
		return nil, IFCERFTBlockCryptKeyServerHalf{}, err
	}
	return data, keyServerHalf, nil
}

// Put implements the BlockServer interface for BlockServerDisk.
func (b *BlockServerDisk) Put(ctx context.Context, id BlockID, tlfID IFCERFTTlfID, context IFCERFTBlockContext, buf []byte,
	serverHalf IFCERFTBlockCryptKeyServerHalf) error {
	b.log.CDebugf(ctx, "BlockServerDisk.Put id=%s tlfID=%s context=%s size=%d",
		id, tlfID, context, len(buf))

	if context.GetRefNonce() != zeroBlockRefNonce {
		return fmt.Errorf("Can't Put() a block with a non-zero refnonce.")
	}

	diskJournal, err := b.getJournal(tlfID)
	if err != nil {
		return err
	}
	return diskJournal.putData(id, context, buf, serverHalf)
}

// AddBlockReference implements the BlockServer interface for BlockServerDisk.
func (b *BlockServerDisk) AddBlockReference(ctx context.Context, id BlockID,
	tlfID IFCERFTTlfID, context IFCERFTBlockContext) error {
	b.log.CDebugf(ctx, "BlockServerDisk.AddBlockReference id=%s "+
		"tlfID=%s context=%s", id, tlfID, context)
	diskJournal, err := b.getJournal(tlfID)
	if err != nil {
		return err
	}
	return diskJournal.addReference(id, context)
}

// RemoveBlockReference implements the BlockServer interface for
// BlockServerDisk.
func (b *BlockServerDisk) RemoveBlockReference(ctx context.Context,
	tlfID IFCERFTTlfID, contexts map[BlockID][]IFCERFTBlockContext) (
	liveCounts map[BlockID]int, err error) {
	b.log.CDebugf(ctx, "BlockServerDisk.RemoveBlockReference "+
		"tlfID=%s contexts=%v", tlfID, contexts)
	diskJournal, err := b.getJournal(tlfID)
	if err != nil {
		return nil, err
	}

	liveCounts = make(map[BlockID]int)
	for id, idContexts := range contexts {
		count, err := diskJournal.removeReferences(id, idContexts)
		if err != nil {
			return nil, err
		}
		liveCounts[id] = count
	}
	return liveCounts, nil
}

// ArchiveBlockReferences implements the BlockServer interface for
// BlockServerDisk.
func (b *BlockServerDisk) ArchiveBlockReferences(ctx context.Context,
	tlfID IFCERFTTlfID, contexts map[BlockID][]IFCERFTBlockContext) error {
	b.log.CDebugf(ctx, "BlockServerDisk.ArchiveBlockReferences "+
		"tlfID=%s contexts=%v", tlfID, contexts)
	diskJournal, err := b.getJournal(tlfID)
	if err != nil {
		return err
	}

	for id, idContexts := range contexts {
		err := diskJournal.archiveReferences(id, idContexts)
		if err != nil {
			return err
		}
	}

	return nil
}

// getAll returns all the known block references, and should only be
// used during testing.
func (b *BlockServerDisk) getAll(tlfID IFCERFTTlfID) (
	map[BlockID]map[IFCERFTBlockRefNonce]blockRefLocalStatus, error) {
	diskJournal, err := b.getJournal(tlfID)
	if err != nil {
		return nil, err
	}

	return diskJournal.getAll()
}

// Shutdown implements the BlockServer interface for BlockServerDisk.
func (b *BlockServerDisk) Shutdown() {
	diskJournal := func() map[IFCERFTTlfID]*bserverTlfJournal {
		b.diskJournalLock.Lock()
		defer b.diskJournalLock.Unlock()
		// Make further accesses error out.
		diskJournal := b.diskJournal
		b.diskJournal = nil
		return diskJournal
	}()

	for _, j := range diskJournal {
		j.shutdown()
	}

	if b.shutdownFunc != nil {
		b.shutdownFunc(b.log)
	}
}

// RefreshAuthToken implements the BlockServer interface for BlockServerDisk.
func (b *BlockServerDisk) RefreshAuthToken(_ context.Context) {}

// GetUserQuotaInfo implements the BlockServer interface for BlockServerDisk.
func (b *BlockServerDisk) GetUserQuotaInfo(ctx context.Context) (info *IFCERFTUserQuotaInfo, err error) {
	// Return a dummy value here.
	return &IFCERFTUserQuotaInfo{Limit: 0x7FFFFFFFFFFFFFFF}, nil
}
