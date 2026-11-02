package stagedsync

import (
	"context"

	"github.com/erigontech/erigon/db/dbservices"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/db/state/execctx"
	"github.com/erigontech/erigon/execution/chain"
	"github.com/erigontech/erigon/execution/protocol/rules"
)

type Eip8304ReceiptSourceForTest = eip8304ReceiptSource

func NewEip8304ReceiptSourceForTest(ctx context.Context, tx kv.TemporalTx, blockReader dbservices.FullBlockReader, chainConfig *chain.Config, engine rules.EngineReader, doms *execctx.SharedDomains) Eip8304ReceiptSourceForTest {
	return eip8304ReceiptSource{ctx: ctx, tx: tx, blockReader: blockReader, chainConfig: chainConfig, engine: engine, doms: doms}
}
