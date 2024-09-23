/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package common

import (
	"context"
	"database/sql"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/hyperledger-labs/fabric-smart-client/pkg/utils/errors"
	"github.com/hyperledger-labs/fabric-smart-client/platform/view/services/db/driver/sql/common"
	"github.com/hyperledger-labs/fabric-smart-client/platform/view/services/tracing"
	tdriver "github.com/hyperledger-labs/fabric-token-sdk/token/driver"
	"github.com/hyperledger-labs/fabric-token-sdk/token/services/db/driver"
	"github.com/hyperledger-labs/fabric-token-sdk/token/token"
	"go.opentelemetry.io/otel/trace"
)

type tokenTables struct {
	Tokens         string
	Ownership      string
	PublicParams   string
	Certifications string
}

func NewTokenDB(db *sql.DB, opts NewDBOpts, ci TokenInterpreter) (driver.TokenDB, error) {
	tables, err := GetTableNames(opts.TablePrefix)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get table names")
	}

	tokenDB := newTokenDB(db, tokenTables{
		Tokens:         tables.Tokens,
		Ownership:      tables.Ownership,
		PublicParams:   tables.PublicParams,
		Certifications: tables.Certifications,
	}, ci)
	if opts.CreateSchema {
		if err = common.InitSchema(db, tokenDB.GetSchema()); err != nil {
			return nil, err
		}
	}
	return tokenDB, nil
}

type TokenDB struct {
	db    *sql.DB
	table tokenTables
	ci    TokenInterpreter
}

func newTokenDB(db *sql.DB, tables tokenTables, ci TokenInterpreter) *TokenDB {
	return &TokenDB{
		db:    db,
		table: tables,
		ci:    ci,
	}
}

func (db *TokenDB) StoreToken(tr driver.TokenRecord, owners []string) (err error) {
	tx, err := db.NewTokenDBTransaction(context.TODO())
	if err != nil {
		return
	}
	if err = tx.StoreToken(context.TODO(), tr, owners); err != nil {
		if err1 := tx.Rollback(); err1 != nil {
			logger.Errorf("error rolling back: %s", err1.Error())
		}
		return
	}
	if err = tx.Commit(); err != nil {
		return
	}
	return nil
}

// DeleteTokens deletes multiple tokens at the same time (when spent, invalid or expired)
func (db *TokenDB) DeleteTokens(deletedBy string, ids ...*token.ID) error {
	logger.Debugf("delete tokens [%s][%v]", deletedBy, ids)
	if len(ids) == 0 {
		return nil
	}
	cond := db.ci.HasTokens("tx_id", "idx", ids...)
	args := append([]any{deletedBy, time.Now().UTC()}, cond.Params()...)
	offset := 3
	where := cond.ToString(&offset)

	query := fmt.Sprintf("UPDATE %s SET is_deleted = true, spent_by = $1, spent_at = $2 WHERE %s", db.table.Tokens, where)
	logger.Debug(query, args)
	if _, err := db.db.Exec(query, args...); err != nil {
		return errors.Wrapf(err, "error setting tokens to deleted [%v]", ids)
	}
	return nil
}

// IsMine just checks if the token is in the local storage and not deleted
func (db *TokenDB) IsMine(txID string, index uint64) (bool, error) {
	id := ""
	query := fmt.Sprintf("SELECT tx_id FROM %s WHERE tx_id = $1 AND idx = $2 AND is_deleted = false AND owner = true LIMIT 1;", db.table.Tokens)
	logger.Debug(query, txID, index)

	row := db.db.QueryRow(query, txID, index)
	if err := row.Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, errors.Wrapf(err, "error querying db")
	}
	return id == txID, nil
}

// UnspentTokensIterator returns an iterator over all unspent tokens
func (db *TokenDB) UnspentTokensIterator() (tdriver.UnspentTokensIterator, error) {
	return db.UnspentTokensIteratorBy(context.TODO(), "", "")
}

// UnspentTokensIteratorBy returns an iterator of unspent tokens owned by the passed id and whose type is the passed on.
// The token type can be empty. In that case, tokens of any type are returned.
func (db *TokenDB) UnspentTokensIteratorBy(ctx context.Context, walletID, tokenType string) (tdriver.UnspentTokensIterator, error) {
	span := trace.SpanFromContext(ctx)
	where, args := common.Where(db.ci.HasTokenDetails(driver.QueryTokenDetailsParams{
		WalletID:  walletID,
		TokenType: tokenType,
	}, db.table.Tokens))
	join := joinOnTokenID(db.table.Tokens, db.table.Ownership)

	query := fmt.Sprintf("SELECT %s.tx_id, %s.idx, owner_raw, token_type, quantity FROM %s %s %s",
		db.table.Tokens, db.table.Tokens, db.table.Tokens, join, where)

	logger.Debug(query, args)
	span.AddEvent("start_query", tracing.WithAttributes(tracing.String(QueryLabel, query)))
	rows, err := db.db.Query(query, args...)
	span.AddEvent("end_query")

	return &UnspentTokensIterator{txs: rows}, err
}

// UnspentTokensInWalletIterator returns the minimum information about the tokens needed for the selector
func (db *TokenDB) SpendableTokensIteratorBy(ctx context.Context, walletID string, typ string) (tdriver.SpendableTokensIterator, error) {
	span := trace.SpanFromContext(ctx)
	where, args := common.Where(db.ci.HasTokenDetails(driver.QueryTokenDetailsParams{
		WalletID:  walletID,
		TokenType: typ,
	}, ""))
	query := fmt.Sprintf(
		"SELECT tx_id, idx, token_type, quantity, owner_wallet_id FROM %s %s",
		db.table.Tokens, where,
	)

	logger.Debug(query, args)
	span.AddEvent("start_query", tracing.WithAttributes(tracing.String(QueryLabel, query)))
	rows, err := db.db.Query(query, args...)
	span.AddEvent("end_query")
	if err != nil {
		return nil, errors.Wrapf(err, "error querying db")
	}
	return &UnspentTokensInWalletIterator{txs: rows}, nil
}

// Balance returns the sun of the amounts, with 64 bits of precision, of the tokens with type and EID equal to those passed as arguments.
func (db *TokenDB) Balance(walletID, typ string) (uint64, error) {
	where, args := common.Where(db.ci.HasTokenDetails(driver.QueryTokenDetailsParams{
		WalletID:  walletID,
		TokenType: typ,
	}, db.table.Tokens))
	join := joinOnTokenID(db.table.Tokens, db.table.Ownership)
	query := fmt.Sprintf("SELECT SUM(amount) FROM %s %s %s", db.table.Tokens, join, where)

	logger.Debug(query, args)
	row := db.db.QueryRow(query, args...)
	var sum *uint64
	if err := row.Scan(&sum); err != nil {
		if errors.HasCause(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, errors.Wrapf(err, "error querying db")
	}
	if sum == nil {
		return 0, nil
	}
	return *sum, nil
}

// ListUnspentTokensBy returns the list of unspent tokens, filtered by owner and token type
func (db *TokenDB) ListUnspentTokensBy(walletID, typ string) (*token.UnspentTokens, error) {
	logger.Debugf("list unspent token by [%s,%s]", walletID, typ)
	it, err := db.UnspentTokensIteratorBy(context.TODO(), walletID, typ)
	if err != nil {
		return nil, err
	}
	defer it.Close()
	tokens := make([]*token.UnspentToken, 0)
	for {
		next, err := it.Next()
		switch {
		case err != nil:
			logger.Errorf("scan failed [%s]", err)
			return nil, err
		case next == nil:
			return &token.UnspentTokens{Tokens: tokens}, nil
		default:
			tokens = append(tokens, next)
		}
	}
}

// ListUnspentTokens returns the list of unspent tokens
func (db *TokenDB) ListUnspentTokens() (*token.UnspentTokens, error) {
	logger.Debugf("list unspent tokens...")
	it, err := db.UnspentTokensIterator()
	if err != nil {
		return nil, err
	}
	defer it.Close()
	tokens := make([]*token.UnspentToken, 0)
	for {
		next, err := it.Next()
		switch {
		case err != nil:
			logger.Errorf("scan failed [%s]", err)
			return nil, err
		case next == nil:
			return &token.UnspentTokens{Tokens: tokens}, nil
		default:
			tokens = append(tokens, next)
		}
	}
}

// ListAuditTokens returns the audited tokens associated to the passed ids
func (db *TokenDB) ListAuditTokens(ids ...*token.ID) ([]*token.Token, error) {
	if len(ids) == 0 {
		return []*token.Token{}, nil
	}
	where, args := common.Where(db.ci.And(
		db.ci.HasTokens("tx_id", "idx", ids...),
		common.ConstCondition("auditor = true"),
	))

	query := fmt.Sprintf("SELECT tx_id, idx, owner_raw, token_type, quantity FROM %s %s", db.table.Tokens, where)
	logger.Debug(query, args)
	rows, err := db.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tokens := make([]*token.Token, len(ids))
	counter := 0
	for rows.Next() {
		id := token.ID{}
		tok := token.Token{
			Owner: &token.Owner{
				Raw: []byte{},
			},
			Type:     "",
			Quantity: "",
		}
		if err := rows.Scan(&id.TxId, &id.Index, &tok.Owner.Raw, &tok.Type, &tok.Quantity); err != nil {
			return tokens, err
		}

		// the result is expected to be in order of the ids
		found := false
		for i := 0; i < len(ids); i++ {
			if ids[i].Equal(id) {
				tokens[i] = &tok
				found = true
				counter++
			}
		}
		if !found {
			return nil, errors.Errorf("retrieved wrong token [%s]", id)
		}
	}

	if rows.Err() != nil {
		return nil, rows.Err()
	}
	if counter == 0 {
		return nil, errors.Errorf("token not found for key [%s:%d]", ids[0].TxId, ids[0].Index)
	}
	if counter != len(ids) {
		for j, t := range tokens {
			if t == nil {
				return nil, errors.Errorf("token not found for key [%s:%d]", ids[j].TxId, ids[j].Index)
			}
		}
		panic("programming error: should not reach this point")
	}
	return tokens, nil
}

// ListHistoryIssuedTokens returns the list of issued tokens
func (db *TokenDB) ListHistoryIssuedTokens() (*token.IssuedTokens, error) {
	query := fmt.Sprintf("SELECT tx_id, idx, owner_raw, token_type, quantity, issuer_raw FROM %s WHERE issuer = true", db.table.Tokens)
	logger.Debug(query)
	rows, err := db.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tokens := []*token.IssuedToken{}
	for rows.Next() {
		tok := token.IssuedToken{
			Id: &token.ID{
				TxId:  "",
				Index: 0,
			},
			Owner: &token.Owner{
				Raw: []byte{},
			},
			Type:     "",
			Quantity: "",
			Issuer: &token.Owner{
				Raw: []byte{},
			},
		}
		if err := rows.Scan(&tok.Id.TxId, &tok.Id.Index, &tok.Owner.Raw, &tok.Type, &tok.Quantity, &tok.Issuer.Raw); err != nil {
			return nil, err
		}
		tokens = append(tokens, &tok)
	}
	return &token.IssuedTokens{Tokens: tokens}, rows.Err()
}

func (db *TokenDB) GetTokenOutputs(ids []*token.ID, callback tdriver.QueryCallbackFunc) error {
	tokens, err := db.getLedgerToken(ids)
	if err != nil {
		return err
	}
	for i := 0; i < len(ids); i++ {
		if err := callback(ids[i], tokens[i]); err != nil {
			return err
		}
	}
	return nil
}

// GetTokenInfos retrieves the token metadata for the passed ids.
// For each id, the callback is invoked to unmarshal the token metadata
func (db *TokenDB) GetTokenInfos(ids []*token.ID) ([][]byte, error) {
	return db.GetAllTokenInfos(ids)
}

// GetTokenInfoAndOutputs retrieves both the token output and information for the passed ids.
func (db *TokenDB) GetTokenInfoAndOutputs(ctx context.Context, ids []*token.ID) ([][]byte, [][]byte, error) {
	span := trace.SpanFromContext(ctx)
	span.AddEvent("get_ledger_token_meta")
	tokens, metas, err := db.getLedgerTokenAndMeta(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	span.AddEvent("create_outputs")
	return tokens, metas, nil
}

// GetAllTokenInfos retrieves the token information for the passed ids.
func (db *TokenDB) GetAllTokenInfos(ids []*token.ID) ([][]byte, error) {
	if len(ids) == 0 {
		return [][]byte{}, nil
	}
	_, metas, err := db.getLedgerTokenAndMeta(context.TODO(), ids)
	return metas, err
}

func (db *TokenDB) getLedgerToken(ids []*token.ID) ([][]byte, error) {
	logger.Debugf("retrieve ledger tokens for [%s]", ids)
	if len(ids) == 0 {
		return [][]byte{}, nil
	}
	where, args := common.Where(db.ci.HasTokens("tx_id", "idx", ids...))

	query := fmt.Sprintf("SELECT tx_id, idx, ledger FROM %s %s", db.table.Tokens, where)
	logger.Debug(query, args)
	rows, err := db.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tokenMap := make(map[string][]byte, len(ids))
	for rows.Next() {
		var tok []byte
		var id token.ID
		if err := rows.Scan(&id.TxId, &id.Index, &tok); err != nil {
			return nil, err
		}
		tokenMap[id.String()] = tok
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	logger.Debugf("retrieve ledger tokens for [%s], retrieved [%d] tokens", ids, len(tokenMap))

	tokens := make([][]byte, len(ids))
	for i, id := range ids {
		if tok, ok := tokenMap[id.String()]; !ok || tok == nil {
			return nil, errors.Errorf("token not found for key [%s]", id)
		} else if len(tok) == 0 {
			return nil, errors.Errorf("empty token found for key [%s]", id)
		} else {
			tokens[i] = tok
		}
	}
	return tokens, nil
}

func (db *TokenDB) getLedgerTokenAndMeta(ctx context.Context, ids []*token.ID) ([][]byte, [][]byte, error) {
	span := trace.SpanFromContext(ctx)
	if len(ids) == 0 {
		return [][]byte{}, [][]byte{}, nil
	}
	where, args := common.Where(db.ci.HasTokens("tx_id", "idx", ids...))

	query := fmt.Sprintf("SELECT tx_id, idx, ledger, ledger_metadata FROM %s %s", db.table.Tokens, where)
	span.AddEvent("query", tracing.WithAttributes(tracing.String(QueryLabel, query)))
	logger.Debug(query, args)
	rows, err := db.db.Query(query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	span.AddEvent("start_scan_rows")
	infoMap := make(map[string][2][]byte, len(ids))
	for rows.Next() {
		var tok []byte
		var metadata []byte
		var id token.ID
		if err := rows.Scan(&id.TxId, &id.Index, &tok, &metadata); err != nil {
			return nil, nil, err
		}
		infoMap[id.String()] = [2][]byte{tok, metadata}
	}
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}
	span.AddEvent("end_scan_rows", tracing.WithAttributes(tracing.Int(ResultRowsLabel, len(ids))))

	span.AddEvent("combine_results")
	tokens := make([][]byte, len(ids))
	metas := make([][]byte, len(ids))
	for i, id := range ids {
		if info, ok := infoMap[id.String()]; !ok {
			return nil, nil, errors.Errorf("token/metadata not found for [%s]", id)
		} else {
			tokens[i] = info[0]
			metas[i] = info[1]
		}
	}
	return tokens, metas, nil
}

// GetTokens returns the owned tokens and their identifier keys for the passed ids.
func (db *TokenDB) GetTokens(inputs ...*token.ID) ([]*token.Token, error) {
	if len(inputs) == 0 {
		return []*token.Token{}, nil
	}
	where, args := common.Where(db.ci.And(
		db.ci.HasTokens("tx_id", "idx", inputs...),
		common.ConstCondition("is_deleted = false"),
		common.ConstCondition("owner = true"),
	))

	query := fmt.Sprintf("SELECT tx_id, idx, owner_raw, token_type, quantity FROM %s %s", db.table.Tokens, where)
	logger.Debug(query, args)
	rows, err := db.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tokens := make([]*token.Token, len(inputs))
	counter := 0
	for rows.Next() {
		tokID := token.ID{}
		var typ, quantity string
		var ownerRaw []byte
		err := rows.Scan(
			&tokID.TxId,
			&tokID.Index,
			&ownerRaw,
			&typ,
			&quantity,
		)
		if err != nil {
			return tokens, err
		}
		tok := &token.Token{
			Owner:    &token.Owner{Raw: ownerRaw},
			Type:     typ,
			Quantity: quantity,
		}

		// put in the right position
		found := false
		for j := 0; j < len(inputs); j++ {
			if inputs[j].Equal(tokID) {
				tokens[j] = tok
				logger.Debugf("set token at location [%s:%s]-[%d]", tok.Type, tok.Quantity, j)
				found = true
				break
			}
		}
		if !found {
			return nil, errors.Errorf("retrieved wrong token [%v]", tokID)
		}

		counter++
	}
	logger.Debugf("found [%d] tokens, expected [%d]", counter, len(inputs))
	if err = rows.Err(); err != nil {
		return tokens, err
	}
	if counter == 0 {
		return nil, errors.Errorf("token not found for key [%s:%d]", inputs[0].TxId, inputs[0].Index)
	}
	if counter != len(inputs) {
		for j, t := range tokens {
			if t == nil {
				return nil, errors.Errorf("token not found for key [%s:%d]", inputs[j].TxId, inputs[j].Index)
			}
		}
		panic("programming error: should not reach this point")
	}
	return tokens, nil
}

// QueryTokenDetails returns details about owned tokens, regardless if they have been spent or not.
// Filters work cumulatively and may be left empty. If a token is owned by two enrollmentIDs and there
// is no filter on enrollmentID, the token will be returned twice (once for each owner).
func (db *TokenDB) QueryTokenDetails(params driver.QueryTokenDetailsParams) ([]driver.TokenDetails, error) {
	where, args := common.Where(db.ci.HasTokenDetails(params, db.table.Tokens))
	join := joinOnTokenID(db.table.Tokens, db.table.Ownership)

	query := fmt.Sprintf("SELECT %s.tx_id, %s.idx, owner_identity, owner_type, wallet_id, token_type, amount, is_deleted, spent_by, stored_at FROM %s %s %s",
		db.table.Tokens, db.table.Tokens, db.table.Tokens, join, where)
	logger.Debug(query, args)
	rows, err := db.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	deets := []driver.TokenDetails{}
	for rows.Next() {
		td := driver.TokenDetails{}
		if err := rows.Scan(
			&td.TxID,
			&td.Index,
			&td.OwnerIdentity,
			&td.OwnerType,
			&td.OwnerEnrollment,
			&td.Type,
			&td.Amount,
			&td.IsSpent,
			&td.SpentBy,
			&td.StoredAt,
		); err != nil {
			return deets, err
		}
		deets = append(deets, td)
	}
	logger.Debugf("found [%d] tokens", len(deets))
	if err = rows.Err(); err != nil {
		return deets, err
	}
	return deets, nil
}

// WhoDeletedTokens returns information about which transaction deleted the passed tokens.
// The bool array is an indicator used to tell if the token at a given position has been deleted or not
func (db *TokenDB) WhoDeletedTokens(inputs ...*token.ID) ([]string, []bool, error) {
	if len(inputs) == 0 {
		return []string{}, []bool{}, nil
	}
	where, args := common.Where(db.ci.HasTokens("tx_id", "idx", inputs...))

	query := fmt.Sprintf("SELECT tx_id, idx, spent_by, is_deleted FROM %s %s", db.table.Tokens, where)
	logger.Debug(query, args)
	rows, err := db.db.Query(query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	spentBy := make([]string, len(inputs))
	isSpent := make([]bool, len(inputs))
	found := make([]bool, len(inputs))

	counter := 0
	for rows.Next() {
		var txid string
		var idx uint64
		var spBy string
		var isSp bool
		if err := rows.Scan(&txid, &idx, &spBy, &isSp); err != nil {
			return spentBy, isSpent, err
		}
		// order is not necessarily the same, so we have to set it in a loop
		for i, inp := range inputs {
			if inp.TxId == txid && inp.Index == idx {
				isSpent[i] = isSp
				spentBy[i] = spBy
				found[i] = true
				break // stop searching for this id but continue looping over rows
			}
		}
		counter++
	}
	logger.Debugf("found [%d] records, expected [%d]", counter, len(inputs))
	if err = rows.Err(); err != nil {
		return nil, isSpent, err
	}
	if counter == 0 {
		return nil, nil, errors.Errorf("token not found for key [%s:%d]", inputs[0].TxId, inputs[0].Index)
	}
	if counter != len(inputs) {
		for j, f := range found {
			if !f {
				return nil, nil, errors.Errorf("token not found for key [%s:%d]", inputs[j].TxId, inputs[j].Index)
			}
		}
		panic("programming error: should not reach this point")
	}
	return spentBy, isSpent, nil
}

func (db *TokenDB) TransactionExists(ctx context.Context, id string) (bool, error) {
	span := trace.SpanFromContext(ctx)
	query := fmt.Sprintf("SELECT tx_id FROM %s WHERE tx_id=$1 LIMIT 1;", db.table.Tokens)
	logger.Debug(query, id)

	span.AddEvent("query", trace.WithAttributes(tracing.String(QueryLabel, query)))
	row := db.db.QueryRow(query, id)
	var found string
	span.AddEvent("scan_rows")
	if err := row.Scan(&found); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		logger.Warnf("tried to check transaction existence for id %s, err %s", id, err)
		return false, err
	}
	return true, nil
}

func (db *TokenDB) StorePublicParams(raw []byte) error {
	now := time.Now().UTC()
	query := fmt.Sprintf("INSERT INTO %s (raw, stored_at) VALUES ($1, $2)", db.table.PublicParams)
	logger.Debug(query, fmt.Sprintf("store public parameters (%d bytes), %v", len(raw), now))

	_, err := db.db.Exec(query, raw, now)
	return err
}

func (db *TokenDB) PublicParams() ([]byte, error) {
	var params []byte
	query := fmt.Sprintf("SELECT raw FROM %s ORDER BY stored_at DESC LIMIT 1;", db.table.PublicParams)
	logger.Debug(query)

	row := db.db.QueryRow(query)
	err := row.Scan(&params)
	if err != nil {
		if errors.HasCause(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, errors.Wrapf(err, "error querying db")
	}
	return params, nil
}

func (db *TokenDB) StoreCertifications(certifications map[*token.ID][]byte) (err error) {
	now := time.Now().UTC()
	query := fmt.Sprintf("INSERT INTO %s (tx_id, idx, certification, stored_at) VALUES ($1, $2, $3, $4)", db.table.Certifications)

	tx, err := db.db.Begin()
	if err != nil {
		return errors.Errorf("failed starting a transaction")
	}
	defer func() {
		if err != nil && tx != nil {
			if err := tx.Rollback(); err != nil {
				logger.Errorf("failed to rollback [%s][%s]", err, debug.Stack())
			}
		}
	}()

	for tokenID, certification := range certifications {
		if tokenID == nil {
			return errors.Errorf("invalid token-id, cannot be nil")
		}
		logger.Debug(query, fmt.Sprintf("(%d bytes)", len(certification)), now)
		if _, err = tx.Exec(query, tokenID.TxId, tokenID.Index, certification, now); err != nil {
			return tokenDBError(err)
		}
	}
	if err = tx.Commit(); err != nil {
		return errors.Wrap(err, "failed committing certifications")
	}
	return
}

func (db *TokenDB) ExistsCertification(tokenID *token.ID) bool {
	if tokenID == nil {
		return false
	}
	where, args := common.Where(db.ci.HasTokens("tx_id", "idx", tokenID))

	query := fmt.Sprintf("SELECT certification FROM %s %s", db.table.Certifications, where)
	logger.Debug(query, args)
	row := db.db.QueryRow(query, args...)

	var certification []byte
	if err := row.Scan(&certification); err != nil {
		if errors.HasCause(err, sql.ErrNoRows) {
			return false
		}
		logger.Warnf("tried to check certification existence for token id %s, err %s", tokenID, err)
		return false
	}
	result := len(certification) != 0
	if !result {
		logger.Warnf("tried to check certification existence for token id %s, got an empty certification", tokenID)
	}
	return result
}

func (db *TokenDB) GetCertifications(ids []*token.ID) ([][]byte, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	where, args := common.Where(db.ci.HasTokens("tx_id", "idx", ids...))
	query := fmt.Sprintf("SELECT tx_id, idx, certification FROM %s %s ", db.table.Certifications, where)

	rows, err := db.db.Query(query, args...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to query")
	}
	defer rows.Close()

	certificationMap := make(map[string][]byte, len(ids))
	for rows.Next() {
		var certification []byte
		var id token.ID
		if err := rows.Scan(&id.TxId, &id.Index, &certification); err != nil {
			return nil, err
		}
		certificationMap[id.String()] = certification
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}

	certifications := make([][]byte, len(ids))
	for i, id := range ids {
		if cert, ok := certificationMap[id.String()]; !ok {
			return nil, errors.Errorf("token %s was not certified", id)
		} else if len(cert) == 0 {
			return nil, errors.Errorf("empty certification for [%s]", id)
		} else {
			certifications[i] = cert
		}
	}
	return certifications, nil
}

func (db *TokenDB) GetSchema() string {
	return fmt.Sprintf(`
		-- Tokens
		CREATE TABLE IF NOT EXISTS %s (
			tx_id TEXT NOT NULL,
			idx INT NOT NULL,
			amount BIGINT NOT NULL,
			token_type TEXT NOT NULL,
			quantity TEXT NOT NULL,
			issuer_raw BYTEA,
			owner_raw BYTEA NOT NULL,
			owner_type TEXT NOT NULL,
			owner_identity BYTEA NOT NULL,
			owner_wallet_id TEXT, 
			ledger BYTEA NOT NULL,
			ledger_metadata BYTEA NOT NULL,
			stored_at TIMESTAMP NOT NULL,
			is_deleted BOOL NOT NULL DEFAULT false,
			spent_by TEXT NOT NULL DEFAULT '',
			spent_at TIMESTAMP,
			owner BOOL NOT NULL DEFAULT false,
			auditor BOOL NOT NULL DEFAULT false,
			issuer BOOL NOT NULL DEFAULT false,
			PRIMARY KEY (tx_id, idx)
		);
		CREATE INDEX IF NOT EXISTS idx_spent_%s ON %s ( is_deleted, owner );
		CREATE INDEX IF NOT EXISTS idx_tx_id_%s ON %s ( tx_id );

		-- Ownership
		CREATE TABLE IF NOT EXISTS %s (
			tx_id TEXT NOT NULL,
			idx INT NOT NULL,
			wallet_id TEXT NOT NULL,
			PRIMARY KEY (tx_id, idx, wallet_id),
			FOREIGN KEY (tx_id, idx) REFERENCES %s
		);

		-- Public Parameters
		CREATE TABLE IF NOT EXISTS %s (
			raw BYTEA NOT NULL,
			stored_at TIMESTAMP NOT NULL PRIMARY KEY
		);

		-- Certifications
		CREATE TABLE IF NOT EXISTS %s (
			tx_id TEXT NOT NULL,
			idx INT NOT NULL,
			certification BYTEA NOT NULL,
			stored_at TIMESTAMP NOT NULL,
			PRIMARY KEY (tx_id, idx),
			FOREIGN KEY (tx_id, idx) REFERENCES %s
		);
		`,
		db.table.Tokens,
		db.table.Tokens, db.table.Tokens,
		db.table.Tokens, db.table.Tokens,
		db.table.Ownership, db.table.Tokens,
		db.table.PublicParams,
		db.table.Certifications, db.table.Tokens,
	)
}

func (db *TokenDB) Close() {
	db.db.Close()
}

func (db *TokenDB) NewTokenDBTransaction(ctx context.Context) (driver.TokenDBTransaction, error) {
	span := trace.SpanFromContext(ctx)
	span.AddEvent("start_begin_tx")
	tx, err := db.db.Begin()
	span.AddEvent("end_begin_tx")
	if err != nil {
		return nil, errors.Errorf("failed starting a db transaction")
	}
	return &TokenTransaction{db: db, tx: tx}, nil
}

type TokenTransaction struct {
	db *TokenDB
	tx *sql.Tx
}

func (t *TokenTransaction) GetToken(ctx context.Context, txID string, index uint64, includeDeleted bool) (*token.Token, []string, error) {
	span := trace.SpanFromContext(ctx)
	where, args := common.Where(t.db.ci.HasTokenDetails(driver.QueryTokenDetailsParams{
		IDs:            []*token.ID{{TxId: txID, Index: index}},
		IncludeDeleted: includeDeleted,
	}, t.db.table.Tokens))
	join := joinOnTokenID(t.db.table.Tokens, t.db.table.Ownership)

	query := fmt.Sprintf("SELECT owner_raw, token_type, quantity, %s.wallet_id, owner_wallet_id FROM %s %s %s", t.db.table.Ownership, t.db.table.Tokens, join, where)
	span.AddEvent("query", tracing.WithAttributes(tracing.String(QueryLabel, query)))
	logger.Debug(query, args)
	rows, err := t.tx.Query(query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	span.AddEvent("start_scan_rows")
	var raw []byte
	var tokenType string
	var quantity string
	owners := []string{}
	var walletID *string
	for rows.Next() {
		var tempOwner *string
		if err := rows.Scan(&raw, &tokenType, &quantity, &tempOwner, &walletID); err != nil {
			return nil, owners, err
		}
		var owner string
		if tempOwner != nil {
			owner = *tempOwner
		}
		if len(owner) > 0 {
			owners = append(owners, owner)
		}
	}
	if rows.Err() != nil {
		return nil, nil, rows.Err()
	}
	if walletID != nil && len(*walletID) != 0 {
		owners = append(owners, *walletID)
	}
	span.AddEvent("end_scan_rows", tracing.WithAttributes(tracing.Int(ResultRowsLabel, len(owners))))
	if len(raw) == 0 {
		return nil, owners, nil
	}
	return &token.Token{
		Owner: &token.Owner{
			Raw: raw,
		},
		Type:     tokenType,
		Quantity: quantity,
	}, owners, nil
}

func (t *TokenTransaction) Delete(ctx context.Context, txID string, index uint64, deletedBy string) error {
	span := trace.SpanFromContext(ctx)
	//logger.Debugf("delete token [%s:%d:%s]", txID, index, deletedBy)
	// We don't delete audit tokens, and we keep the 'ownership' relation.
	now := time.Now().UTC()
	query := fmt.Sprintf("UPDATE %s SET is_deleted = true, spent_by = $1, spent_at = $2 WHERE tx_id = $3 AND idx = $4;", t.db.table.Tokens)
	logger.Infof(query, deletedBy, now, txID, index)
	span.AddEvent("query", tracing.WithAttributes(tracing.String(QueryLabel, query)))
	if _, err := t.tx.Exec(query, deletedBy, now, txID, index); err != nil {
		span.RecordError(err)
		return errors.Wrapf(err, "error setting token to deleted [%s]", txID)
	}
	span.AddEvent("end_query")
	return nil
}

func (t *TokenTransaction) StoreToken(ctx context.Context, tr driver.TokenRecord, owners []string) error {
	if len(tr.OwnerWalletID) == 0 && len(owners) == 0 && tr.Owner {
		return errors.Errorf("no owners specified [%s]", string(debug.Stack()))
	}

	span := trace.SpanFromContext(ctx)
	//logger.Debugf("store record [%s:%d,%v] in table [%s]", tr.TxID, tr.Index, owners, t.db.table.Tokens)

	// Store token
	now := time.Now().UTC()
	query := fmt.Sprintf("INSERT INTO %s (tx_id, idx, issuer_raw, owner_raw, owner_type, owner_identity, owner_wallet_id, ledger, ledger_metadata, token_type, quantity, amount, stored_at, owner, auditor, issuer) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)", t.db.table.Tokens)
	logger.Debug(query,
		tr.TxID,
		tr.Index,
		len(tr.IssuerRaw),
		len(tr.OwnerRaw),
		tr.OwnerType,
		len(tr.OwnerIdentity),
		tr.OwnerWalletID,
		len(tr.Ledger),
		len(tr.LedgerMetadata),
		tr.Type,
		tr.Quantity,
		tr.Amount,
		now,
		tr.Owner,
		tr.Auditor,
		tr.Issuer)
	span.AddEvent("query", tracing.WithAttributes(tracing.String(QueryLabel, query)))
	if _, err := t.tx.Exec(query,
		tr.TxID,
		tr.Index,
		tr.IssuerRaw,
		tr.OwnerRaw,
		tr.OwnerType,
		tr.OwnerIdentity,
		tr.OwnerWalletID,
		tr.Ledger,
		tr.LedgerMetadata,
		tr.Type,
		tr.Quantity,
		tr.Amount,
		now,
		tr.Owner,
		tr.Auditor,
		tr.Issuer); err != nil {
		logger.Errorf("error storing token [%s] in table [%s]: [%s][%s]", tr.TxID, t.db.table.Tokens, err, string(debug.Stack()))
		return errors.Wrapf(err, "error storing token [%s] in table [%s]", tr.TxID, t.db.table.Tokens)
	}

	// Store ownership
	span.AddEvent("store_ownerships")
	for _, eid := range owners {
		query = fmt.Sprintf("INSERT INTO %s (tx_id, idx, wallet_id) VALUES ($1, $2, $3)", t.db.table.Ownership)
		logger.Debug(query, tr.TxID, tr.Index, eid)
		span.AddEvent("query", tracing.WithAttributes(tracing.String(QueryLabel, query)))
		if _, err := t.tx.Exec(query, tr.TxID, tr.Index, eid); err != nil {
			return errors.Wrapf(err, "error storing token ownership [%s]", tr.TxID)
		}
	}

	return nil
}

func (t *TokenTransaction) Commit() error {
	return t.tx.Commit()
}

func (t *TokenTransaction) Rollback() error {
	return t.tx.Rollback()
}

type UnspentTokensInWalletIterator struct {
	txs *sql.Rows
}

func (u *UnspentTokensInWalletIterator) Close() {
	u.txs.Close()
}

func (u *UnspentTokensInWalletIterator) Next() (*token.UnspentTokenInWallet, error) {
	if !u.txs.Next() {
		return nil, nil
	}

	tok := &token.UnspentTokenInWallet{
		Id:       &token.ID{},
		WalletID: "",
		Type:     "",
		Quantity: "",
	}
	if err := u.txs.Scan(&tok.Id.TxId, &tok.Id.Index, &tok.Type, &tok.Quantity, &tok.WalletID); err != nil {
		return nil, err
	}
	return tok, nil
}

type UnspentTokensIterator struct {
	txs *sql.Rows
}

func (u *UnspentTokensIterator) Close() {
	u.txs.Close()
}

func (u *UnspentTokensIterator) Next() (*token.UnspentToken, error) {
	if !u.txs.Next() {
		return nil, nil
	}

	var typ, quantity string
	var owner []byte
	var id token.ID
	// tx_id, idx, owner_raw, token_type, quantity
	err := u.txs.Scan(
		&id.TxId,
		&id.Index,
		&owner,
		&typ,
		&quantity,
	)
	if err != nil {
		return nil, err
	}
	return &token.UnspentToken{
		Id: &id,
		Owner: &token.Owner{
			Raw: owner,
		},
		Type:     typ,
		Quantity: quantity,
	}, err
}

func tokenDBError(err error) error {
	if err == nil {
		return nil
	}
	logger.Error(err)
	e := strings.ToLower(err.Error())
	if strings.Contains(e, "foreign key constraint") {
		return driver.ErrTokenDoesNotExist
	}
	return err
}
