// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package ethapi implements the general Ethereum API functions.
package mev

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/gopool"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/eth/tracers/logger"
	"github.com/ethereum/go-ethereum/eth/tracers/native"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/holiman/uint256"
)

const (
	// defaultTraceTimeout is the amount of time a single transaction can execute
	// by default before being forcefully aborted.
	defaultTraceTimeout = 1 * time.Second

	// defaultTraceReexec is the number of blocks the tracer is willing to go back
	// and reexecute to produce missing historical state necessary to run a specific
	// trace.
	defaultTraceReexec = uint64(128)
)

type PrevTxInfo struct {
	Hash     common.Hash  `json:"hash"`
	GasPrice *hexutil.Big `json:"gasPrice"`
}

type FilteredSimTx struct {
	SimTxInfo    *SimResult           `json:"simTxInfo"`
	FilterResult []*native.FilterData `json:"filterResult,omitempty"`
}

type SimResult struct {
	tracer       tracers.Tracer `json:"-"`
	Hash         common.Hash    `json:"hash"`
	PrevTxs      []*PrevTxInfo  `json:"prevTxs"`
	PrevGasIsDec bool           `json:"prevGasIsDecrese"`
	EffGasPrice  *hexutil.Big   `json:"effGasPrice"`
	GasTipCap    *hexutil.Big   `json:"gasTipCap"`
	GasPrice     *hexutil.Big   `json:"gasPrice"`
	TxType       byte           `json:"txType"`
	GasUsed      uint64         `json:"gasUsed"`
	TracerResult interface{}    `json:"tracerInfo"`
	BlockNumber  uint64         `json:"blockNumber"`
	BlockTime    uint64         `json:"blockTime"`
	BlockHash    common.Hash    `json:"blockHash"`
}

type SimResults []*SimResult

type MevAPI struct {
	b MevBackend

	accNonceMark map[common.Address]uint64
	mevTxsFeed   event.Feed
	mevMu        sync.RWMutex // for searcher
	stopCh       chan struct{}
}

func NewPrivateMevAPI(b MevBackend) *MevAPI {
	api := MevAPI{
		b:            b,
		stopCh:       make(chan struct{}, 0),
		accNonceMark: make(map[common.Address]uint64),
	}
	log.Info("[MEV] NewPrivateMevAPI")

	go api.searcherLoop()

	return &api
}

// =============================================================================================================
//
//	Simulation api
//
// =============================================================================================================
type chainContext struct {
	api *MevAPI
	ctx context.Context
}

func (context *chainContext) Engine() consensus.Engine {
	return context.api.b.Engine()
}

func (context *chainContext) GetHeader(hash common.Hash, number uint64) *types.Header {
	header, err := context.api.b.HeaderByNumber(context.ctx, rpc.BlockNumber(number))
	if err != nil {
		return nil
	}
	if header.Hash() == hash {
		return header
	}
	header, err = context.api.b.HeaderByHash(context.ctx, hash)
	if err != nil {
		return nil
	}
	return header
}

// chainContext construts the context reader which is used by the evm for reading
// the necessary chain context.
func (api *MevAPI) chainContext(ctx context.Context) core.ChainContext {
	return &chainContext{api: api, ctx: ctx}
}

func (api *MevAPI) TraceCallMany(ctx context.Context, argss []ethapi.TransactionArgs, blockNrOrHash rpc.BlockNumberOrHash, config *tracers.TraceCallConfig) ([]interface{}, error) {

	var (
		err   error
		block *types.Block
		ress  []interface{}
	)
	if hash, ok := blockNrOrHash.Hash(); ok {
		block, err = api.b.BlockByHash(ctx, hash)
	} else if number, ok := blockNrOrHash.Number(); ok {
		block, err = api.b.BlockByNumber(ctx, number)
	} else {
		return nil, errors.New("invalid arguments; neither block nor hash specified")
	}
	if err != nil {
		return nil, err
	}
	// try to recompute the state
	reexec := defaultTraceReexec
	if config != nil && config.Reexec != nil {
		reexec = *config.Reexec
	}
	statedb, release, err := api.b.StateAtBlock(ctx, block, reexec, nil, true, false)
	if err != nil {
		return nil, err
	}
	defer release()

	// Apply the customized state rules if required.
	if config != nil {
		if err := config.StateOverrides.Apply(statedb); err != nil {
			return nil, err
		}
	}

	for _, args := range argss {
		msg, err := args.ToMessage(api.b.RPCGasCap(), block.BaseFee())
		if err != nil {
			return nil, err
		}
		vmctx := core.NewEVMBlockContext(block.Header(), api.chainContext(ctx), nil)
		if config.BlockOverrides != nil {
			config.BlockOverrides.Apply(&vmctx)
		}
		var traceConfig *tracers.TraceConfig
		if config != nil {
			traceConfig = &config.TraceConfig
		}

		var tracer tracers.Tracer
		var er *core.ExecutionResult
		tracer, err, er = api.mevTraceTx(ctx, msg, new(tracers.Context), vmctx, statedb, traceConfig)

		if err != nil {
			log.Debug("[MEV] call mevTraceTx failed", err)
			break
		}
		var jstr interface{}
		jstr, err = tracer.GetResult()
		if err != nil {
			log.Info("[MEV] tracer.GetResult() failed", err)
		}
		ress = append(ress, jstr)
		if er.Err != nil {
			log.Debug("[MEV] mevTraceTx exec failed", er.Err)
			break
		}
	}
	// Execute the trace
	return ress, err
}

// traceTx configures a new tracer according to the provided configuration, and
// executes the given message in the provided environment. The return value will
// be tracer dependent.
func (api *MevAPI) mevTraceTx(ctx context.Context, message *core.Message, txctx *tracers.Context, vmctx vm.BlockContext, statedb *state.StateDB, config *tracers.TraceConfig) (tracers.Tracer, error, *core.ExecutionResult) {
	var (
		tracer    tracers.Tracer
		err       error
		txContext = core.NewEVMTxContext(message)
	)
	if config == nil {
		config = &tracers.TraceConfig{}
	}
	// Default tracer is the struct logger
	tracer = logger.NewStructLogger(config.Config)
	if config.Tracer != nil {
		tracer, err = tracers.DefaultDirectory.New(*config.Tracer, txctx, config.TracerConfig)
		if err != nil {
			return nil, err, nil
		}
	}
	snap := statedb.Snapshot()
	vmenv := vm.NewEVM(vmctx, txContext, statedb, api.b.ChainConfig(), vm.Config{Tracer: tracer, NoBaseFee: true})

	var intrinsicGas uint64 = 0
	// Run the transaction with tracing enabled.
	if posa, ok := api.b.Engine().(consensus.PoSA); ok && message.From == vmctx.Coinbase &&
		posa.IsSystemContract(message.To) && message.GasPrice.Cmp(big.NewInt(0)) == 0 {
		balance := statedb.GetBalance(consensus.SystemAddress)
		if balance.Cmp(common.U2560) > 0 {
			statedb.SetBalance(consensus.SystemAddress, uint256.NewInt(0))
			statedb.AddBalance(vmctx.Coinbase, balance)
		}
		intrinsicGas, _ = core.IntrinsicGas(message.Data, message.AccessList, false, true, true, false)
	}

	// Call Prepare to clear out the statedb access list
	statedb.SetTxContext(txctx.TxHash, txctx.TxIndex)
	var er *core.ExecutionResult
	er, err = core.ApplyMessage(vmenv, message, new(core.GasPool).AddGas(message.GasLimit))
	if err != nil {
		statedb.RevertToSnapshot(snap)
		return nil, fmt.Errorf("tracing failed: %w", err), nil
	}
	tracer.CaptureSystemTxEnd(intrinsicGas)

	return tracer, err, er
}

// callbundle

type BundleTransaction struct {
	Tx     *ethapi.TransactionArgs `json:"tx"`
	TxHash *common.Hash            `json:"txHash"`
}

type BundleArgs struct {
	Txs            []BundleTransaction      `json:"txs"`
	IgnoreRevert   bool                     `json:"ignoreRevert"`
	Tracer         *tracers.TraceCallConfig `json:"tracer"`
	BlockOverrides []ethapi.BlockOverrides  `json:"blockOverrides"`
}

func (api *MevAPI) TraceCallBundle(ctx context.Context, bundle BundleArgs) (map[string]interface{}, error) {

	var (
		err                error
		tracer             tracers.Tracer
		receipt            *types.Receipt
		statedb            *state.StateDB
		header             *types.Header
		traceConfig        *tracers.TraceConfig
		bundleTracerConfig *tracers.TraceConfig
		results            []map[string]interface{}
		totalGasUsed       uint64
	)

	callTracer := "callTracer"
	if bundle.Tracer != nil {
		bundleTracerConfig = &bundle.Tracer.TraceConfig
	} else {
		bundleTracerConfig = &tracers.TraceConfig{}
		bundleTracerConfig.Tracer = &callTracer
	}

	chainConfig := api.b.ChainConfig()
	gp := new(core.GasPool).AddGas(math.MaxUint64)

	block, err := api.b.BlockByNumber(ctx, rpc.LatestBlockNumber)
	if err != nil {
		return nil, err
	}
	statedb, release, err := api.b.StateAtBlock(ctx, block, 128, nil, true, false)
	if err != nil {
		return nil, err
	}
	defer release()
	header = block.Header()

	// statedb, header, err = api.b.StateAndHeaderByNumber(ctx, rpc.LatestBlockNumber)
	signer := types.MakeSigner(api.b.ChainConfig(), header.Number, header.Time)
	if err != nil {
		return nil, err
	}

	for txIdx, txOrHash := range bundle.Txs {
		var tx *types.Transaction
		var msg *core.Message

		if txOrHash.Tx != nil {
			tx = txOrHash.Tx.ToTransaction()
			msg = TransactionToMessageNoSignerCheck(tx, txOrHash.Tx.From)

		} else if txOrHash.TxHash != nil {
			tx := api.b.TxPool().Get(*txOrHash.TxHash)
			if tx == nil {
				return nil, fmt.Errorf("Not find pending hash: %s", txOrHash.TxHash.String())
			}

			msg, err = core.TransactionToMessage(tx, signer, header.BaseFee)
			if err != nil {
				return nil, fmt.Errorf("Pending Tx to message error: %s, %s", txOrHash.TxHash.String(), err.Error())
			}
		} else {
			return nil, fmt.Errorf("Tx or Txhash is nil index %d", txIdx)
		}

		traceConfig = bundleTracerConfig
		tracer = logger.NewStructLogger(traceConfig.Config)
		if traceConfig.Tracer != nil {
			tracer, err = tracers.DefaultDirectory.New(*traceConfig.Tracer, new(tracers.Context), traceConfig.TracerConfig)
			if err != nil {
				return nil, err
			}
		}
		statedb.SetTxContext(tx.Hash(), txIdx)
		var blockOverride = bundle.BlockOverrides[txIdx]
		receipt, err = commitTransaction(chainConfig, api.b.Chain(), statedb, header, tx, msg, gp, vm.Config{Tracer: tracer, NoBaseFee: false}, false, &blockOverride)

		tracerResult, _ := tracer.GetResult()
		result := map[string]interface{}{
			"hash":   tx.Hash().String(),
			"tracer": tracerResult,
			"error":  nil,
			"status": 0,
		}

		if err != nil {
			fmt.Println("[MEV] CallBundle Sim failed", "err:", err, "hash:", tx.Hash().Hex(), "block:", header.Number)
			result["error"] = err.Error()
		} else {
			totalGasUsed += receipt.GasUsed
			result["gasUsed"] = receipt.GasUsed
			result["status"] = receipt.Status
		}

		results = append(results, result)

		if !bundle.IgnoreRevert && (err != nil || receipt.Status == 0) {
			break
		}
	}

	ret := map[string]interface{}{
		"results":          results,
		"totalGasUsed":     totalGasUsed,
		"stateBlockNumber": header.Number.Int64(),
	}

	return ret, nil
}

// ============================================================
//
//	eth_callMany
//
// ============================================================
type revertError struct {
	error
	reason string // revert reason hex encoded
}

func newRevertError(result *core.ExecutionResult) *revertError {
	reason, errUnpack := abi.UnpackRevert(result.Revert())
	err := errors.New("execution reverted")
	if errUnpack == nil {
		err = fmt.Errorf("execution reverted: %v", reason)
	}
	return &revertError{
		error:  err,
		reason: hexutil.Encode(result.Revert()),
	}
}

func (s *MevAPI) CallMany(ctx context.Context, txsArgs []ethapi.TransactionArgs, blockNrOrHash rpc.BlockNumberOrHash) ([]interface{}, error) {
	resutls := make([]interface{}, 0)
	globalGasCap := s.b.RPCGasCap()
	state, header, err := s.b.StateAndHeaderByNumberOrHash(ctx, blockNrOrHash)
	if state == nil || err != nil {
		return nil, err
	}
	for _, args := range txsArgs {
		// Get a new instance of the EVM.
		msg, err := args.ToMessage(globalGasCap, header.BaseFee)
		if err != nil {
			return nil, err
		}
		blockCtx := core.NewEVMBlockContext(header, ethapi.NewChainContext(ctx, s.b), nil)
		evm := s.b.GetEVM(ctx, msg, state, header, &vm.Config{NoBaseFee: true}, &blockCtx)

		// Execute the message.
		gp := new(core.GasPool).AddGas(math.MaxUint64)
		result, err := core.ApplyMessage(evm, msg, gp)
		if err != nil {
			resutls = append(resutls, err.Error())
			continue
		}

		if len(result.Revert()) > 0 {
			resutls = append(resutls, newRevertError(result))
		}

		var ret hexutil.Bytes
		ret = result.Return()
		if ret != nil {
			resutls = append(resutls, ret)
		} else {
			resutls = append(resutls, result.Err.Error())
		}
	}
	return resutls, nil
}

// ==========================================================
// full tx sub
// ==========================================================
func (api *MevAPI) NewFullPendingTransactions(ctx context.Context) (*rpc.Subscription, error) {
	notifier, supported := rpc.NotifierFromContext(ctx)
	if !supported {
		return &rpc.Subscription{}, rpc.ErrNotificationsUnsupported
	}

	rpcSub := notifier.CreateSubscription()

	gopool.Submit(func() {
		txs := make(chan core.NewTxsEvent, 128)
		pendingTxSub := api.b.TxPool().SubscribeNewTxsEvent(txs)
		chainConfig := api.b.ChainConfig()

		for {
			select {
			case ev := <-txs:
				// To keep the original behaviour, send a single tx hash in one notification.
				// TODO(rjl493456442) Send a batch of tx hashes in one notification
				latest := api.b.CurrentHeader()
				for _, tx := range ev.Txs {
					rpcTx := ethapi.NewRPCPendingTransaction(tx, latest, chainConfig)
					notifier.Notify(rpcSub.ID, rpcTx)
				}
			case <-rpcSub.Err():
				pendingTxSub.Unsubscribe()
				return
			case <-notifier.Closed():
				pendingTxSub.Unsubscribe()
				return
			}
		}
	})

	return rpcSub, nil
}

// =============================================================================================================
//
//	Notifier function
//
// =============================================================================================================

func filterMatchTxs(filters *native.TraceFilterLookup, internalTxs SimResults) []*FilteredSimTx {
	finalTxs := make([]*FilteredSimTx, 0)
	for _, simTx := range internalTxs {
		ftx := &FilteredSimTx{
			SimTxInfo: simTx,
		}
		ftx.FilterResult = native.TracerFindLookupResult(simTx.tracer, filters)
		if len(ftx.FilterResult) > 0 {
			var err error
			simTx.TracerResult, err = simTx.tracer.GetResult()
			if err != nil {
				log.Warn("[MEV] tracer to json", "error", err)
				continue
			}
			finalTxs = append(finalTxs, ftx)
		}
	}
	return finalTxs
}

func (api *MevAPI) NewMevTransactions(ctx context.Context, filterArgs []native.FilterArgs) (*rpc.Subscription, error) {
	notifier, supported := rpc.NotifierFromContext(ctx)
	if !supported {
		return &rpc.Subscription{}, rpc.ErrNotificationsUnsupported
	}

	rpcSub := notifier.CreateSubscription()
	fmt.Println("[MEV] Subscribe mev txs from:", rpcSub.ID)

	gopool.Submit(func() {
		txFilter := native.NewFilterLookup(filterArgs)
		mevTxCh := make(chan SimResults, 1024)
		mevTxSub := api.mevTxsFeed.Subscribe(mevTxCh)

		for {
			select {
			case internalTxs := <-mevTxCh:
				finalTxs := filterMatchTxs(txFilter, internalTxs)
				if len(finalTxs) > 0 {
					notifier.Notify(rpcSub.ID, finalTxs)
				}

			case err := <-rpcSub.Err():
				fmt.Println("[MEV] RPC sub error:", err, rpcSub.ID)
				mevTxSub.Unsubscribe()
				return
			case <-notifier.Closed():
				fmt.Println("[MEV] notifier closed:", rpcSub.ID)
				mevTxSub.Unsubscribe()
				return
			}
		}
	})

	return rpcSub, nil
}
