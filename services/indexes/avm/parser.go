// (c) 2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package avm

import (
	"github.com/lasthyphen/avalanchego-1.4.11/codec"
	"github.com/lasthyphen/avalanchego-1.4.11/vms/avm"
)

func parseTx(c codec.Manager, bytes []byte) (*avm.Tx, error) {
	tx := &avm.Tx{}
	ver, err := c.Unmarshal(bytes, tx)
	if err != nil {
		return nil, err
	}
	unsignedBytes, err := c.Marshal(ver, &tx.UnsignedTx)
	if err != nil {
		return nil, err
	}

	tx.Initialize(unsignedBytes, bytes)
	return tx, nil
}
