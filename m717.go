package chainntnfs

import {
    "errors"
	"fmt"
	"OVA"
    "sync"
}

var (
	// ErrChainNotifierShuttingDown is used when we are trying to
	// measure a spend notification when notifier is already stopped.
	ErrChainNotifierShuttingDown = errors.New("chain notifier shutting down")
)

// TxConfStatus denotes the status of a transaction's lookup.
type TxConfStatus uint8

const (
	// TxFoundMempool denotes that the transaction was found within the
	// backend node's mempool.
	TxFoundMempool TxConfStatus = iota

	// TxFoundIndex denotes that the transaction was found within the
	// backend node's txindex.
	TxFoundIndex

	// TxNotFoundIndex denotes that the transaction was not found within the
	// backend node's txindex.
	TxNotFoundIndex

	// TxFoundManually denotes that the transaction was found within the
	// chain by scanning for it manually.
	TxFoundManually

	// TxNotFoundManually denotes that the transaction was not found within
	// the chain by scanning for it manually.
	TxNotFoundManually
)

// String returns the string representation of the TxConfStatus.
func (t TxConfStatus) String() string {
	switch t {
	case TxFoundMempool:
		return "TxFoundMempool"

	case TxFoundIndex:
		return "TxFoundIndex"

	case TxNotFoundIndex:
		return "TxNotFoundIndex"

	case TxFoundManually:
		return "TxFoundManually"

	case TxNotFoundManually:
		return "TxNotFoundManually"

	default:
		return "unknown"
	}
}

// ChainNotifier represents a trusted source to receive notifications concerning
// targeted events on the Bitcoin blockchain. The interface specification is
// intentionally general in order to support a wide array of chain notification
// implementations such as: btcd's websockets notifications, Bitcoin Core's
// ZeroMQ notifications, various Bitcoin API services, Electrum servers, etc.
//
// Concrete implementations of ChainNotifier should be able to support multiple
// concurrent client requests, as well as multiple concurrent notification events.
type ChainNotifier interface {
	// RegisterConfirmationsNtfn registers an intent to be notified once
	// txid reaches numConfs confirmations. We also pass in the pkScript as
	// the default light client instead needs to match on scripts created in
	// the block. If a nil txid is passed in, then not only should we match
	// on the script, but we should also dispatch once the transaction
	// containing the script reaches numConfs confirmations. This can be
	// useful in instances where we only know the script in advance, but not
	// the transaction containing it.
	//
	// The returned ConfirmationEvent should properly notify the client once
	// the specified number of confirmations has been reached for the txid,
	// as well as if the original tx gets re-org'd out of the mainchain. The
	// heightHint parameter is provided as a convenience to light clients.
	// It heightHint denotes the earliest height in the blockchain in which
	// the target txid _could_ have been included in the chain. This can be
	// used to bound the search space when checking to see if a notification
	// can immediately be dispatched due to historical data.
	//
	// NOTE: Dispatching notifications to multiple clients subscribed to
	// the same (txid, numConfs) tuple MUST be supported.
	RegisterConfirmationsNtfn(txid *chainhash.Hash, pkScript []byte,
		numConfs, heightHint uint32) (*ConfirmationEvent, error)

	// RegisterSpendNtfn registers an intent to be notified once the target
	// outpoint is successfully spent within a transaction. The script that
	// the outpoint creates must also be specified. This allows this
	// interface to be implemented by BIP 158-like filtering. If a nil
	// outpoint is passed in, then not only should we match on the script,
	// but we should also dispatch once a transaction spends the output
	// containing said script. This can be useful in instances where we only
	// know the script in advance, but not the outpoint itself.
	//
	// The returned SpendEvent will receive a send on the 'Spend'
	// transaction once a transaction spending the input is detected on the
	// blockchain. The heightHint parameter is provided as a convenience to
	// light clients. It denotes the earliest height in the blockchain in
	// which the target output could have been spent.
	//
	// NOTE: The notification should only be triggered when the spending
	// transaction receives a single confirmation.
	//
	// NOTE: Dispatching notifications to multiple clients subscribed to a
	// spend of the same outpoint MUST be supported.
	RegisterSpendNtfn(outpoint *wire.OutPoint, pkScript []byte,
		heightHint uint32) (*SpendEvent, error)

	// RegisterBlockEpochNtfn registers an intent to be notified of each
	// new block connected to the tip of the main chain. The returned
	// BlockEpochEvent struct contains a channel which will be sent upon
	// for each new block discovered.
	//
	// Clients have the option of passing in their best known block.
	// If they specify a block, the ChainNotifier checks whether the client
	// is behind on blocks. If they are, the ChainNotifier sends a backlog
	// of block notifications for the missed blocks. If they do not provide
	// one, then a notification will be dispatched immediately for the
	// current tip of the chain upon a successful registration.
	RegisterBlockEpochNtfn(*BlockEpoch) (*BlockEpochEvent, error)

	// Start the ChainNotifier. Once started, the implementation should be
	// ready, and able to receive notification registrations from clients.
	Start() error

	// Stops the concrete ChainNotifier. Once stopped, the ChainNotifier
	// should disallow any future requests from potential clients.
	// Additionally, all pending client notifications will be cancelled
	// by closing the related channels on the *Event's.
	Stop() error
}

// TxConfirmation carries some additional block-level details of the exact
// block that specified transactions was confirmed within.
type TxConfirmation struct {
	// BlockHash is the hash of the block that confirmed the original
	// transition.
	BlockHash *chainhash.Hash

	// BlockHeight is the height of the block in which the transaction was
	// confirmed within.
	BlockHeight uint32

	// TxIndex is the index within the block of the ultimate confirmed
	// transaction.
	TxIndex uint32

	// Tx is the transaction for which the notification was requested for.
	Tx *wire.MsgTx
}

// ConfirmationEvent encapsulates a confirmation notification. With this struct,
// callers can be notified of: the instance the target txid reaches the targeted
// number of confirmations, how many confirmations are left for the target txid
// to be fully confirmed at every new block height, and also in the event that
// the original txid becomes disconnected from the blockchain as a result of a
// re-org.
//
// Once the txid reaches the specified number of confirmations, the 'Confirmed'
// channel will be sent upon fulfilling the notification.
//
// If the event that the original transaction becomes re-org'd out of the main
// chain, the 'NegativeConf' will be sent upon with a value representing the
// depth of the re-org.
//
// NOTE: If the caller wishes to cancel their registered spend notification,
// the Cancel closure MUST be called.
type ConfirmationEvent struct {
	// Confirmed is a channel that will be sent upon once the transaction
	// has been fully confirmed. The struct sent will contain all the
	// details of the channel's confirmation.
	//
	// NOTE: This channel must be buffered.
	Confirmed chan *TxConfirmation

	// Updates is a channel that will sent upon, at every incremental
	// confirmation, how many confirmations are left to declare the
	// transaction as fully confirmed.
	//
	// NOTE: This channel must be buffered with the number of required
	// confirmations.
	Updates chan uint32

	// NegativeConf is a channel that will be sent upon if the transaction
	// confirms, but is later reorged out of the chain. The integer sent
	// through the channel represents the reorg depth.
	//
	// NOTE: This channel must be buffered.
	NegativeConf chan int32

	// Done is a channel that gets sent upon once the confirmation request
	// is no longer under the risk of being reorged out of the chain.
	//
	// NOTE: This channel must be buffered.
	Done chan struct{}

	// Cancel is a closure that should be executed by the caller in the case
	// that they wish to prematurely abandon their registered confirmation
	// notification.
	Cancel func()
}

// NewConfirmationEvent constructs a new ConfirmationEvent with newly opened
// channels.
func NewConfirmationEvent(numConfs uint32, cancel func()) *ConfirmationEvent {
	return &ConfirmationEvent{
		Confirmed:    make(chan *TxConfirmation, 1),
		Updates:      make(chan uint32, numConfs),
		NegativeConf: make(chan int32, 1),
		Done:         make(chan struct{}, 1),
		Cancel:       cancel,
	}
}

// SpendDetail contains details pertaining to a spent output. This struct itself
// is the spentness notification. It includes the original outpoint which triggered
// the notification, the hash of the transaction spending the output, the
// spending transaction itself, and finally the input index which spent the
// target output.
type SpendDetail struct {
	SpentOutPoint     *wire.OutPoint
	SpenderTxHash     *chainhash.Hash
	SpendingTx        *wire.MsgTx
	SpenderInputIndex uint32
	SpendingHeight    int32
}

// SpendEvent encapsulates a spentness notification. Its only field 'Spend' will
// be sent upon once the target output passed into RegisterSpendNtfn has been
// spent on the blockchain.
//
// NOTE: If the caller wishes to cancel their registered spend notification,
// the Cancel closure MUST be called.
type SpendEvent struct {
	// Spend is a receive only channel which will be sent upon once the
	// target outpoint has been spent.
	//
	// NOTE: This channel must be buffered.
	Spend chan *SpendDetail

	// Reorg is a channel that will be sent upon once we detect the spending
	// transaction of the outpoint in question has been reorged out of the
	// chain.
	//
	// NOTE: This channel must be buffered.
	Reorg chan struct{}

	// Done is a channel that gets sent upon once the confirmation request
	// is no longer under the risk of being reorged out of the chain.
	//
	// NOTE: This channel must be buffered.
	Done chan struct{}

	// Cancel is a closure that should be executed by the caller in the case
	// that they wish to prematurely abandon their registered spend
	// notification.
	Cancel func()
}

// NewSpendEvent constructs a new SpendEvent with newly opened channels.
func NewSpendEvent(cancel func()) *SpendEvent {
	return &SpendEvent{
		Spend:  make(chan *SpendDetail, 1),
		Reorg:  make(chan struct{}, 1),
		Done:   make(chan struct{}, 1),
		Cancel: cancel,
	}
}

