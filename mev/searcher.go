package mev

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

const (
	maxTxsFromAccountNum = 3
)

// =============================================================================================================
//
//	Search mev transaction
//
// =============================================================================================================
func (api *MevAPI) searcherLoop() {

	fmt.Println("[MEV] searcher thread run")
	intrrupt := false
	txsCh := make(chan core.NewTxsEvent, 1024)
	chainHeadCh := make(chan core.ChainHeadEvent, 10)
	txsSub := api.b.TxPool().SubscribeNewTxsEvent(txsCh)
	chainHeadSub := api.b.SubscribeChainHeadEvent(chainHeadCh)

	for {
		select {
		case ev := <-txsCh:
			api.mevMu.RLock()
			simTxs, err := api.commitTransactions(ev.Txs, &intrrupt)
			api.mevMu.RUnlock()

			if err == nil && len(simTxs) > 0 {
				api.mevTxsFeed.Send(simTxs)
			}

		case <-txsSub.Err():
			chainHeadSub.Unsubscribe()
			log.Warn("[MEV] search loop exit, txsSub error")
			return

		case ev := <-chainHeadCh:
			log.Info("[MEV] new chain header", "blockNumber:", ev.Block.Header().Number)
			intrrupt = true
			api.mevMu.Lock()
			intrrupt = false
			api.accNonceMark = make(map[common.Address]uint64)
			api.mevMu.Unlock()

		case <-chainHeadSub.Err():
			txsSub.Unsubscribe()
			log.Warn("[MEV] search loop exit, chainHeadSub error")
			return

		case <-api.stopCh:
			intrrupt = true
			api.mevMu.Lock()
			txsSub.Unsubscribe()
			chainHeadSub.Unsubscribe()
			api.mevMu.Unlock()
			log.Info("[MEV] search loop exit, user control")
			return
		}
	}
}

//=================================================================
// simulation functions
//=================================================================

func (api *MevAPI) commitTransactions(evTxs []*types.Transaction, interrupt *bool) (SimResults, error) {
	var (
		tracer  tracers.Tracer
		receipt *types.Receipt
	)

	simResults := make(SimResults, 0)
	ctx := context.Background()

	callTracerStr := "callTracer"
	traceConfig := &tracers.TraceConfig{
		Tracer:       &callTracerStr,
		TracerConfig: json.RawMessage(`{"WithLog":true}`),
	}

	signer := types.LatestSigner(api.b.ChainConfig())
	accTxs := make(map[common.Address]types.Transactions, 0)
	for _, tx := range evTxs {
		from, _ := types.Sender(signer, tx)
		accTxs[from] = append(accTxs[from], tx)
	}

	gp := new(core.GasPool).AddGas(math.MaxUint64)
	chainConfig := api.b.ChainConfig()

	for from, txs := range accTxs {
		if *interrupt {
			break
		}

		pendingTxs := api.b.TxPool().PendingFrom(from)
		if len(pendingTxs) == 0 {
			fmt.Println("From:", from.String(), "recv tx:", len(txs), "but pending list is:", 0)
			continue
		}
		// 按 Nonce 升序
		sort.SliceStable(txs, func(i, j int) bool {
			return txs[i].Nonce() < txs[j].Nonce()
		})

		lowNonce := pendingTxs[0].Nonce()
		tx0Nonce := txs[0].Nonce()
		if tx0Nonce < lowNonce {
			fmt.Println("[MEV] Nonce too low From:", from.String(), "pending[0].Nonce", lowNonce, "txNonce:", tx0Nonce)
			continue
		}

		if len(pendingTxs) >= maxTxsFromAccountNum {
			maxNonce := pendingTxs[maxTxsFromAccountNum-1].Nonce()
			if tx0Nonce > maxNonce {
				waitNonce, ok := api.accNonceMark[from]
				if ok && waitNonce > lowNonce {
					// 已经sim的tx还未上链,不用再次sim
					fmt.Println("[MEV] From:", from.String(), "lowNonce", lowNonce, "waitNonce:", waitNonce, "txNonce:", tx0Nonce)
					continue
				}
				api.accNonceMark[from] = waitNonce + uint64(maxTxsFromAccountNum)
				txs = pendingTxs[0:maxTxsFromAccountNum]
			}
		}

		block, err := api.b.BlockByNumber(ctx, rpc.LatestBlockNumber)
		if err != nil {
			return nil, err
		}
		statedb, release, err := api.b.StateAtBlock(ctx, block, 128, nil, true, false)
		if err != nil {
			return nil, err
		}
		defer release()
		header := block.Header()

		var lastGas *big.Int
		signer := types.MakeSigner(chainConfig, header.Number, header.Time)
		preGasIsDecrese := true
		prevTxsInfo := make([]*PrevTxInfo, 0)
		for idx, tx := range pendingTxs {
			if len(txs) == 0 || idx == maxTxsFromAccountNum {
				break
			}

			setResult := false
			if txs[0].Nonce() == tx.Nonce() {
				txs = txs[1:]
				setResult = true
			}
			tracer, err = tracers.DefaultDirectory.New(*traceConfig.Tracer, new(tracers.Context), traceConfig.TracerConfig)
			if err != nil {
				fmt.Println("[MEV] Get tracer error", err)
				break
			}
			statedb.SetTxContext(tx.Hash(), idx)
			msg, err := core.TransactionToMessage(tx, signer, header.BaseFee)
			if err != nil {
				fmt.Println("[MEV] tx as message error:", tx.Hash().String(), "blockNum:", header.Number, "err:", err)
				break
			}
			receipt, err = commitTransaction(chainConfig, api.b.Chain(), statedb, header, tx, msg, gp, vm.Config{Tracer: tracer, NoBaseFee: true}, false, nil)
			if err != nil {
				fmt.Println("[MEV] Sim error tx:", tx.Hash().String(), "blockNum:", header.Number, "err:", err)
				break
			}

			if receipt.Status != 1 {
				break
			}

			gasPrice, err := tx.EffectiveGasTip(header.BaseFee)
			if err != nil {
				fmt.Println("[MEV] get gasPrice error:", err)
				break

			}
			if lastGas == nil {
				lastGas = gasPrice
			} else {
				preGasIsDecrese = gasPrice.Cmp(lastGas) <= 0
			}

			if setResult {
				simResult := &SimResult{
					Hash:         tx.Hash(),
					GasUsed:      receipt.GasUsed,
					PrevGasIsDec: preGasIsDecrese,
					tracer:       tracer,
					EffGasPrice:  (*hexutil.Big)(gasPrice),
					GasTipCap:    (*hexutil.Big)(tx.GasTipCap()),
					GasPrice:     (*hexutil.Big)(tx.GasPrice()),
					TxType:       tx.Type(),
					BlockNumber:  header.Number.Uint64(),
					BlockTime:    header.Time,
					BlockHash:    block.Hash(),
				}
				simResult.PrevTxs = make([]*PrevTxInfo, len(prevTxsInfo))
				copy(simResult.PrevTxs, prevTxsInfo)
				simResults = append(simResults, simResult)
			}
			prevTxsInfo = append(prevTxsInfo, &PrevTxInfo{tx.Hash(), (*hexutil.Big)(tx.GasPrice())})
		}
	}

	return simResults, nil
}

func commitTransaction(config *params.ChainConfig, bc core.ChainContext, statedb *state.StateDB, header *types.Header, tx *types.Transaction, msg *core.Message, gp *core.GasPool, cfg vm.Config, errRevert bool, bover ethapi.BlockOverrides) (*types.Receipt, error) {
	var snap int
	if errRevert {
		snap = statedb.Snapshot()
	}

	receipt, err := ApplyTransactionNoProcessor(config, bc, &header.Coinbase, gp, statedb, header, tx, msg, &header.GasUsed, cfg, bover)

	if errRevert && err != nil {
		statedb.RevertToSnapshot(snap)
		if err != nil {
			return nil, err
		}
	}

	return receipt, err
}
