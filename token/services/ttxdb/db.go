/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package ttxdb

import (
	"context"
	"math/big"
	"reflect"
	"time"

	"github.com/hyperledger-labs/fabric-smart-client/platform/view/services/cache/secondcache"
	"github.com/hyperledger-labs/fabric-token-sdk/token"
	driver2 "github.com/hyperledger-labs/fabric-token-sdk/token/driver"
	"github.com/hyperledger-labs/fabric-token-sdk/token/services/db"
	"github.com/hyperledger-labs/fabric-token-sdk/token/services/db/driver"
	"github.com/hyperledger-labs/fabric-token-sdk/token/services/logging"
	"github.com/pkg/errors"
)

type (
	Holder  = db.DriverHolder[*DB, driver.TokenTransactionDB, driver.TTXDBDriver]
	Manager = db.Manager[*DB, driver.TokenTransactionDB, driver.TTXDBDriver]
)

var (
	managerType = reflect.TypeOf((*Manager)(nil))
	logger      = logging.MustGetLogger("token-sdk.ttxdb")
)

func NewHolder(drivers []db.NamedDriver[driver.TTXDBDriver]) *Holder {
	return db.NewDriverHolder[*DB, driver.TokenTransactionDB, driver.TTXDBDriver](newDB, drivers...)
}

func GetByTMSId(sp token.ServiceProvider, tmsID token.TMSID) (*DB, error) {
	s, err := sp.GetService(managerType)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get manager service")
	}
	c, err := s.(*Manager).DBByTMSId(tmsID)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get db for tms [%s]", tmsID)
	}
	return c, nil
}

// TxStatus is the status of a transaction
type TxStatus = driver.TxStatus

const (
	// Unknown is the status of a transaction that is unknown
	Unknown = driver.Unknown
	// Pending is the status of a transaction that has been submitted to the ledger
	Pending = driver.Pending
	// Confirmed is the status of a transaction that has been confirmed by the ledger
	Confirmed = driver.Confirmed
	// Deleted is the status of a transaction that has been deleted due to a failure to commit
	Deleted = driver.Deleted
)

// TxStatusMessage maps TxStatus to string
var TxStatusMessage = driver.TxStatusMessage

// ActionType is the type of action performed by a transaction.
type ActionType = driver.ActionType

const (
	// Issue is the action type for issuing tokens.
	Issue = driver.Issue
	// Transfer is the action type for transferring tokens.
	Transfer = driver.Transfer
	// Redeem is the action type for redeeming tokens.
	Redeem = driver.Redeem
)

// TransactionRecord is a more finer-grained version of a movement record.
// Given a Token Transaction, for each token action in the Token Request,
// a transaction record is created for each unique enrollment ID found in the outputs.
// The transaction record contains the total amount of the token type that was transferred to/from that enrollment ID
// in that action.
type TransactionRecord = driver.TransactionRecord

// MovementRecord is a record of a movement of assets.
// Given a Token Transaction, a movement record is created for each enrollment ID that participated in the transaction
// and each token type that was transferred.
// The movement record contains the total amount of the token type that was transferred to/from the enrollment ID
// in a given token transaction.
type MovementRecord = driver.MovementRecord

// ValidationRecord is a more finer-grained version of a movement record.
// Given a Token Transaction, for each token action in the Token Request,
// a transaction record is created for each unique enrollment ID found in the outputs.
// The transaction record contains the total amount of the token type that was transferred to/from that enrollment ID
// in that action.
type ValidationRecord = driver.ValidationRecord

// TransactionIterator is an iterator over transaction records
type TransactionIterator struct {
	it driver.TransactionIterator
}

// Close closes the iterator. It must be called when done with the iterator.
func (t *TransactionIterator) Close() {
	t.it.Close()
}

// Next returns the next transaction record, if any.
// It returns nil, nil if there are no more records.
func (t *TransactionIterator) Next() (*TransactionRecord, error) {
	next, err := t.it.Next()
	if err != nil {
		return nil, err
	}
	if next == nil {
		return nil, nil
	}
	return next, nil
}

// ValidationRecordsIterator is an iterator over validation records
type ValidationRecordsIterator struct {
	it driver.ValidationRecordsIterator
}

// Close closes the iterator. It must be called when done with the iterator.
func (t *ValidationRecordsIterator) Close() {
	t.it.Close()
}

// Next returns the next validation record, if any.
// It returns nil, nil if there are no more records.
func (t *ValidationRecordsIterator) Next() (*ValidationRecord, error) {
	next, err := t.it.Next()
	if err != nil {
		return nil, err
	}
	if next == nil {
		return nil, nil
	}
	return next, nil
}

// Wallet models a wallet
type Wallet interface {
	// ID returns the wallet ID
	ID() string
	// TMS returns the TMS of the wallet
	TMS() *token.ManagementService
}

type Cache interface {
	Get(key string) ([]byte, bool)
	Add(key string, value []byte)
	Delete(key string)
}

// DB is a database that stores token transactions related information
type DB struct {
	*db.StatusSupport
	db    driver.TokenTransactionDB
	cache Cache
}

func newDB(p driver.TokenTransactionDB) *DB {
	return &DB{
		StatusSupport: db.NewStatusSupport(),
		db:            p,
		cache:         secondcache.NewTyped[[]byte](1000),
	}
}

// QueryTransactionsParams defines the parameters for querying movements
type QueryTransactionsParams = driver.QueryTransactionsParams

// QueryTokenRequestsParams defines the parameters for querying token requests
type QueryTokenRequestsParams = driver.QueryTokenRequestsParams

// QueryValidationRecordsParams defines the parameters for querying movements
type QueryValidationRecordsParams = driver.QueryValidationRecordsParams

// Transactions returns an iterators of transaction records filtered by the given params.
func (d *DB) Transactions(params QueryTransactionsParams) (driver.TransactionIterator, error) {
	return d.db.QueryTransactions(params)
}

// TokenRequests returns an iterator over the token requests matching the passed params
func (d *DB) TokenRequests(params QueryTokenRequestsParams) (driver.TokenRequestIterator, error) {
	return d.db.QueryTokenRequests(params)
}

// ValidationRecords returns an iterators of validation records filtered by the given params.
func (d *DB) ValidationRecords(params QueryValidationRecordsParams) (*ValidationRecordsIterator, error) {
	it, err := d.db.QueryValidations(params)
	if err != nil {
		return nil, errors.Errorf("failed to query validation records: %s", err)
	}
	return &ValidationRecordsIterator{it: it}, nil
}

// AppendTransactionRecord appends the transaction records corresponding to the passed token request.
func (d *DB) AppendTransactionRecord(req *token.Request) error {
	logger.Debugf("appending new transaction record... [%s]", req.Anchor)

	ins, outs, err := req.InputsAndOutputs()
	if err != nil {
		return errors.WithMessagef(err, "failed getting inputs and outputs for request [%s]", req.Anchor)
	}
	record := &token.AuditRecord{
		Anchor:  req.Anchor,
		Inputs:  ins,
		Outputs: outs,
	}

	raw, err := req.Bytes()
	if err != nil {
		return errors.Wrapf(err, "failed to marshal token request [%s]", req.Anchor)
	}
	txs, err := TransactionRecords(record, time.Now().UTC())
	if err != nil {
		return errors.WithMessage(err, "failed parsing transactions from audit record")
	}

	logger.Debugf("storing new records... [%d,%d]", len(raw), len(txs))
	w, err := d.db.BeginAtomicWrite()
	if err != nil {
		return errors.WithMessagef(err, "begin update for txid [%s] failed", record.Anchor)
	}
	d.cache.Add(record.Anchor, raw)
	if err := w.AddTokenRequest(
		record.Anchor,
		raw,
		req.Metadata.Application,
		req.TokenService.PublicParametersManager().PublicParamsHash(),
	); err != nil {
		w.Rollback()
		return errors.WithMessagef(err, "append token request for txid [%s] failed", record.Anchor)
	}
	for _, tx := range txs {
		if err := w.AddTransaction(&tx); err != nil {
			w.Rollback()
			return errors.WithMessagef(err, "append transactions for txid [%s] failed", record.Anchor)
		}
	}
	if err := w.Commit(); err != nil {
		return errors.WithMessagef(err, "committing tx for txid [%s] failed", record.Anchor)
	}

	logger.Debugf("appending transaction record new completed without errors")
	return nil
}

// SetStatus sets the status of the audit records with the passed transaction id to the passed status
func (d *DB) SetStatus(ctx context.Context, txID string, status driver.TxStatus, message string) error {
	logger.Debugf("set status [%s][%s]...", txID, status)
	if err := d.db.SetStatus(ctx, txID, status, message); err != nil {
		return errors.Wrapf(err, "failed setting status [%s][%s]", txID, driver.TxStatusMessage[status])
	}

	// notify the listeners
	d.Notify(db.StatusEvent{
		Ctx:            ctx,
		TxID:           txID,
		ValidationCode: status,
	})
	logger.Debugf("set status [%s][%s] done", txID, driver.TxStatusMessage[status])
	return nil
}

// GetStatus return the status of the given transaction id.
// It returns an error if no transaction with that id is found
func (d *DB) GetStatus(txID string) (TxStatus, string, error) {
	logger.Debugf("get status [%s]...", txID)
	status, message, err := d.db.GetStatus(txID)
	if err != nil {
		return Unknown, "", errors.Wrapf(err, "failed geting status [%s]", txID)
	}
	logger.Debugf("got status [%s][%s]", txID, status)
	return status, message, nil
}

// GetTokenRequest returns the token request bound to the passed transaction id, if available.
func (d *DB) GetTokenRequest(txID string) ([]byte, error) {
	res, ok := d.cache.Get(txID)
	if ok {
		return res, nil
	}
	return d.db.GetTokenRequest(txID)
}

// AddTransactionEndorsementAck records the signature of a given endorser for a given transaction
func (d *DB) AddTransactionEndorsementAck(txID string, id token.Identity, sigma []byte) error {
	return d.db.AddTransactionEndorsementAck(txID, id, sigma)
}

// GetTransactionEndorsementAcks returns the endorsement signatures for the given transaction id
func (d *DB) GetTransactionEndorsementAcks(txID string) (map[string][]byte, error) {
	return d.db.GetTransactionEndorsementAcks(txID)
}

// AppendValidationRecord appends the given validation metadata related to the given transaction id
func (d *DB) AppendValidationRecord(txID string, tokenRequest []byte, meta map[string][]byte, ppHash driver2.PPHash) error {
	logger.Debugf("appending new validation record... [%s]", txID)

	w, err := d.db.BeginAtomicWrite()
	if err != nil {
		return errors.WithMessagef(err, "begin update for txid [%s] failed", txID)
	}
	// we store the token request, but don't have or care about the application metadata
	d.cache.Add(txID, tokenRequest)
	if err := w.AddTokenRequest(txID, tokenRequest, nil, ppHash); err != nil {
		w.Rollback()
		return errors.WithMessagef(err, "append token request for txid [%s] failed", txID)
	}
	if err := w.AddValidationRecord(txID, meta); err != nil {
		w.Rollback()
		return errors.WithMessagef(err, "append validation record for txid [%s] failed", txID)
	}
	if err := w.Commit(); err != nil {
		return errors.WithMessagef(err, "append validation record commit for txid [%s] failed", txID)
	}
	logger.Debugf("appending validation record completed without errors")
	return nil
}

// TransactionRecords is a pure function that converts an AuditRecord for storage in the database.
func TransactionRecords(record *token.AuditRecord, timestamp time.Time) (txs []TransactionRecord, err error) {
	inputs := record.Inputs
	outputs := record.Outputs

	actionIndex := 0
	for {
		// collect inputs and outputs from the same action
		ins := inputs.Filter(func(t *token.Input) bool {
			return t.ActionIndex == actionIndex
		})
		ous := outputs.Filter(func(t *token.Output) bool {
			return t.ActionIndex == actionIndex
		})
		if ins.Count() == 0 && ous.Count() == 0 {
			logger.Debugf("no actions left for tx [%s][%d]", record.Anchor, actionIndex)
			break
		}

		// create a transaction record from ins and ous

		// All ins should be for same EID, check this
		inEIDs := ins.EnrollmentIDs()
		if len(inEIDs) > 1 {
			return nil, errors.Errorf("expected at most 1 input enrollment id, got %d, [%v]", len(inEIDs), inEIDs)
		}
		inEID := ""
		if len(inEIDs) == 1 {
			inEID = inEIDs[0]
		}

		outEIDs := ous.EnrollmentIDs()
		outEIDs = append(outEIDs, "")
		outTT := ous.TokenTypes()
		for _, outEID := range outEIDs {
			for _, tokenType := range outTT {
				received := ous.ByEnrollmentID(outEID).ByType(tokenType).Sum()
				if received.Cmp(big.NewInt(0)) <= 0 {
					continue
				}

				tt := driver.Issue
				if len(inEIDs) != 0 {
					if len(outEID) == 0 {
						tt = driver.Redeem
					} else {
						tt = driver.Transfer
					}
				}

				txs = append(txs, driver.TransactionRecord{
					TxID:         record.Anchor,
					SenderEID:    inEID,
					RecipientEID: outEID,
					TokenType:    tokenType,
					Amount:       received,
					Status:       driver.Pending,
					ActionType:   tt,
					Timestamp:    timestamp,
				})
			}
		}

		actionIndex++
	}
	logger.Debugf("parsed transactions for tx [%s]", record.Anchor)

	return
}

// Movements converts an AuditRecord to MovementRecords for storage in the database.
// A positive movement Amount means incoming tokens, and negative means outgoing tokens from the enrollment ID.
func Movements(record *token.AuditRecord, created time.Time) (mv []MovementRecord, err error) {
	inputs := record.Inputs
	outputs := record.Outputs
	// we need to consider both inputs and outputs enrollment IDs because the record can refer to a redeem
	eIDs := joinIOEIDs(record)
	logger.Debugf("eIDs [%v]", eIDs)
	tokenTypes := outputs.TokenTypes()

	for _, eID := range eIDs {
		for _, tokenType := range tokenTypes {
			received := outputs.ByEnrollmentID(eID).ByType(tokenType).Sum()
			sent := inputs.ByEnrollmentID(eID).ByType(tokenType).Sum()
			diff := received.Sub(received, sent)
			if sent == received {
				continue
			}

			logger.Debugf("adding movement [%s:%d]", eID, diff.Int64())
			mv = append(mv, driver.MovementRecord{
				TxID:         record.Anchor,
				EnrollmentID: eID,
				Amount:       diff,
				TokenType:    tokenType,
				Timestamp:    created,
				Status:       driver.Pending,
			})
		}
	}
	logger.Debugf("finished to parse sent movements for tx [%s]", record.Anchor)

	return
}

// joinIOEIDs joins enrollment IDs of inputs and outputs
func joinIOEIDs(record *token.AuditRecord) []string {
	iEIDs := record.Inputs.EnrollmentIDs()
	oEIDs := record.Outputs.EnrollmentIDs()
	eIDs := append(iEIDs, oEIDs...)
	eIDs = deduplicate(eIDs)
	return eIDs
}

// deduplicate removes duplicate entries from a slice
func deduplicate(source []string) []string {
	support := make(map[string]bool)
	var res []string
	for _, item := range source {
		if _, value := support[item]; !value {
			support[item] = true
			res = append(res, item)
		}
	}
	return res
}
