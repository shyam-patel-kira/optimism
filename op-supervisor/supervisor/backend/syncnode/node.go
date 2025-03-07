package syncnode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/ethereum-optimism/optimism/op-service/rpc"
	gethrpc "github.com/ethereum/go-ethereum/rpc"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/log"

	"github.com/ethereum-optimism/optimism/op-node/rollup/event"
	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum-optimism/optimism/op-supervisor/supervisor/backend/superevents"
	"github.com/ethereum-optimism/optimism/op-supervisor/supervisor/types"
	gethevent "github.com/ethereum/go-ethereum/event"
)

type backend interface {
	LocalSafe(ctx context.Context, chainID eth.ChainID) (pair types.DerivedIDPair, err error)
	LocalUnsafe(ctx context.Context, chainID eth.ChainID) (eth.BlockID, error)
	CrossSafe(ctx context.Context, chainID eth.ChainID) (pair types.DerivedIDPair, err error)
	SafeDerivedAt(ctx context.Context, chainID eth.ChainID, derivedFrom eth.BlockID) (derived eth.BlockID, err error)
	Finalized(ctx context.Context, chainID eth.ChainID) (eth.BlockID, error)
	L1BlockRefByNumber(ctx context.Context, number uint64) (eth.L1BlockRef, error)
}

const (
	internalTimeout            = time.Second * 30
	nodeTimeout                = time.Second * 10
	maxWalkBackAttempts        = 300
	blockNotFoundRPCErrCode    = -39001
	conflictingBlockRPCErrCode = -39002
)

type ManagedNode struct {
	log     log.Logger
	Node    SyncControl
	chainID eth.ChainID

	backend backend

	// When the node has an update for us
	// Nil when node events are pulled synchronously.
	nodeEvents chan *types.ManagedEvent

	subscriptions []gethevent.Subscription

	emitter event.Emitter

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

var _ event.AttachEmitter = (*ManagedNode)(nil)
var _ event.Deriver = (*ManagedNode)(nil)

func NewManagedNode(log log.Logger, id eth.ChainID, node SyncControl, backend backend, noSubscribe bool) *ManagedNode {
	ctx, cancel := context.WithCancel(context.Background())
	m := &ManagedNode{
		log:     log.New("chain", id),
		backend: backend,
		Node:    node,
		chainID: id,
		ctx:     ctx,
		cancel:  cancel,
	}
	if !noSubscribe {
		m.SubscribeToNodeEvents()
	}
	m.WatchSubscriptionErrors()
	return m
}

func (m *ManagedNode) AttachEmitter(em event.Emitter) {
	m.emitter = em
}

func (m *ManagedNode) OnEvent(ev event.Event) bool {
	switch x := ev.(type) {
	case superevents.InvalidateLocalSafeEvent:
		if x.ChainID != m.chainID {
			return false
		}
		m.onInvalidateLocalSafe(x.Candidate)
	case superevents.CrossUnsafeUpdateEvent:
		if x.ChainID != m.chainID {
			return false
		}
		m.onCrossUnsafeUpdate(x.NewCrossUnsafe)
	case superevents.CrossSafeUpdateEvent:
		if x.ChainID != m.chainID {
			return false
		}
		m.onCrossSafeUpdate(x.NewCrossSafe)
	case superevents.FinalizedL2UpdateEvent:
		if x.ChainID != m.chainID {
			return false
		}
		m.onFinalizedL2(x.FinalizedL2)
	case superevents.LocalSafeOutOfSyncEvent:
		if x.ChainID != m.chainID {
			return false
		}
		m.resetSignal(x.Err, x.L1Ref)
	case superevents.ChainRewoundEvent:
		if x.ChainID != m.chainID {
			return false
		}
		m.sendReset()
	default:
		return false
	}
	return true
}

func (m *ManagedNode) SubscribeToNodeEvents() {
	m.nodeEvents = make(chan *types.ManagedEvent, 10)

	// Resubscribe, since the RPC subscription might fail intermittently.
	// And fall back to polling, if RPC subscriptions are not supported.
	m.subscriptions = append(m.subscriptions, gethevent.ResubscribeErr(time.Second*10,
		func(ctx context.Context, prevErr error) (gethevent.Subscription, error) {
			if prevErr != nil {
				// This is the RPC runtime error, not the setup error we have logging for below.
				m.log.Error("RPC subscription failed, restarting now", "err", prevErr)
			}
			sub, err := m.Node.SubscribeEvents(ctx, m.nodeEvents)
			if err != nil {
				if errors.Is(err, gethrpc.ErrNotificationsUnsupported) {
					m.log.Warn("No RPC notification support detected, falling back to polling")
					// fallback to polling if subscriptions are not supported.
					sub, err := rpc.StreamFallback[types.ManagedEvent](
						m.Node.PullEvent, time.Millisecond*100, m.nodeEvents)
					if err != nil {
						m.log.Error("Failed to start RPC stream fallback", "err", err)
						return nil, err
					}
					return sub, err
				}
				return nil, err
			}
			return sub, nil
		}))
}

func (m *ManagedNode) WatchSubscriptionErrors() {
	watchSub := func(sub ethereum.Subscription) {
		defer m.wg.Done()
		select {
		case err := <-sub.Err():
			m.log.Error("Subscription error", "err", err)
		case <-m.ctx.Done():
			// we're closing, stop watching the subscription
		}
	}
	for _, sub := range m.subscriptions {
		m.wg.Add(1)
		go watchSub(sub)
	}
}

func (m *ManagedNode) Start() {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()

		for {
			select {
			case <-m.ctx.Done():
				m.log.Info("Exiting node syncing")
				return
			case ev := <-m.nodeEvents: // nil, indefinitely blocking, if no node-events subscriber is set up.
				m.onNodeEvent(ev)
			}
		}
	}()
}

// PullEvents pulls all events, until there are none left,
// the ctx is canceled, or an error upon event-pulling occurs.
func (m *ManagedNode) PullEvents(ctx context.Context) (pulledAny bool, err error) {
	for {
		ev, err := m.Node.PullEvent(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				// no events left
				return pulledAny, nil
			}
			return pulledAny, err
		}
		pulledAny = true
		m.onNodeEvent(ev)
	}
}

func (m *ManagedNode) onNodeEvent(ev *types.ManagedEvent) {
	if ev == nil {
		m.log.Warn("Received nil event")
		return
	}
	if ev.Reset != nil {
		m.onResetEvent(*ev.Reset)
	}
	if ev.UnsafeBlock != nil {
		m.onUnsafeBlock(*ev.UnsafeBlock)
	}
	if ev.DerivationUpdate != nil {
		m.onDerivationUpdate(*ev.DerivationUpdate)
	}
	if ev.ExhaustL1 != nil {
		m.onExhaustL1Event(*ev.ExhaustL1)
	}
	if ev.ReplaceBlock != nil {
		m.onReplaceBlock(*ev.ReplaceBlock)
	}
}

func (m *ManagedNode) onResetEvent(errStr string) {
	m.log.Warn("Node sent us a reset error", "err", errStr)
	if strings.Contains(errStr, "cannot continue derivation until Engine has been reset") {
		// TODO
		return
	}
	// Try and restore the safe head of the op-supervisor.
	// The node will abort the reset until we find a block that is known.
	m.resetSignal(types.ErrFuture, eth.L1BlockRef{})
}

func (m *ManagedNode) onCrossUnsafeUpdate(seal types.BlockSeal) {
	m.log.Debug("updating cross unsafe", "crossUnsafe", seal)
	ctx, cancel := context.WithTimeout(m.ctx, nodeTimeout)
	defer cancel()
	id := seal.ID()
	err := m.Node.UpdateCrossUnsafe(ctx, id)
	if err != nil {
		m.log.Warn("Node failed cross-unsafe updating", "err", err)
		return
	}
}

func (m *ManagedNode) onCrossSafeUpdate(pair types.DerivedBlockSealPair) {
	m.log.Debug("updating cross safe", "derived", pair.Derived, "derivedFrom", pair.DerivedFrom)
	ctx, cancel := context.WithTimeout(m.ctx, nodeTimeout)
	defer cancel()
	pairIDs := pair.IDs()
	err := m.Node.UpdateCrossSafe(ctx, pairIDs.Derived, pairIDs.DerivedFrom)
	if err != nil {
		m.log.Warn("Node failed cross-safe updating", "err", err)
		return
	}
}

func (m *ManagedNode) onFinalizedL2(seal types.BlockSeal) {
	m.log.Info("updating finalized L2", "finalized", seal)
	ctx, cancel := context.WithTimeout(m.ctx, nodeTimeout)
	defer cancel()
	id := seal.ID()
	err := m.Node.UpdateFinalized(ctx, id)
	if err != nil {
		m.log.Warn("Node failed finality updating", "err", err)
		return
	}
}

func (m *ManagedNode) onUnsafeBlock(unsafeRef eth.BlockRef) {
	m.log.Info("Node has new unsafe block", "unsafeBlock", unsafeRef)
	m.emitter.Emit(superevents.LocalUnsafeReceivedEvent{
		ChainID:        m.chainID,
		NewLocalUnsafe: unsafeRef,
	})
}

func (m *ManagedNode) onDerivationUpdate(pair types.DerivedBlockRefPair) {
	m.log.Info("Node derived new block", "derived", pair.Derived,
		"derivedParent", pair.Derived.ParentID(), "derivedFrom", pair.DerivedFrom)
	m.emitter.Emit(superevents.LocalDerivedEvent{
		ChainID: m.chainID,
		Derived: pair,
	})
	// TODO: keep synchronous local-safe DB update feedback?
	// We'll still need more async ways of doing this for reorg handling.

	// ctx, cancel := context.WithTimeout(m.ctx, internalTimeout)
	// defer cancel()
	// if err := m.backend.UpdateLocalSafe(ctx, m.chainID, pair.DerivedFrom, pair.Derived); err != nil {
	//	m.log.Warn("Backend failed to process local-safe update",
	//		"derived", pair.Derived, "derivedFrom", pair.DerivedFrom, "err", err)
	//	m.resetSignal(err, pair.DerivedFrom)
	// }
}

func (m *ManagedNode) resetSignal(errSignal error, l1Ref eth.BlockRef) {
	// if conflict error -> send reset to drop
	// if future error -> send reset to rewind
	// if out of order -> warn, just old data
	// TODO(#13971): When there are errors getting these blocks, we shouldn't always exit early.
	ctx, cancel := context.WithTimeout(m.ctx, internalTimeout)
	defer cancel()
	u, err := m.backend.LocalUnsafe(ctx, m.chainID)
	if err != nil {
		m.log.Warn("Failed to retrieve local-unsafe", "err", err)
		return
	}
	f, err := m.backend.Finalized(ctx, m.chainID)
	if err != nil {
		m.log.Warn("Failed to retrieve finalized", "err", err)
		return
	}

	// TODO: Lots of changes needed here
	// Reset walkback exists via resolveConflict, so this error-type handling should be reconsidered.
	switch {
	case errors.Is(errSignal, types.ErrConflict):
		if err := m.resolveConflict(ctx, l1Ref, u, f); err != nil {
			m.log.Warn("Failed to resolve conflict", "unsafe", u, "finalized", f)
			return
		}
	case errors.Is(errSignal, types.ErrFuture):
		s, err := m.backend.LocalSafe(ctx, m.chainID)
		if err != nil {
			m.log.Warn("Failed to retrieve local-safe", "err", err)
		}
		m.log.Debug("Node detected future block, resetting", "unsafe", u, "safe", s, "finalized", f)
		err = m.Node.Reset(ctx, u, s.Derived, f)
		if err != nil {
			m.log.Warn("Node failed to reset", "err", err)
		}
	case errors.Is(errSignal, types.ErrOutOfOrder):
		s, err := m.backend.LocalSafe(ctx, m.chainID)
		if err != nil {
			m.log.Warn("Failed to retrieve local-safe", "err", err)
			return
		}
		m.log.Warn("Node detected out of order block", "unsafe", u, "finalized", f)
		err = m.Node.Reset(ctx, u, s.Derived, f)
		if err != nil {
			m.log.Warn("Node failed to reset", "err", err)
		}
	}
}

func (m *ManagedNode) sendReset() {
	ctx, cancel := context.WithTimeout(m.ctx, internalTimeout)
	defer cancel()

	u, err := m.backend.LocalUnsafe(ctx, m.chainID)
	if err != nil {
		m.log.Warn("Failed to retrieve local-unsafe", "err", err)
		return
	}
	s, err := m.backend.CrossSafe(ctx, m.chainID)
	if err != nil {
		m.log.Warn("Failed to retrieve cross-safe", "err", err)
		return
	}
	f, err := m.backend.Finalized(ctx, m.chainID)
	if err != nil {
		if errors.Is(err, types.ErrFuture) {
			f = eth.BlockID{Number: 0}
		} else {
			m.log.Warn("Failed to retrieve finalized", "err", err)
			return
		}
	}

	if err := m.Node.Reset(ctx, u, s.Derived, f); err != nil {
		m.log.Warn("Node failed to reset", "err", err)
		return
	}
}

// resolveConflict attempts to reset the node to a valid state when a conflict is detected.
// It first tries using the latest safe block, and if that fails, walks back block by block
// until it finds a common ancestor or reaches the finalized block.
func (m *ManagedNode) resolveConflict(ctx context.Context, l1Ref eth.BlockRef, u eth.BlockID, f eth.BlockID) error {
	// First try to reset to the last known safe block
	s, err := m.backend.SafeDerivedAt(ctx, m.chainID, l1Ref.ID())
	if err != nil {
		return fmt.Errorf("failed to retrieve safe block for %v: %w", l1Ref.ID(), err)
	}

	// Helper to attempt a reset and classify the error
	tryReset := func(safe eth.BlockID) (resolved bool, needsWalkback bool, err error) {
		m.log.Debug("Attempting reset", "unsafe", u, "safe", safe, "finalized", f)
		if err := m.Node.Reset(ctx, u, safe, f); err == nil {
			return true, false, nil
		} else {
			var rpcErr *gethrpc.JsonError
			if errors.As(err, &rpcErr) && (rpcErr.Code == blockNotFoundRPCErrCode || rpcErr.Code == conflictingBlockRPCErrCode) {
				return false, true, err
			}
			return false, false, err
		}
	}

	// Try initial reset
	resolved, needsWalkback, err := tryReset(s)
	if resolved {
		return nil
	}
	if !needsWalkback {
		return fmt.Errorf("error during reset: %w", err)
	}

	// Walk back one block at a time looking for a common ancestor
	currentBlock := s.Number
	for i := 0; i < maxWalkBackAttempts; i++ {
		currentBlock--
		if currentBlock <= f.Number {
			return fmt.Errorf("reached finalized block %d without finding common ancestor", f.Number)
		}

		safe, err := m.backend.SafeDerivedAt(ctx, m.chainID, eth.BlockID{Number: currentBlock})
		if err != nil {
			return fmt.Errorf("failed to retrieve safe block %d: %w", currentBlock, err)
		}

		resolved, _, err := tryReset(safe)
		if resolved {
			return nil
		}
		// Continue walking back on walkable errors, otherwise return the error
		var rpcErr *gethrpc.JsonError
		if !errors.As(err, &rpcErr) || (rpcErr.Code != blockNotFoundRPCErrCode && rpcErr.Code != conflictingBlockRPCErrCode) {
			return fmt.Errorf("error during reset at block %d: %w", currentBlock, err)
		}
	}
	return fmt.Errorf("exceeded maximum walk-back attempts (%d)", maxWalkBackAttempts)
}

func (m *ManagedNode) onExhaustL1Event(completed types.DerivedBlockRefPair) {
	m.log.Info("Node completed syncing", "l2", completed.Derived, "l1", completed.DerivedFrom)

	internalCtx, cancel := context.WithTimeout(m.ctx, internalTimeout)
	defer cancel()
	nextL1, err := m.backend.L1BlockRefByNumber(internalCtx, completed.DerivedFrom.Number+1)
	if err != nil {
		if errors.Is(err, ethereum.NotFound) {
			m.log.Debug("Next L1 block is not yet available", "l1Block", completed.DerivedFrom, "err", err)
			return
		}
		m.log.Error("Failed to retrieve next L1 block for node", "l1Block", completed.DerivedFrom, "err", err)
		return
	}

	nodeCtx, cancel := context.WithTimeout(m.ctx, nodeTimeout)
	defer cancel()
	if err := m.Node.ProvideL1(nodeCtx, nextL1); err != nil {
		m.log.Warn("Failed to provide next L1 block to node", "err", err)
		// We will reset the node if we receive a reset-event from it,
		// which is fired if the provided L1 block was received successfully,
		// but does not fit on the derivation state.
		return
	}
}

// onInvalidateLocalSafe listens for when a local-safe block is found to be invalid in the cross-safe context
// and needs to be replaced with a deposit only block.
func (m *ManagedNode) onInvalidateLocalSafe(invalidated types.DerivedBlockRefPair) {
	m.log.Warn("Instructing node to replace invalidated local-safe block",
		"invalidated", invalidated.Derived, "scope", invalidated.DerivedFrom)

	ctx, cancel := context.WithTimeout(m.ctx, nodeTimeout)
	defer cancel()
	// Send instruction to the node to invalidate the block, and build a replacement block.
	if err := m.Node.InvalidateBlock(ctx, types.BlockSealFromRef(invalidated.Derived)); err != nil {
		m.log.Warn("Node is unable to invalidate block",
			"invalidated", invalidated.Derived, "scope", invalidated.DerivedFrom, "err", err)
	}
}

func (m *ManagedNode) onReplaceBlock(replacement types.BlockReplacement) {
	m.log.Info("Node provided replacement block",
		"ref", replacement.Replacement, "invalidated", replacement.Invalidated)
	m.emitter.Emit(superevents.ReplaceBlockEvent{
		ChainID:     m.chainID,
		Replacement: replacement,
	})
}

func (m *ManagedNode) Close() error {
	m.cancel()
	m.wg.Wait() // wait for work to complete

	// Now close all subscriptions, since we don't use them anymore.
	for _, sub := range m.subscriptions {
		sub.Unsubscribe()
	}
	return nil
}
