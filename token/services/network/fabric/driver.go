/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package fabric

import (
	"github.com/hyperledger-labs/fabric-smart-client/platform/fabric"
	"github.com/hyperledger-labs/fabric-token-sdk/token"
	"github.com/hyperledger-labs/fabric-token-sdk/token/services/auditdb"
	"github.com/hyperledger-labs/fabric-token-sdk/token/services/config"
	"github.com/hyperledger-labs/fabric-token-sdk/token/services/network/common"
	"github.com/hyperledger-labs/fabric-token-sdk/token/services/network/driver"
	"github.com/hyperledger-labs/fabric-token-sdk/token/services/tokens"
	"github.com/hyperledger-labs/fabric-token-sdk/token/services/ttxdb"
	"github.com/hyperledger-labs/fabric-token-sdk/token/services/vault"
	"github.com/pkg/errors"
)

func NewDriver(auditDBManager *auditdb.Manager, ttxDBManager *ttxdb.Manager) driver.NamedDriver {
	return driver.NamedDriver{
		Name: "fabric",
		Driver: &Driver{
			auditDBManager: auditDBManager,
			ttxDBManager:   ttxDBManager,
		},
	}
}

type Driver struct {
	auditDBManager *auditdb.Manager
	ttxDBManager   *ttxdb.Manager
}

func (d *Driver) New(sp token.ServiceProvider, network, channel string) (driver.Network, error) {
	n, err := fabric.GetFabricNetworkService(sp, network)
	if err != nil {
		return nil, errors.WithMessagef(err, "fabric network [%s] not found", network)
	}
	ch, err := n.Channel(channel)
	if err != nil {
		return nil, errors.WithMessagef(err, "fabric channel [%s:%s] not found", network, channel)
	}
	m, err := vault.GetProvider(sp)
	if err != nil {
		return nil, errors.WithMessagef(err, "failed to get vault manager")
	}
	tokensProvider, err := tokens.GetProvider(sp)
	if err != nil {
		return nil, errors.WithMessagef(err, "failed to get tokens db provider")
	}
	cs, err := config.GetService(sp)
	if err != nil {
		return nil, errors.WithMessage(err, "failed to get config service")
	}
	return NewNetwork(
		sp,
		n,
		ch,
		m.Vault,
		cs,
		common.NewAcceptTxInDBFilterProvider(d.ttxDBManager, d.auditDBManager),
		tokensProvider,
	), nil
}
