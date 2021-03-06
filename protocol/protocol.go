package protocol

import (
	"context"
	"sync"
	"time"

	"github.com/bytom/errors"
	"github.com/bytom/log"
	"github.com/bytom/protocol/bc"
	"github.com/bytom/protocol/bc/legacy"
	"github.com/bytom/protocol/state"
)

// maxCachedValidatedTxs is the max number of validated txs to cache.
const maxCachedValidatedTxs = 1000

var (
	// ErrTheDistantFuture is returned when waiting for a blockheight
	// too far in excess of the tip of the blockchain.
	ErrTheDistantFuture = errors.New("block height too far in future")
)

// Store provides storage for blockchain data: blocks and state tree
// snapshots.
//
// Note, this is different from a state snapshot. A state snapshot
// provides access to the state at a given point in time -- outputs
// and issuance memory. The Chain type uses Store to load state
// from storage and persist validated data.
type Store interface {
	Height() uint64
	GetBlock(uint64) (*legacy.Block, error)
	LatestSnapshot(context.Context) (*state.Snapshot, uint64, error)

	SaveBlock(*legacy.Block) error
	FinalizeBlock(context.Context, uint64) error
	SaveSnapshot(context.Context, uint64, *state.Snapshot) error
}

// Chain provides a complete, minimal blockchain database. It
// delegates the underlying storage to other objects, and uses
// validation logic from package validation to decide what
// objects can be safely stored.
type Chain struct {
	InitialBlockHash  bc.Hash
	MaxIssuanceWindow time.Duration // only used by generators

	state struct {
		cond     sync.Cond // protects height, block, snapshot
		height   uint64
		block    *legacy.Block
		snapshot *state.Snapshot
	}
	store Store

	lastQueuedSnapshot time.Time
	pendingSnapshots   chan pendingSnapshot

	txPool *TxPool
	assets_utxo struct{
		cond     sync.Cond
		assets_amount map[string]uint64
	}
}

type pendingSnapshot struct {
	height   uint64
	snapshot *state.Snapshot
}

// NewChain returns a new Chain using store as the underlying storage.
func NewChain(ctx context.Context, initialBlockHash bc.Hash, store Store, txPool *TxPool, heights <-chan uint64) (*Chain, error) {
	c := &Chain{
		InitialBlockHash: initialBlockHash,
		store:            store,
		pendingSnapshots: make(chan pendingSnapshot, 1),
		txPool:           txPool,
	}
	c.state.cond.L = new(sync.Mutex)

	c.assets_utxo.assets_amount = make(map[string]uint64,1024)  //prepared buffer 1024 key-values
	c.assets_utxo.cond.L = new(sync.Mutex)

	log.Printf(ctx, "bytom's Height:%v.", store.Height())
	c.state.height = store.Height()

	if c.state.height < 1 {
		c.state.snapshot = state.Empty()
	} else {
		c.state.block, _ = store.GetBlock(c.state.height)
		c.state.snapshot, _, _ = store.LatestSnapshot(ctx)
	}

	// Note that c.height.n may still be zero here.
	if heights != nil {
		go func() {
			for h := range heights {
				c.setHeight(h)
			}
		}()
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ps := <-c.pendingSnapshots:
				err := store.SaveSnapshot(ctx, ps.height, ps.snapshot)
				if err != nil {
					log.Error(ctx, err, "at", "saving snapshot")
				}
			}
		}
	}()

	return c, nil
}

func (c *Chain) GetStore() *Store {
	return &(c.store)
}

// Height returns the current height of the blockchain.
func (c *Chain) Height() uint64 {
	c.state.cond.L.Lock()
	defer c.state.cond.L.Unlock()
	return c.state.height
}

// TimestampMS returns the latest known block timestamp.
func (c *Chain) TimestampMS() uint64 {
	c.state.cond.L.Lock()
	defer c.state.cond.L.Unlock()
	if c.state.block == nil {
		return 0
	}
	return c.state.block.TimestampMS
}

// State returns the most recent state available. It will not be current
// unless the current process is the leader. Callers should examine the
// returned block header's height if they need to verify the current state.
func (c *Chain) State() (*legacy.Block, *state.Snapshot) {
	c.state.cond.L.Lock()
	defer c.state.cond.L.Unlock()
	return c.state.block, c.state.snapshot
}

func (c *Chain) setState(b *legacy.Block, s *state.Snapshot) {
	c.state.cond.L.Lock()
	defer c.state.cond.L.Unlock()
	c.state.block = b
	c.state.snapshot = s
	if b != nil && b.Height > c.state.height {
		c.state.height = b.Height
		c.state.cond.Broadcast()
	}
}

// BlockSoonWaiter returns a channel that
// waits for the block at the given height,
// but it is an error to wait for a block far in the future.
// WaitForBlockSoon will timeout if the context times out.
// To wait unconditionally, the caller should use WaitForBlock.
func (c *Chain) BlockSoonWaiter(ctx context.Context, height uint64) <-chan error {
	ch := make(chan error, 1)

	go func() {
		const slop = 3
		if height > c.Height()+slop {
			ch <- ErrTheDistantFuture
			return
		}

		select {
		case <-c.BlockWaiter(height):
			ch <- nil
		case <-ctx.Done():
			ch <- ctx.Err()
		}
	}()

	return ch
}

// BlockWaiter returns a channel that
// waits for the block at the given height.
func (c *Chain) BlockWaiter(height uint64) <-chan struct{} {
	ch := make(chan struct{}, 1)
	go func() {
		c.state.cond.L.Lock()
		defer c.state.cond.L.Unlock()
		for c.state.height < height {
			c.state.cond.Wait()
		}
		ch <- struct{}{}
	}()

	return ch
}
