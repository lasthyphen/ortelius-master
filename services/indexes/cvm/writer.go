// (c) 2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package cvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lasthyphen/avalanchego-1.4.11/codec"
	"github.com/lasthyphen/avalanchego-1.4.11/genesis"
	"github.com/lasthyphen/avalanchego-1.4.11/ids"
	"github.com/lasthyphen/avalanchego-1.4.11/utils/hashing"
	"github.com/lasthyphen/avalanchego-1.4.11/utils/math"
	"github.com/lasthyphen/avalanchego-1.4.11/vms/components/verify"
	"github.com/ava-labs/coreth/core/types"
	"github.com/ava-labs/coreth/plugin/evm"
	"github.com/ava-labs/ortelius/cfg"
	"github.com/ava-labs/ortelius/db"
	"github.com/ava-labs/ortelius/models"
	"github.com/ava-labs/ortelius/modelsc"
	"github.com/ava-labs/ortelius/services"
	djtxIndexer "github.com/ava-labs/ortelius/services/indexes/djtx"
	"github.com/ava-labs/ortelius/utils"
	"github.com/ethereum/go-ethereum/common"
)

var (
	ErrUnknownBlockType = errors.New("unknown block type")
)

type Writer struct {
	networkID   uint32
	djtxAssetID ids.ID

	codec codec.Manager
	djtx  *djtxIndexer.Writer
}

func NewWriter(networkID uint32, chainID string) (*Writer, error) {
	_, djtxAssetID, err := genesis.Genesis(networkID, "")
	if err != nil {
		return nil, err
	}

	return &Writer{
		networkID:   networkID,
		djtxAssetID: djtxAssetID,
		codec:       evm.Codec,
		djtx:        djtxIndexer.NewWriter(chainID, djtxAssetID),
	}, nil
}

func (*Writer) Name() string { return "cvm-index" }

func (w *Writer) ParseJSON(txdata []byte) ([]byte, error) {
	block, err := modelsc.Unmarshal(txdata)
	if err != nil {
		return nil, err
	}
	if block.BlockExtraData == nil || len(block.BlockExtraData) == 0 {
		return []byte(""), nil
	}
	atomicTX := new(evm.Tx)
	_, err = w.codec.Unmarshal(block.BlockExtraData, atomicTX)
	if err != nil {
		return nil, err
	}

	return json.Marshal(atomicTX)
}

func (w *Writer) ConsumeLogs(ctx context.Context, conns *utils.Connections, c services.Consumable, txLogs *types.Log, persist db.Persist) error {
	job := conns.Stream().NewJob("cvm-index")
	sess := conns.DB().NewSessionForEventReceiver(job)

	dbTx, err := sess.Begin()
	if err != nil {
		return err
	}
	defer dbTx.RollbackUnlessCommitted()

	cCtx := services.NewConsumerContext(ctx, dbTx, c.Timestamp(), c.Nanosecond(), persist)

	firstTopic := ""
	if len(txLogs.Topics) > 0 {
		firstTopic = txLogs.Topics[0].Hex()
	}
	cvmLogs := &db.CvmLogs{
		BlockHash:     txLogs.BlockHash.Hex(),
		TxHash:        txLogs.TxHash.Hex(),
		LogIndex:      uint64(txLogs.Index),
		Block:         fmt.Sprintf("%d", txLogs.BlockNumber),
		FirstTopic:    firstTopic,
		Removed:       txLogs.Removed,
		CreatedAt:     cCtx.Time(),
		Serialization: c.Body(),
	}
	err = cvmLogs.ComputeID()
	if err != nil {
		return err
	}
	err = persist.InsertCvmLogs(ctx, dbTx, cvmLogs, cfg.PerformUpdates)
	if err != nil {
		return err
	}

	return dbTx.Commit()
}

func (w *Writer) ConsumeTrace(ctx context.Context, conns *utils.Connections, c services.Consumable, transactionTrace *modelsc.TransactionTrace, persist db.Persist) error {
	job := conns.Stream().NewJob("cvm-index")
	sess := conns.DB().NewSessionForEventReceiver(job)

	dbTx, err := sess.Begin()
	if err != nil {
		return err
	}
	defer dbTx.RollbackUnlessCommitted()

	txTraceModel := &models.CvmTransactionsTxDataTrace{}
	err = json.Unmarshal(transactionTrace.Trace, txTraceModel)
	if err != nil {
		return err
	}

	cCtx := services.NewConsumerContext(ctx, dbTx, c.Timestamp(), c.Nanosecond(), persist)

	txTraceService := &db.CvmTransactionsTxdataTrace{
		Hash:          transactionTrace.Hash,
		Idx:           transactionTrace.Idx,
		ToAddr:        txTraceModel.ToAddr,
		FromAddr:      txTraceModel.FromAddr,
		CallType:      txTraceModel.CallType,
		Type:          txTraceModel.Type,
		Serialization: transactionTrace.Trace,
		CreatedAt:     cCtx.Time(),
	}

	err = persist.InsertCvmTransactionsTxdataTrace(ctx, dbTx, txTraceService, cfg.PerformUpdates)
	if err != nil {
		return err
	}

	return dbTx.Commit()
}

func (w *Writer) Consume(ctx context.Context, conns *utils.Connections, c services.Consumable, block *modelsc.Block, persist db.Persist) error {
	job := conns.Stream().NewJob("cvm-index")
	sess := conns.DB().NewSessionForEventReceiver(job)

	dbTx, err := sess.Begin()
	if err != nil {
		return err
	}
	defer dbTx.RollbackUnlessCommitted()

	// Consume the tx and commit
	err = w.indexBlock(services.NewConsumerContext(ctx, dbTx, c.Timestamp(), c.Nanosecond(), persist), c.Body(), block)
	if err != nil {
		return err
	}
	return dbTx.Commit()
}

func (w *Writer) indexBlock(ctx services.ConsumerCtx, blockBytes []byte, block *modelsc.Block) error {
	var atomicTX *evm.Tx
	var unsignedBytes []byte
	if len(blockBytes) > 0 {
		atomicTX = new(evm.Tx)
		ver, err := w.codec.Unmarshal(blockBytes, atomicTX)
		if err != nil {
			return err
		}
		unsignedBytes, err = w.codec.Marshal(ver, &atomicTX.UnsignedAtomicTx)
		if err != nil {
			return err
		}
	}
	return w.indexBlockInternal(ctx, atomicTX, blockBytes, block, unsignedBytes)
}

func (w *Writer) indexBlockInternal(ctx services.ConsumerCtx, atomicTX *evm.Tx, blockBytes []byte, block *modelsc.Block, unsignedBytes []byte) error {
	txIDString := ""

	id, err := ids.ToID(hashing.ComputeHash256([]byte(block.Header.Number.String())))
	if err != nil {
		return err
	}

	var typ models.CChainType = 0
	var blockchainID string
	if atomicTX != nil {
		txID, err := ids.ToID(hashing.ComputeHash256(blockBytes))
		if err != nil {
			return err
		}
		txIDString = txID.String()
		switch atx := atomicTX.UnsignedAtomicTx.(type) {
		case *evm.UnsignedExportTx:
			typ = models.CChainExport
			blockchainID = atx.BlockchainID.String()
			err = w.indexExportTx(ctx, txID, atx, blockBytes)
			if err != nil {
				return err
			}
		case *evm.UnsignedImportTx:
			typ = models.CChainImport
			blockchainID = atx.BlockchainID.String()
			err = w.indexImportTx(ctx, txID, atx, atomicTX.Creds, blockBytes, unsignedBytes)
			if err != nil {
				return err
			}
		default:
		}
	}

	for ipos, rawtx := range block.Txs {
		rawtxCp := rawtx
		txdata, err := json.Marshal(&rawtxCp)
		if err != nil {
			return err
		}
		rawhash := rawtx.Hash()
		rcptstr := utils.CommonAddressHexRepair(rawtx.To())
		cvmTransactionTxdata := &db.CvmTransactionsTxdata{
			Hash:          rawhash.String(),
			Block:         block.Header.Number.String(),
			Idx:           uint64(ipos),
			Rcpt:          rcptstr,
			Nonce:         rawtx.Nonce(),
			Serialization: txdata,
			CreatedAt:     ctx.Time(),
		}
		err = ctx.Persist().InsertCvmTransactionsTxdata(ctx.Ctx(), ctx.DB(), cvmTransactionTxdata, cfg.PerformUpdates)
		if err != nil {
			return err
		}
	}
	block.TxsBytes = nil
	block.Txs = nil

	blockjson, err := json.Marshal(block)
	if err != nil {
		return err
	}

	htime := int64(block.Header.Time)
	if htime == 0 {
		htime = 1
	}
	tm := time.Unix(htime, 0)
	cvmTransaction := &db.CvmTransactions{
		ID:            id.String(),
		TransactionID: txIDString,
		Type:          typ,
		BlockchainID:  blockchainID,
		Block:         block.Header.Number.String(),
		CreatedAt:     ctx.Time(),
		Serialization: blockjson,
		TxTime:        tm,
		Nonce:         block.Header.Nonce.Uint64(),
		Hash:          block.Header.Hash().String(),
		ParentHash:    block.Header.ParentHash.String(),
	}
	err = ctx.Persist().InsertCvmTransactions(ctx.Ctx(), ctx.DB(), cvmTransaction, cfg.PerformUpdates)
	if err != nil {
		return err
	}

	return nil
}

func (w *Writer) indexTransaction(
	ctx services.ConsumerCtx,
	id ids.ID,
	typ models.CChainType,
	blockChainID ids.ID,
	txFee uint64,
	unsignedBytes []byte,
) error {
	avmTxtype := ""
	switch typ {
	case models.CChainImport:
		avmTxtype = "atomic_import_tx"
	case models.CChainExport:
		avmTxtype = "atomic_export_tx"
	}

	return w.djtx.InsertTransactionBase(
		ctx,
		id,
		blockChainID.String(),
		avmTxtype,
		[]byte(""),
		unsignedBytes,
		txFee,
		false,
		w.networkID,
	)
}

func (w *Writer) insertAddress(
	typ models.CChainType,
	ctx services.ConsumerCtx,
	idx uint64,
	id ids.ID,
	address common.Address,
	assetID ids.ID,
	amount uint64,
	nonce uint64,
) error {
	idprefix := id.Prefix(idx)

	cvmAddress := &db.CvmAddresses{
		ID:            idprefix.String(),
		Type:          typ,
		Idx:           idx,
		TransactionID: id.String(),
		Address:       address.String(),
		AssetID:       assetID.String(),
		Amount:        amount,
		Nonce:         nonce,
		CreatedAt:     ctx.Time(),
	}
	return ctx.Persist().InsertCvmAddresses(ctx.Ctx(), ctx.DB(), cvmAddress, cfg.PerformUpdates)
}

func (w *Writer) indexExportTx(ctx services.ConsumerCtx, txID ids.ID, tx *evm.UnsignedExportTx, blockBytes []byte) error {
	var err error

	var totalin uint64
	for icnt, in := range tx.Ins {
		icntval := uint64(icnt)
		err = w.insertAddress(models.CChainIn, ctx, icntval, txID, in.Address, in.AssetID, in.Amount, in.Nonce)
		if err != nil {
			return err
		}
		if in.AssetID == w.djtxAssetID {
			totalin, err = math.Add64(totalin, in.Amount)
			if err != nil {
				return err
			}
		}
	}

	var totalout uint64
	var idx uint32
	for _, out := range tx.ExportedOutputs {
		totalout, err = w.djtx.InsertTransactionOuts(idx, ctx, totalout, out, txID, tx.DestinationChain.String(), false, false)
		if err != nil {
			return err
		}
		idx++
	}

	return w.indexTransaction(ctx, txID, models.CChainExport, tx.BlockchainID, totalin-totalout, blockBytes)
}

func (w *Writer) indexImportTx(ctx services.ConsumerCtx, txID ids.ID, tx *evm.UnsignedImportTx, creds []verify.Verifiable, blockBytes []byte, unsignedBytes []byte) error {
	var err error

	var totalout uint64
	for icnt, out := range tx.Outs {
		icntval := uint64(icnt)
		err = w.insertAddress(models.CchainOut, ctx, icntval, txID, out.Address, out.AssetID, out.Amount, 0)
		if err != nil {
			return err
		}
		if out.AssetID == w.djtxAssetID {
			totalout, err = math.Add64(totalout, out.Amount)
			if err != nil {
				return err
			}
		}
	}

	var totalin uint64
	for inidx, in := range tx.ImportedInputs {
		totalin, err = w.djtx.InsertTransactionIns(inidx, ctx, totalin, in, txID, creds, unsignedBytes, tx.SourceChain.String())
		if err != nil {
			return err
		}
	}

	return w.indexTransaction(ctx, txID, models.CChainImport, tx.BlockchainID, totalin-totalout, blockBytes)
}
