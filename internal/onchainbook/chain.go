package onchainbook

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/numofx/market-maker/internal/exchange"
)

// The ABIs are trimmed copies of risk-core's compiled artifacts (exchange repo, contracts/risk-core/out), the
// functions this package calls plus every custom error, so a revert is reported by name. Regenerate with
// jq from the artifacts; chain_test.go pins each selector to `cast sig`.
//
// Two book versions: OnchainBookV1.json is the book deployed 2026-11-16 (EURC-30NOV26, pegged splines only);
// OnchainBook.json is v2 (spec C4: quote, self-priced splines), whose SplineUpdate gains `selfPriced` and so whose
// updateSplines selector differs. NewChain detects the version, so the same image runs against either.
//
//go:embed abi/*.json
var abiFiles embed.FS

type abis struct {
	book, bookV1, subAccounts, future, engine, forward, spot abi.ABI
}

func loadABIs() (abis, error) {
	var a abis
	for name, dst := range map[string]*abi.ABI{
		"OnchainBook": &a.book, "OnchainBookV1": &a.bookV1, "SubAccounts": &a.subAccounts, "FutureAsset": &a.future,
		"MarginEngine": &a.engine, "ForwardFeed": &a.forward, "SpotFeed": &a.spot,
	} {
		raw, err := abiFiles.ReadFile("abi/" + name + ".json")
		if err != nil {
			return a, err
		}
		parsed, err := abi.JSON(bytes.NewReader(raw))
		if err != nil {
			return a, fmt.Errorf("%s abi: %w", name, err)
		}
		*dst = parsed
	}
	return a, nil
}

// Market is OnchainBook.markets(id), the fields this package uses.
type Market struct {
	Future        common.Address
	Engine        common.Address
	SubID         *big.Int
	Expiry        uint64
	LastTradeTime time.Time
	TickSize      *big.Int
	LotSize       *big.Int
	MaxTTL        uint32
	Enabled       bool
}

// Feeds are the three prices a cycle reads, resolved from the chain at startup.
type Feeds struct {
	Index     common.Address // the future's index (EurcIndexFeed): what the book's band and peg read
	Forward   common.Address // the future's forward feed: the fair the forward publisher signs
	Reference common.Address // the reference market's spot feed on the engine (Chainlink push)
}

type Chain struct {
	eth         *ethclient.Client
	chainID     *big.Int
	signer      exchange.Signer
	a           abis
	Book        common.Address
	SubAccounts common.Address
	MarketID    *big.Int
	AccountID   *big.Int
	// ReceiptTimeout bounds the wait for each transaction.
	ReceiptTimeout time.Duration
	// V2 is a book with self-priced splines (detected by NewChain); false: v1, pegged only.
	V2 bool
	// HTTPReference, when set, is the divergence guard's reference instead of an engine market's feed: for a market
	// whose engine has no price independent of the index (cNGN). Set by main from MM_BOOK_REFERENCE_URL.
	HTTPReference *HTTPReference
}

func NewChain(ctx context.Context, url string, signer exchange.Signer, book, subAccounts common.Address, marketID, accountID uint64) (*Chain, error) {
	a, err := loadABIs()
	if err != nil {
		return nil, err
	}
	eth, err := ethclient.DialContext(ctx, url)
	if err != nil {
		return nil, err
	}
	chainID, err := eth.ChainID(ctx)
	if err != nil {
		return nil, fmt.Errorf("chain id: %w", err)
	}
	c := &Chain{eth: eth, chainID: chainID, signer: signer, a: a, Book: book, SubAccounts: subAccounts,
		MarketID: new(big.Int).SetUint64(marketID), AccountID: new(big.Int).SetUint64(accountID),
		ReceiptTimeout: 60 * time.Second}
	if c.V2, err = c.detectV2(ctx); err != nil {
		eth.Close()
		return nil, err
	}
	return c, nil
}

// detectV2 calls maxSelfPricedTtl, which only v2 has: v1 reverts with no data (no such function, no fallback).
// Any other failure is an error, not a guess: sending v1 calldata to v2, or v2 to v1, reverts every update.
func (c *Chain) detectV2(ctx context.Context) (bool, error) {
	data, err := c.a.book.Pack("maxSelfPricedTtl", c.MarketID)
	if err != nil {
		return false, err
	}
	out, err := c.eth.CallContract(ctx, ethereum.CallMsg{To: &c.Book, Data: data}, nil)
	if err == nil {
		if len(out) != 32 {
			return false, fmt.Errorf("book.maxSelfPricedTtl returned %d bytes, not one word", len(out))
		}
		return true, nil
	}
	// Only a revert says v1: every JSON-RPC error carries ErrorData, so an empty one alone could be a rate limit.
	var de rpc.DataError
	if strings.Contains(err.Error(), "execution reverted") && (!errors.As(err, &de) || len(hexData(de.ErrorData())) == 0) {
		return false, nil
	}
	return false, fmt.Errorf("book version (maxSelfPricedTtl): %w", c.revertError(err))
}

// SelfPricedTTLCap is the market's maxSelfPricedTtl in seconds: 0 means self-priced splines are off. v1 has none.
func (c *Chain) SelfPricedTTLCap(ctx context.Context) (uint32, error) {
	if !c.V2 {
		return 0, nil
	}
	out, err := c.call(ctx, c.Book, c.a.book, "maxSelfPricedTtl", c.MarketID)
	if err != nil {
		return 0, fmt.Errorf("maxSelfPricedTtl: %w", err)
	}
	return out[0].(uint32), nil
}

func (c *Chain) ChainID() *big.Int { return c.chainID }

// BookSubAccounts is the SubAccounts the book was deployed on: the account checks read the same contract.
func BookSubAccounts(ctx context.Context, url string, book common.Address) (common.Address, error) {
	a, err := loadABIs()
	if err != nil {
		return common.Address{}, err
	}
	eth, err := ethclient.DialContext(ctx, url)
	if err != nil {
		return common.Address{}, err
	}
	defer eth.Close()
	data, _ := a.book.Pack("subAccounts")
	out, err := eth.CallContract(ctx, ethereum.CallMsg{To: &book, Data: data}, nil)
	if err != nil {
		return common.Address{}, fmt.Errorf("book.subAccounts: %w", err)
	}
	res, err := a.book.Unpack("subAccounts", out)
	if err != nil {
		return common.Address{}, fmt.Errorf("book.subAccounts at %s (is it the book?): %w", book.Hex(), err)
	}
	return res[0].(common.Address), nil
}

func (c *Chain) Close() { c.eth.Close() }

func (c *Chain) call(ctx context.Context, to common.Address, a abi.ABI, method string, args ...any) ([]any, error) {
	data, err := a.Pack(method, args...)
	if err != nil {
		return nil, err
	}
	out, err := c.eth.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data}, nil)
	if err != nil {
		return nil, c.revertError(err)
	}
	return a.Unpack(method, out)
}

// ---------------------------------------------------------------------------------------------
// reads
// ---------------------------------------------------------------------------------------------

func (c *Chain) Market(ctx context.Context) (Market, error) {
	// v1's ABI for both versions: v2's markets getter is the same auto getter, 13 words.
	out, err := c.call(ctx, c.Book, c.a.bookV1, "markets", c.MarketID)
	if err != nil {
		return Market{}, fmt.Errorf("markets(%s): %w", c.MarketID, err)
	}
	m := Market{
		Future:        out[0].(common.Address),
		Engine:        out[1].(common.Address),
		SubID:         out[2].(*big.Int),
		Expiry:        out[3].(uint64),
		LastTradeTime: time.Unix(int64(out[4].(uint64)), 0),
		TickSize:      out[6].(*big.Int),
		LotSize:       out[7].(*big.Int),
		MaxTTL:        out[10].(uint32),
		Enabled:       out[12].(bool),
	}
	if m.Future == (common.Address{}) {
		return Market{}, fmt.Errorf("the book has no market %s", c.MarketID)
	}
	return m, nil
}

func (c *Chain) ResolveFeeds(ctx context.Context, m Market, referenceMarketID uint64) (Feeds, error) {
	var f Feeds
	out, err := c.call(ctx, m.Future, c.a.future, "indexFeed")
	if err != nil {
		return f, fmt.Errorf("future.indexFeed: %w", err)
	}
	f.Index = out[0].(common.Address)
	if out, err = c.call(ctx, m.Future, c.a.future, "forwardFeed"); err != nil {
		return f, fmt.Errorf("future.forwardFeed: %w", err)
	}
	f.Forward = out[0].(common.Address)
	if c.HTTPReference != nil {
		if f.Index == (common.Address{}) || f.Forward == (common.Address{}) {
			return f, fmt.Errorf("a feed is unset: %+v", f)
		}
		return f, nil // the reference is the rate service, read in Prices
	}
	if out, err = c.call(ctx, m.Engine, c.a.engine, "getMarketFeeds", new(big.Int).SetUint64(referenceMarketID)); err != nil {
		return f, fmt.Errorf("engine.getMarketFeeds(%d): %w", referenceMarketID, err)
	}
	f.Reference = out[0].(common.Address)
	if f.Index == (common.Address{}) || f.Forward == (common.Address{}) || f.Reference == (common.Address{}) {
		return f, fmt.Errorf("a feed is unset: %+v", f)
	}
	if f.Reference == f.Index {
		return f, errors.New("the reference feed is the index itself: the divergence guard would compare it with itself")
	}
	return f, nil
}

// Prices reads index, forward and reference. A feed that reverts (stale) or fails to answer is nil, with the
// error returned for the log; Decide takes the spline down on any nil.
func (c *Chain) Prices(ctx context.Context, f Feeds, expiry uint64) (index, forward, reference *big.Int, errs []error) {
	spot := func(feed common.Address, what string) *big.Int {
		out, err := c.call(ctx, feed, c.a.spot, "getSpot")
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", what, err))
			return nil
		}
		return out[0].(*big.Int)
	}
	index = spot(f.Index, "index")
	if c.HTTPReference != nil {
		var err error
		if reference, err = c.HTTPReference.Price(ctx); err != nil {
			errs = append(errs, fmt.Errorf("reference (%s): %w", c.HTTPReference.URL, err))
			reference = nil
		}
	} else {
		reference = spot(f.Reference, "reference")
	}
	out, err := c.call(ctx, f.Forward, c.a.forward, "getForwardPrice", expiry)
	if err != nil {
		errs = append(errs, fmt.Errorf("forward: %w", err))
	} else {
		forward = out[0].(*big.Int)
	}
	return index, forward, reference, errs
}

func (c *Chain) Spline(ctx context.Context) (Spline, error) {
	// v1's flat outputs for both versions: v2 returns a struct of the same 12 static fields, so the same bytes
	// (chain_test.go asserts the types agree); selfPriced is read separately.
	out, err := c.call(ctx, c.Book, c.a.bookV1, "splines", c.MarketID, c.AccountID)
	if err != nil {
		return Spline{}, fmt.Errorf("splines: %w", err)
	}
	selfPriced := false
	if c.V2 {
		sp, err := c.call(ctx, c.Book, c.a.book, "isSelfPriced", c.MarketID, c.AccountID)
		if err != nil {
			return Spline{}, fmt.Errorf("isSelfPriced: %w", err)
		}
		selfPriced = sp[0].(bool)
	}
	return Spline{
		OffsetTicks:         out[0].(int32),
		HalfSpreadTicks:     out[1].(uint32),
		BidCurve:            out[2].(uint16),
		AskCurve:            out[3].(uint16),
		SkewTicksPerLot:     out[4].(uint32),
		MaxSkewTicks:        out[5].(uint32),
		RefillLotsPerSecond: out[6].(uint32),
		UpdatedAt:           time.Unix(out[7].(*big.Int).Int64(), 0),
		TTL:                 out[8].(uint32),
		Seq:                 out[9].(uint64),
		Active:              out[10].(bool),
		Owner:               out[11].(common.Address),
		SelfPriced:          selfPriced,
	}, nil
}

func (c *Chain) Curve(ctx context.Context, owner common.Address, id uint16) ([]Region, error) {
	out, err := c.call(ctx, c.Book, c.a.book, "curve", owner, id)
	if err != nil {
		return nil, fmt.Errorf("curve(%s, %d): %w", owner.Hex(), id, err)
	}
	regions, ok := abi.ConvertType(out[0], new([]Region)).(*[]Region)
	if !ok {
		return nil, errors.New("curve: unexpected abi type")
	}
	return *regions, nil
}

func (c *Chain) IsRegistered(ctx context.Context) (bool, error) {
	out, err := c.call(ctx, c.Book, c.a.book, "isRegistered", c.MarketID, c.AccountID)
	if err != nil {
		return false, err
	}
	return out[0].(bool), nil
}

func (c *Chain) MakerAllowed(ctx context.Context, owner common.Address) (bool, error) {
	out, err := c.call(ctx, c.Book, c.a.book, "makerAllowed", owner)
	if err != nil {
		return false, err
	}
	return out[0].(bool), nil
}

func (c *Chain) AccountOwnerAndManager(ctx context.Context) (owner, manager common.Address, err error) {
	out, err := c.call(ctx, c.SubAccounts, c.a.subAccounts, "ownerOf", c.AccountID)
	if err != nil {
		return owner, manager, fmt.Errorf("ownerOf(%s): %w", c.AccountID, err)
	}
	owner = out[0].(common.Address)
	if out, err = c.call(ctx, c.SubAccounts, c.a.subAccounts, "manager", c.AccountID); err != nil {
		return owner, manager, fmt.Errorf("manager(%s): %w", c.AccountID, err)
	}
	return owner, out[0].(common.Address), nil
}

func (c *Chain) Cash(ctx context.Context, engine common.Address) (common.Address, error) {
	out, err := c.call(ctx, engine, c.a.engine, "cashAsset")
	if err != nil {
		return common.Address{}, err
	}
	return out[0].(common.Address), nil
}

// PositionLots is the account's position in the market's series, in lots (negative short), rounded toward zero.
func (c *Chain) PositionLots(ctx context.Context, m Market) (int64, error) {
	out, err := c.call(ctx, c.SubAccounts, c.a.subAccounts, "getBalance", c.AccountID, m.Future, m.SubID)
	if err != nil {
		return 0, fmt.Errorf("getBalance(%s): %w", c.AccountID, err)
	}
	if m.LotSize == nil || m.LotSize.Sign() <= 0 {
		return 0, errors.New("lot size unset")
	}
	lots := new(big.Int).Quo(out[0].(*big.Int), m.LotSize)
	if !lots.IsInt64() {
		return 0, fmt.Errorf("position %s lots out of range", lots)
	}
	return lots.Int64(), nil
}

// IndexBand is the future's mark-to-index band for the series, 18 dp (FutureAsset.markIndexBand): fills further from
// the index revert, and the book's walk stops there.
func (c *Chain) IndexBand(ctx context.Context, m Market) (*big.Int, error) {
	out, err := c.call(ctx, m.Future, c.a.future, "markIndexBand", m.Expiry)
	if err != nil {
		return nil, fmt.Errorf("markIndexBand(%d): %w", m.Expiry, err)
	}
	return out[0].(*big.Int), nil
}

// Allowance is the account's asset allowance to the book, positive or negative direction.
func (c *Chain) Allowance(ctx context.Context, owner, asset common.Address, positive bool) (*big.Int, error) {
	method := "negativeAssetAllowance"
	if positive {
		method = "positiveAssetAllowance"
	}
	out, err := c.call(ctx, c.SubAccounts, c.a.subAccounts, method, c.AccountID, owner, asset, c.Book)
	if err != nil {
		return nil, err
	}
	return out[0].(*big.Int), nil
}

// ---------------------------------------------------------------------------------------------
// writes: one transaction at a time, signed through exchange.Signer (local key or KMS)
// ---------------------------------------------------------------------------------------------

// splineUpdate matches OnchainBook.SplineUpdate field for field (go-ethereum maps by camel-cased name); v1's has no
// SelfPriced (splineUpdateV1).
type splineUpdate struct {
	MarketId            *big.Int
	AccountId           *big.Int
	OffsetTicks         int32
	HalfSpreadTicks     uint32
	BidCurve            uint16
	AskCurve            uint16
	SkewTicksPerLot     uint32
	MaxSkewTicks        uint32
	RefillLotsPerSecond uint32
	Ttl                 uint32
	Seq                 uint64
	SelfPriced          bool
}

type splineUpdateV1 struct {
	MarketId            *big.Int
	AccountId           *big.Int
	OffsetTicks         int32
	HalfSpreadTicks     uint32
	BidCurve            uint16
	AskCurve            uint16
	SkewTicksPerLot     uint32
	MaxSkewTicks        uint32
	RefillLotsPerSecond uint32
	Ttl                 uint32
	Seq                 uint64
}

// ErrSelfPricedOnV1 is a self-priced spline for a v1 book, which has none: its offset would be read as a peg.
var ErrSelfPricedOnV1 = errors.New("a self-priced spline needs a v2 book; this book is v1")

func (c *Chain) UpdateCalldata(s Spline) ([]byte, error) {
	if !c.V2 {
		if s.SelfPriced {
			return nil, ErrSelfPricedOnV1
		}
		return c.a.bookV1.Pack("updateSplines", []splineUpdateV1{{
			MarketId: c.MarketID, AccountId: c.AccountID, OffsetTicks: s.OffsetTicks, HalfSpreadTicks: s.HalfSpreadTicks,
			BidCurve: s.BidCurve, AskCurve: s.AskCurve, SkewTicksPerLot: s.SkewTicksPerLot, MaxSkewTicks: s.MaxSkewTicks,
			RefillLotsPerSecond: s.RefillLotsPerSecond, Ttl: s.TTL, Seq: s.Seq,
		}})
	}
	return c.a.book.Pack("updateSplines", []splineUpdate{{
		MarketId: c.MarketID, AccountId: c.AccountID, OffsetTicks: s.OffsetTicks, HalfSpreadTicks: s.HalfSpreadTicks,
		BidCurve: s.BidCurve, AskCurve: s.AskCurve, SkewTicksPerLot: s.SkewTicksPerLot, MaxSkewTicks: s.MaxSkewTicks,
		RefillLotsPerSecond: s.RefillLotsPerSecond, Ttl: s.TTL, Seq: s.Seq, SelfPriced: s.SelfPriced,
	}})
}

func (c *Chain) Update(ctx context.Context, s Spline) (common.Hash, error) {
	data, err := c.UpdateCalldata(s)
	if err != nil {
		return common.Hash{}, err
	}
	return c.send(ctx, data)
}

func (c *Chain) Cancel(ctx context.Context) (common.Hash, error) {
	data, err := c.a.book.Pack("cancelSpline", c.MarketID, c.AccountID)
	if err != nil {
		return common.Hash{}, err
	}
	return c.send(ctx, data)
}

func (c *Chain) SetCurve(ctx context.Context, id uint16, regions []Region) (common.Hash, error) {
	data, err := c.a.book.Pack("setCurve", id, regions)
	if err != nil {
		return common.Hash{}, err
	}
	return c.send(ctx, data)
}

func (c *Chain) Register(ctx context.Context) (common.Hash, error) {
	data, err := c.a.book.Pack("registerMaker", c.MarketID, c.AccountID)
	if err != nil {
		return common.Hash{}, err
	}
	return c.send(ctx, data)
}

func (c *Chain) send(ctx context.Context, data []byte) (common.Hash, error) {
	from := c.signer.Address()
	msg := ethereum.CallMsg{From: from, To: &c.Book, Data: data}
	// The estimate runs the call: a revert is reported here, by name, before anything is signed.
	gas, err := c.eth.EstimateGas(ctx, msg)
	if err != nil {
		return common.Hash{}, c.revertError(err)
	}
	nonce, err := c.eth.PendingNonceAt(ctx, from)
	if err != nil {
		return common.Hash{}, fmt.Errorf("nonce: %w", err)
	}
	tip, err := c.eth.SuggestGasTipCap(ctx)
	if err != nil {
		return common.Hash{}, fmt.Errorf("tip: %w", err)
	}
	head, err := c.eth.HeaderByNumber(ctx, nil)
	if err != nil {
		return common.Hash{}, fmt.Errorf("head: %w", err)
	}
	feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip)
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID: c.chainID, Nonce: nonce, GasTipCap: tip, GasFeeCap: feeCap,
		Gas: gas*12/10 + 10_000, To: &c.Book, Data: data,
	})
	txSigner := types.LatestSignerForChainID(c.chainID)
	sig, err := c.signer.SignHash(ctx, txSigner.Hash(tx).Bytes())
	if err != nil {
		return common.Hash{}, fmt.Errorf("sign: %w", err)
	}
	signed, err := tx.WithSignature(txSigner, sig)
	if err != nil {
		return common.Hash{}, err
	}
	if sender, err := types.Sender(txSigner, signed); err != nil || sender != from {
		return common.Hash{}, fmt.Errorf("signed by %s, not the signer %s (%v)", sender.Hex(), from.Hex(), err)
	}
	if err := c.eth.SendTransaction(ctx, signed); err != nil {
		return signed.Hash(), fmt.Errorf("send: %w", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, c.ReceiptTimeout)
	defer cancel()
	receipt, err := c.waitMined(waitCtx, signed.Hash())
	if err != nil {
		return signed.Hash(), fmt.Errorf("receipt: %w", err)
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return signed.Hash(), fmt.Errorf("reverted on chain in block %s", receipt.BlockNumber)
	}
	return signed.Hash(), nil
}

func (c *Chain) waitMined(ctx context.Context, hash common.Hash) (*types.Receipt, error) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		receipt, err := c.eth.TransactionReceipt(ctx, hash)
		if err == nil {
			return receipt, nil
		}
		if !errors.Is(err, ethereum.NotFound) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
}

// ---------------------------------------------------------------------------------------------
// errors
// ---------------------------------------------------------------------------------------------

// RevertError is a call or estimate that reverted, with the custom error's name when the ABIs know it.
type RevertError struct {
	Name string
	Data []byte
	err  error
}

func (e *RevertError) Error() string {
	if e.Name != "" {
		return "reverted: " + e.Name
	}
	return fmt.Sprintf("reverted: %v (data %x)", e.err, e.Data)
}

func (e *RevertError) Unwrap() error { return e.err }

// IsRevert reports whether err is a revert named `name` (an OnchainBook error such as "OB_StaleSeq").
func IsRevert(err error, name string) bool {
	var r *RevertError
	return errors.As(err, &r) && r.Name == name
}

func (c *Chain) revertError(err error) error {
	var de rpc.DataError
	if !errors.As(err, &de) {
		return err
	}
	data := hexData(de.ErrorData())
	return &RevertError{Name: c.errorName(data), Data: data, err: err}
}

func (c *Chain) errorName(data []byte) string {
	if len(data) < 4 {
		return ""
	}
	for _, a := range []abi.ABI{c.a.book, c.a.bookV1, c.a.subAccounts, c.a.future, c.a.engine, c.a.forward, c.a.spot} {
		for name, e := range a.Errors {
			if bytes.Equal(e.ID[:4], data[:4]) {
				return name
			}
		}
	}
	return ""
}

func hexData(v any) []byte {
	s, ok := v.(string)
	if !ok {
		return nil
	}
	b, err := hexDecode(s)
	if err != nil {
		return nil
	}
	return b
}

func hexDecode(s string) ([]byte, error) {
	return hexutil.Decode(s)
}
