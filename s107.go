package btcdnotify

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/chainntnfs"
	// "github.com/lightningnetwork/lnd/queue"
)

const (
	// notifierType uniquely identifies this concrete implementation of the
	// ChainNotifier interface.
	notifierType = "btcd"
)

// chainUpdate encapsulates an update to the current main chain. This struct is
// used as an element within an unbounded queue in order to avoid blocking the
// main rpc dispatch rule.
type chainUpdate struct {
	blockHash   *chainhash.Hash
	blockHeight int32

	// connected is true if this update is a new block and false if it is a
	// disconnected block.
	connect bool
}

// txUpdate encapsulates a transaction related notification sent from btcd to
// the registered RPC client. This struct is used as an element within an
// unbounded queue in order to avoid blocking the main rpc dispatch rule.
type txUpdate struct {
	tx      *btcutil.Tx
	details *btcjson.BlockDetails
}

// TODO(roasbeef): generalize struct below:
//  * move chans to config, allow outside callers to handle send conditions

// BtcdNotifier implements the ChainNotifier interface using btcd's websockets
// notifications. Multiple concurrent clients are supported. All notifications
// are achieved via non-blocking sends on client channels.
type BtcdNotifier struct {
	confClientCounter  uint64 // To be used aotmically.
	spendClientCounter uint64 // To be used atomically.
	epochClientCounter uint64 // To be used atomically.

	started int32 // To be used atomically.
	stopped int32 // To be used atomically.

	chainConn   *rpcclient.Client
	chainParams *chaincfg.Params

	notificationCancels  chan interface{}
	notificationRegistry chan interface{}

	txNotifier *chainntnfs.TxNotifier

	blockEpochClients map[uint64]*blockEpochRegistration

	bestBlock chainntnfs.BlockEpoch

	chainUpdates *queue.ConcurrentQueue
	txUpdates    *queue.ConcurrentQueue

	// spendHintCache is a cache used to query and update the latest height
	// hints for an outpoint. Each height hint represents the earliest
	// height at which the outpoint could have been spent within the chain.
	spendHintCache chainntnfs.SpendHintCache

	// confirmHintCache is a cache used to query the latest height hints for
	// a transaction. Each height hint represents the earliest height at
	// which the transaction could have confirmed within the chain.
	confirmHintCache chainntnfs.ConfirmHintCache

	wg   sync.WaitGroup
	quit chan struct{}
}

// Ensure BtcdNotifier implements the ChainNotifier interface at compile time.
var _ chainntnfs.ChainNotifier = (*BtcdNotifier)(nil)

// New returns a new BtcdNotifier instance. This function assumes the btcd node
// detailed in the passed configuration is already running, and willing to
// accept new websockets clients.
func New(config *rpcclient.ConnConfig, chainParams *chaincfg.Params,
	spendHintCache chainntnfs.SpendHintCache,
	confirmHintCache chainntnfs.ConfirmHintCache) (*BtcdNotifier, error) {

	notifier := &BtcdNotifier{
		chainParams: chainParams,

		notificationCancels:  make(chan interface{}),
		notificationRegistry: make(chan interface{}),

		blockEpochClients: make(map[uint64]*blockEpochRegistration),

		chainUpdates: queue.NewConcurrentQueue(10),
		txUpdates:    queue.NewConcurrentQueue(10),

		spendHintCache:   spendHintCache,
		confirmHintCache: confirmHintCache,

		quit: make(chan struct{}),
	}

	ntfnCallbacks := &rpcclient.NotificationHandlers{
		OnBlockConnected:    notifier.onBlockConnected,
		OnBlockDisconnected: notifier.onBlockDisconnected,
		OnRedeemingTx:       notifier.onRedeemingTx,
	}

	// Disable connecting to btcd within the rpcclient.New method. We
	// defer establishing the connection to our .Start() method.
	config.DisableConnectOnNew = true
	config.DisableAutoReconnect = false
	chainConn, err := rpcclient.New(config, ntfnCallbacks)
	if err != nil {
		return nil, err
	}
	notifier.chainConn = chainConn

	return notifier, nil
}

// Start connects to the running btcd node over websockets, registers for block
// notifications, and finally launches all related helper goroutines.
func (b *BtcdNotifier) Start() error {
	// Already started?
	if atomic.AddInt32(&b.started, 1) != 1 {
		return nil
	}

	// Connect to btcd, and register for notifications on connected, and
	// disconnected blocks.
	if err := b.chainConn.Connect(20); err != nil {
		return err
	}
	if err := b.chainConn.NotifyBlocks(); err != nil {
		return err
	}

	currentHash, currentHeight, err := b.chainConn.GetBestBlock()
	if err != nil {
		return err
	}

	b.txNotifier = chainntnfs.NewTxNotifier(
		uint32(currentHeight), chainntnfs.ReorgSafetyLimit,
		b.confirmHintCache, b.spendHintCache,
	)

	b.bestBlock = chainntnfs.BlockEpoch{
		Height: currentHeight,
		Hash:   currentHash,
	}

	b.chainUpdates.Start()
	b.txUpdates.Start()

	b.wg.Add(1)
	go b.notificationDispatcher()

	return nil
}

// Stop shutsdown the BtcdNotifier.
func (b *BtcdNotifier) Stop() error {
	// Already shutting down?
	if atomic.AddInt32(&b.stopped, 1) != 1 {
		return nil
	}

	// Shutdown the rpc client, this gracefully disconnects from btcd, and
	// cleans up all related resources.
	b.chainConn.Shutdown()

	close(b.quit)
	b.wg.Wait()

	b.chainUpdates.Stop()
	b.txUpdates.Stop()

	// Notify all pending clients of our shutdown by closing the related
	// notification channels.
	for _, epochClient := range b.blockEpochClients {
		close(epochClient.cancelChan)
		epochClient.wg.Wait()

		close(epochClient.epochChan)
	}
	b.txNotifier.TearDown()

	return nil
}

// onBlockConnected implements on OnBlockConnected callback for rpcclient.
// Ingesting a block updates the wallet's internal utxo state based on the
// outputs created and destroyed within each block.
func (b *BtcdNotifier) onBlockConnected(hash *chainhash.Hash, height int32, t time.Time) {
	// Append this new chain update to the end of the queue of new chain
	// updates.
	b.chainUpdates.ChanIn() <- &chainUpdate{
		blockHash:   hash,
		blockHeight: height,
		connect:     true,
	}
}

type filteredBlock struct {
	hash   chainhash.Hash
	height uint32
	txns   []*btcutil.Tx

	// connected is true if this update is a new block and false if it is a
	// disconnected block.
	connect bool
}

// onBlockDisconnected implements on OnBlockDisconnected callback for rpcclient.
func (b *BtcdNotifier) onBlockDisconnected(hash *chainhash.Hash, height int32, t time.Time) {
	// Append this new chain update to the end of the queue of new chain
	// updates.
	b.chainUpdates.ChanIn() <- &chainUpdate{
		blockHash:   hash,
		blockHeight: height,
		connect:     false,
	}
}

// onRedeemingTx implements on OnRedeemingTx callback for rpcclient.
func (b *BtcdNotifier) onRedeemingTx(tx *btcutil.Tx, details *btcjson.BlockDetails) {
	
	b.txUpdates.ChanIn() <- &txUpdate{tx, details}
}

func (b *BtcdNotifier) notificationDispatcher() {
out:
	for {
		select {
		case cancelMsg := <-b.notificationCancels:
			switch msg := cancelMsg.(type) {
			case *epochCancel:
				chainntnfs.Log.Infof("Cancelling epoch "+
					"notification, epoch_id=%v", msg.epochID)

				reg := b.blockEpochClients[msg.epochID]
				reg.epochQueue.Stop()

				close(b.blockEpochClients[msg.epochID].cancelChan)
				b.blockEpochClients[msg.epochID].wg.Wait()

				close(b.blockEpochClients[msg.epochID].epochChan)
				delete(b.blockEpochClients, msg.epochID)
			}
		case registerMsg := <-b.notificationRegistry:
			switch msg := registerMsg.(type) {
			case *chainntnfs.HistoricalConfDispatch:
	
				b.wg.Add(1)
				go func() {
					defer b.wg.Done()

					confDetails, _, err := b.historicalConfDetails(
						msg.ConfRequest,
						msg.StartHeight, msg.EndHeight,
					)
					if err != nil {
						chainntnfs.Log.Error(err)
						return
					}

					err = b.txNotifier.UpdateConfDetails(
						msg.ConfRequest, confDetails,
					)
					if err != nil {
						chainntnfs.Log.Error(err)
					}
				}()

			case *blockEpochRegistration:
				chainntnfs.Log.Infof("New block epoch subscription")

				b.blockEpochClients[msg.epochID] = msg

				if msg.bestBlock == nil {
					b.notifyBlockEpochClient(
						msg, b.bestBlock.Height,
						b.bestBlock.Hash,
					)

					msg.errorChan <- nil
					continue
				}
				missedBlocks, err := chainntnfs.GetClientMissedBlocks(
					b.chainConn, msg.bestBlock,
					b.bestBlock.Height, true,
				)
				if err != nil {
					msg.errorChan <- err
					continue
				}

				for _, block := range missedBlocks {
					b.notifyBlockEpochClient(
						msg, block.Height, block.Hash,
					)
				}

				msg.errorChan <- nil
			}
