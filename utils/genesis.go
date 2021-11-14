// (c) 2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package utils

import (
	"github.com/lasthyphen/avalanchego-1.4.11/genesis"
	"github.com/lasthyphen/avalanchego-1.4.11/ids"
	avmVM "github.com/lasthyphen/avalanchego-1.4.11/vms/avm"
	"github.com/lasthyphen/avalanchego-1.4.11/vms/platformvm"
)

type GenesisContainer struct {
	NetworkID       uint32
	XChainGenesisTx *platformvm.Tx
	XChainID        ids.ID
	DjtxAssetID     ids.ID
	GenesisBytes    []byte
}

func NewGenesisContainer(networkID uint32) (*GenesisContainer, error) {
	gc := &GenesisContainer{NetworkID: networkID}
	var err error
	gc.GenesisBytes, gc.DjtxAssetID, err = genesis.Genesis(gc.NetworkID, "")
	if err != nil {
		return nil, err
	}

	gc.XChainGenesisTx, err = genesis.VMGenesis(gc.GenesisBytes, avmVM.ID)
	if err != nil {
		return nil, err
	}

	gc.XChainID = gc.XChainGenesisTx.ID()
	return gc, nil
}
