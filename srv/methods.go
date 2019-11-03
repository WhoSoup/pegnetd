// MIT License
//
// Copyright 2018 Canonical Ledgers, LLC
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to
// deal in the Software without restriction, including without limitation the
// rights to use, copy, modify, merge, publish, distribute, sublicense, and/or
// sell copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING
// FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS
// IN THE SOFTWARE.

package srv

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"sort"

	"github.com/pegnet/pegnet/modules/conversions"

	"github.com/AdamSLevy/jsonrpc2"
	jrpc "github.com/AdamSLevy/jsonrpc2/v11"
	"github.com/Factom-Asset-Tokens/factom"
	"github.com/pegnet/pegnetd/config"
	"github.com/pegnet/pegnetd/fat/fat2"
	"github.com/pegnet/pegnetd/node"
	"github.com/pegnet/pegnetd/node/pegnet"
)

func (s *APIServer) jrpcMethods() jrpc.MethodMap {
	return jrpc.MethodMap{
		"get-rich-list":          s.getRichList,
		"get-transactions":       s.getTransactions,
		"get-transaction-status": s.getTransactionStatus,
		"get-transaction":        s.getTransaction(false),
		"get-transaction-entry":  s.getTransaction(true),
		"get-pegnet-balances":    s.getPegnetBalances,
		"get-pegnet-issuance":    s.getPegnetIssuance,

		"send-transaction": s.sendTransaction,

		"get-sync-status": s.getSyncStatus,

		"get-pegnet-rates": s.getPegnetRates,
	}

}

type ResultGetRichList struct {
	Height uint32      `json:"height"`
	Top100 []RichEntry `json:"top100"`
}
type RichEntry struct {
	Address  *factom.FAAddress `json:"address"`
	USDEquiv float64           `json:"usdequiv"`
}

func (s *APIServer) getRichList(data json.RawMessage) interface{} {
	height := s.Node.GetCurrentSync()

	var res ResultGetRichList
	res.Height = height

	addrs, err := s.Node.Pegnet.SelectAddresses()
	if err != nil {
		return err
	}
	rates, err := s.Node.Pegnet.SelectRates(nil, height)
	if err != nil {
		return rates
	}

	for _, a := range addrs {
		var usd int64
		balance, err := s.Node.Pegnet.SelectBalances(a)
		if err != nil {
			return err
		}

		for k, v := range balance {
			tmp, _ := conversions.Convert(int64(v), rates[k], rates[fat2.PTickerUSD])
			usd += tmp
		}

		if usd == 0 {
			continue
		}

		res.Top100 = append(res.Top100, RichEntry{Address: a, USDEquiv: float64(usd) / 1e8})
	}

	sort.Slice(res.Top100, func(i, j int) bool {
		return res.Top100[i].USDEquiv > res.Top100[j].USDEquiv
	})

	if len(res.Top100) > 100 {
		res.Top100 = res.Top100[:100]
	}

	return res
}

type ResultGetTransactionStatus struct {
	Height   uint32 `json:"height"`
	Executed uint32 `json:"executed"`
}

func (s *APIServer) getTransactionStatus(data json.RawMessage) interface{} {
	params := ParamsGetPegnetTransactionStatus{}
	_, _, err := validate(data, &params)
	if err != nil {
		return err
	}

	height, executed, err := s.Node.Pegnet.SelectTransactionHistoryStatus(params.Hash)
	if err != nil {
		return jrpc.InvalidParams(err.Error())
	}

	if height == 0 {
		return ErrorTransactionNotFound
	}

	var res ResultGetTransactionStatus
	res.Height = height
	res.Executed = executed

	return res
}

// ResultGetTransactions returns history entries.
// `Actions` contains []pegnet.HistoryTransaction.
// `Count` is the total number of possible transactions
// `NextOffset` returns the offset to use to get the next set of records.
//  0 means no more records available
type ResultGetTransactions struct {
	Actions    interface{} `json:"actions"`
	Count      int         `json:"count"`
	NextOffset int         `json:"nextoffset"`
}

func (s *APIServer) getTransactions(data json.RawMessage) interface{} {
	params := ParamsGetPegnetTransaction{}
	_, _, err := validate(data, &params)
	if err != nil {
		return err
	}

	// using a separate options struct due to golang's circular import restrictions
	var options pegnet.HistoryQueryOptions
	options.Offset = params.Offset
	options.Desc = params.Desc
	options.Transfer = params.Transfer
	options.Conversion = params.Conversion
	options.Coinbase = params.Coinbase
	options.FCTBurn = params.Burn

	var actions []pegnet.HistoryTransaction
	var count int

	if params.Hash != nil {
		actions, count, err = s.Node.Pegnet.SelectTransactionHistoryActionsByHash(params.Hash, options)
	} else if params.Address != "" {
		addr, _ := factom.NewFAAddress(params.Address) // verified in param
		actions, count, err = s.Node.Pegnet.SelectTransactionHistoryActionsByAddress(&addr, options)
	} else {
		actions, count, err = s.Node.Pegnet.SelectTransactionHistoryActionsByHeight(uint32(params.Height), options)
	}

	if err != nil {
		return jrpc.InvalidParams(err.Error())
	}

	if len(actions) == 0 {
		return ErrorTransactionNotFound
	}

	var res ResultGetTransactions
	res.Count = count
	if params.Offset+len(actions) < count {
		res.NextOffset = params.Offset + len(actions)
	}
	res.Actions = actions

	return res
}

type ResultGetTransaction struct {
	Hash      *factom.Bytes32 `json:"entryhash"`
	Timestamp int64           `json:"timestamp"`
	Tx        interface{}     `json:"actions"`
}

func (s *APIServer) getTransaction(getEntry bool) jrpc.MethodFunc {
	return func(data json.RawMessage) interface{} {
		params := ParamsGetTransaction{}
		_, _, err := validate(data, &params)
		if err != nil {
			return err
		}

		found, err := s.Node.Pegnet.DoesTransactionExist(*params.Hash)
		if err != nil {
			return err
		}

		if !found {
			return ErrorTransactionNotFound
		}

		var e factom.Entry
		e.Hash = params.Hash
		err = e.Get(s.Node.FactomClient)
		if err != nil {
			return jsonrpc2.InternalError
		}

		if !e.IsPopulated() {
			return ErrorTransactionNotFound
		}

		var res ResultGetTransaction
		res.Hash = e.Hash
		// TODO: Save timestamp? We'd have to save it to the db
		//res.Timestamp = e.Timestamp.Unix()
		// TODO: Fill out the txid field
		//res.TxIndex

		if getEntry {
			return e
		}

		txBatch := fat2.NewTransactionBatch(e)
		err = txBatch.UnmarshalEntry()
		if err != nil {
			return jsonrpc2.InternalError
		}

		res.Tx = txBatch
		return res
	}
}

// TODO: This is incompatible with FAT.
type ResultPegnetTickerMap map[fat2.PTicker]uint64

func (r ResultPegnetTickerMap) MarshalJSON() ([]byte, error) {
	strMap := make(map[string]uint64, len(r))
	for ticker, balance := range r {
		strMap[ticker.String()] = balance
	}
	return json.Marshal(strMap)
}
func (r *ResultPegnetTickerMap) UnmarshalJSON(data []byte) error {
	var strMap map[string]uint64
	if err := json.Unmarshal(data, &strMap); err != nil {
		return err
	}
	*r = make(map[fat2.PTicker]uint64, len(strMap))
	for str, balance := range strMap {
		ticker := new(fat2.PTicker)
		if err := ticker.UnmarshalJSON([]byte(str)); err != nil {
			return err
		}
		//if err := fat2.PTicker.UnmarshalJSON(&ticker, []byte(str)); err != nil {
		//	return err
		//}
		(*r)[*ticker] = balance
	}
	return nil
}

func (s *APIServer) getPegnetBalances(data json.RawMessage) interface{} {
	params := ParamsGetPegnetBalances{}
	if _, _, err := validate(data, &params); err != nil {
		return err
	}
	bals, err := s.Node.Pegnet.SelectBalances(params.Address)
	if err == sql.ErrNoRows {
		return ErrorAddressNotFound
	}
	if err != nil {
		return jsonrpc2.InternalError
	}
	return ResultPegnetTickerMap(bals)
}

type ResultGetIssuance struct {
	SyncStatus ResultGetSyncStatus   `json:"syncstatus"`
	Issuance   ResultPegnetTickerMap `json:"issuance"`
}

func (s *APIServer) getPegnetIssuance(data json.RawMessage) interface{} {
	issuance, err := s.Node.Pegnet.SelectIssuances()
	if err == sql.ErrNoRows {
		return ErrorAddressNotFound
	}
	if err != nil {
		return jsonrpc2.InternalError
	}

	syncStatus := s.getSyncStatus(nil)
	return ResultGetIssuance{
		SyncStatus: syncStatus.(ResultGetSyncStatus),
		Issuance:   issuance,
	}
}

func (s *APIServer) getPegnetRates(data json.RawMessage) interface{} {
	params := ParamsGetPegnetRates{}
	if _, _, err := validate(data, &params); err != nil {
		return err
	}
	rates, err := s.Node.Pegnet.SelectRates(context.Background(), *params.Height)
	if err == sql.ErrNoRows || rates == nil || len(rates) == 0 {
		return ErrorNotFound
	}
	if err != nil {
		return jsonrpc2.InternalError
	}

	// The balance results actually works for rates too
	return ResultPegnetTickerMap(rates)
}

func (s *APIServer) sendTransaction(data json.RawMessage) interface{} {
	params := ParamsSendTransaction{}
	_, _, err := validate(data, &params)
	if err != nil {
		return err
	}
	// defer put()

	ecPrivateKeyString := s.Config.GetString(config.ECPrivateKey)
	var ecPrivateKey factom.EsAddress
	if err = ecPrivateKey.Set(ecPrivateKeyString); err != nil {
		return jsonrpc2.InternalError
	}

	entry := params.Entry()
	entry.ChainID = &node.TransactionChain
	// TODO: attempt to apply
	//txErr, err := attemptApplyFAT2TxBatch(chain, entry)
	//if err != nil {
	//	panic(err)
	//}
	//if txErr != nil {
	//	err := ErrorInvalidTransaction
	//	err.Data = txErr.Error()
	//	return err
	//}

	var txID *factom.Bytes32
	if !params.DryRun {
		balance, err := ecPrivateKey.ECAddress().GetBalance(s.Node.FactomClient)
		if err != nil {
			panic(err)
		}
		cost, err := entry.Cost(false)
		if err != nil {
			rerr := ErrorInvalidTransaction
			rerr.Data = err.Error()
			return rerr
		}
		if balance < uint64(cost) {
			return ErrorNoEC
		}
		txID, err = entry.ComposeCreate(s.Node.FactomClient, ecPrivateKey, false)
		if err != nil {
			panic(err)
		}
	}

	return struct {
		ChainID *factom.Bytes32 `json:"chainid"`
		TxID    *factom.Bytes32 `json:"txid,omitempty"`
		Hash    *factom.Bytes32 `json:"entryhash"`
	}{ChainID: entry.ChainID, TxID: txID, Hash: entry.Hash}
	return nil
}

//func attemptApplyFAT2TxBatch(chain *engine.Chain, e factom.Entry) (txErr, err error) {
//	txBatch := fat2.NewTransactionBatch(e)
//	if txErr = txBatch.Validate(); txErr != nil {
//		return
//	}
//	// TODO: check this entry never been put in chain before
//	//valid, err := entries.CheckUniquelyValid(chain.Conn, 0, e.Hash)
//	//if err != nil {
//	//	return
//	//}
//	//if !valid {
//	//	txErr = fmt.Errorf("replay: hash previously marked valid")
//	//	return
//	//}
//
//	// TODO: Check all input balances
//
//	return
//}

type ResultGetSyncStatus struct {
	Sync    uint32 `json:"syncheight"`
	Current int32  `json:"factomheight"`
}

func (s *APIServer) getSyncStatus(data json.RawMessage) interface{} {
	heights := new(factom.Heights)
	err := heights.Get(s.Node.FactomClient)
	if err != nil {
		return ResultGetSyncStatus{Sync: s.Node.GetCurrentSync(), Current: -1}
	}
	return ResultGetSyncStatus{Sync: s.Node.GetCurrentSync(), Current: int32(heights.DirectoryBlock)}
}

// TODO: Re-eval this function. The chain data that is supplied needs to be reimplemented
//		return was (*engine.Chain, func(), error)
func validate(data json.RawMessage, params Params) (interface{}, func(), error) {
	if params == nil {
		if len(data) > 0 {
			return nil, nil, jrpc.InvalidParams(`no "params" accepted`)
		}
		return nil, nil, nil
	}
	if len(data) == 0 {
		return nil, nil, params.IsValid()
	}
	if err := unmarshalStrict(data, params); err != nil {
		return nil, nil, jrpc.InvalidParams(err.Error())
	}
	if err := params.IsValid(); err != nil {
		return nil, nil, err
	}
	//if params.HasIncludePending() && flag.DisablePending {
	//	return nil, nil, ErrorPendingDisabled
	//}
	chainID := params.ValidChainID()
	if chainID != nil {
		if *chainID != node.TransactionChain {
			return nil, nil, ErrorTokenNotFound
		}
		// TODO: Do we need to stub out any of the chain fields?
		//chain := engine.Chains.Get(chainID)
		//if !chain.IsIssued() {
		//	return nil, nil, ErrorTokenNotFound
		//}
		//if params.HasIncludePending() {
		//	chain.ApplyPending()
		//}
		//conn, put := chain.Get()
		//chain.Conn = conn
		//return &chain, put, nil
	}

	// If there is no chain, then we can't really validate it since we aren't fatd.
	// The chainid is just to be compatible, but in reality it means nothing to us.
	return nil, nil, nil
}

func unmarshalStrict(data []byte, v interface{}) error {
	b := bytes.NewBuffer(data)
	d := json.NewDecoder(b)
	d.DisallowUnknownFields()
	return d.Decode(v)
}
