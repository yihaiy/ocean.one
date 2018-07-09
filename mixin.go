package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/MixinMessenger/bot-api-go-client"
	"github.com/MixinMessenger/go-number"
	"github.com/MixinMessenger/ocean.one/config"
	"github.com/MixinMessenger/ocean.one/engine"
	"github.com/MixinMessenger/ocean.one/persistence"
	"github.com/satori/go.uuid"
	"github.com/ugorji/go/codec"
)

const (
	AmountPrecision = 4
	MaxPrice        = 1000000000
	MaxAmount       = 5000000000
	MaxFunds        = MaxPrice * MaxAmount

	BitcoinAssetId = "c6d0c728-2624-429b-8e0d-d9d19b6592fa"
	USDTAssetId    = "815b0b1a-2764-3736-8faa-42d694fa620a"
)

type Error struct {
	Status      int    `json:"status"`
	Code        int    `json:"code"`
	Description string `json:"description"`
}

type Snapshot struct {
	SnapshotId string `json:"snapshot_id"`
	Amount     string `json:"amount"`
	Asset      struct {
		AssetId string `json:"asset_id"`
	} `json:"asset"`
	CreatedAt time.Time `json:"created_at"`

	TraceId    string `json:"trace_id"`
	UserId     string `json:"user_id"`
	OpponentId string `json:"opponent_id"`
	Data       string `json:"data"`
}

type OrderAction struct {
	S string    // side
	A uuid.UUID // asset
	P string    // price
	T string    // type
	O uuid.UUID // order
}

func (ex *Exchange) ensureProcessSnapshot(ctx context.Context, s *Snapshot) {
	for {
		err := ex.processSnapshot(ctx, s)
		if err == nil {
			break
		}
		log.Println("ensureProcessSnapshot", err)
		time.Sleep(100 * time.Millisecond)
	}
}

func (ex *Exchange) processSnapshot(ctx context.Context, s *Snapshot) error {
	if s.UserId != config.ClientId {
		return nil
	}
	if s.OpponentId == "" || s.TraceId == "" {
		return nil
	}
	if number.FromString(s.Amount).Exhausted() {
		return nil
	}

	action := ex.decryptOrderAction(ctx, s.Data)
	if action == nil {
		return ex.refundSnapshot(ctx, s)
	}
	if action.A.String() == s.Asset.AssetId {
		return ex.refundSnapshot(ctx, s)
	}
	if action.O.String() != uuid.Nil.String() {
		return persistence.CancelOrderAction(ctx, action.O.String(), s.CreatedAt, s.OpponentId)
	}

	if action.T != engine.OrderTypeLimit && action.T != engine.OrderTypeMarket {
		return ex.refundSnapshot(ctx, s)
	}

	quote, base := ex.getQuoteBasePair(s, action)
	if quote == "" {
		return ex.refundSnapshot(ctx, s)
	}

	priceDecimal := number.FromString(action.P)
	maxPrice := number.NewDecimal(MaxPrice, int32(QuotePrecision(quote)))
	if priceDecimal.Cmp(maxPrice) > 0 {
		return ex.refundSnapshot(ctx, s)
	}
	price := priceDecimal.Integer(QuotePrecision(quote))
	if action.T == engine.OrderTypeLimit {
		if price.IsZero() {
			return ex.refundSnapshot(ctx, s)
		}
	} else if !price.IsZero() {
		return ex.refundSnapshot(ctx, s)
	}

	fundsPrecision := AmountPrecision + QuotePrecision(quote)
	funds := number.NewInteger(0, fundsPrecision)
	amount := number.NewInteger(0, AmountPrecision)

	assetDecimal := number.FromString(s.Amount)
	if action.S == engine.PageSideBid {
		maxFunds := number.NewDecimal(MaxFunds, int32(fundsPrecision))
		if assetDecimal.Cmp(maxFunds) > 0 {
			return ex.refundSnapshot(ctx, s)
		}
		funds = assetDecimal.Integer(fundsPrecision)
		if funds.Decimal().Cmp(QuoteMinimum(quote)) < 0 {
			return ex.refundSnapshot(ctx, s)
		}
	} else {
		maxAmount := number.NewDecimal(MaxAmount, AmountPrecision)
		if assetDecimal.Cmp(maxAmount) > 0 {
			return ex.refundSnapshot(ctx, s)
		}
		amount = assetDecimal.Integer(AmountPrecision)
		if action.T == engine.OrderTypeLimit && price.Mul(amount).Decimal().Cmp(QuoteMinimum(quote)) < 0 {
			return ex.refundSnapshot(ctx, s)
		}
	}

	return persistence.CreateOrderAction(ctx, &engine.Order{
		Id:              s.TraceId,
		Type:            action.T,
		Side:            action.S,
		Quote:           quote,
		Base:            base,
		Price:           price,
		RemainingAmount: amount,
		FilledAmount:    amount.Zero(),
		RemainingFunds:  funds,
		FilledFunds:     funds.Zero(),
	}, s.OpponentId, s.CreatedAt)
}

func (ex *Exchange) getQuoteBasePair(s *Snapshot, a *OrderAction) (string, string) {
	var quote, base string
	if a.S == engine.PageSideAsk {
		quote, base = a.A.String(), s.Asset.AssetId
	} else if a.S == engine.PageSideBid {
		quote, base = s.Asset.AssetId, a.A.String()
	} else {
		return "", ""
	}
	if quote != BitcoinAssetId && quote != USDTAssetId {
		return "", ""
	}
	if quote == BitcoinAssetId && base == USDTAssetId {
		return "", ""
	}
	return quote, base
}

func (ex *Exchange) refundSnapshot(ctx context.Context, s *Snapshot) error {
	amount := number.FromString(s.Amount).Mul(number.FromString("0.999"))
	if amount.Exhausted() {
		return nil
	}
	return persistence.CreateRefundTransfer(ctx, s.OpponentId, s.Asset.AssetId, amount, s.TraceId)
}

func (ex *Exchange) decryptOrderAction(ctx context.Context, data string) *OrderAction {
	payload, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil
	}
	var action OrderAction
	decoder := codec.NewDecoderBytes(payload, ex.codec)
	err = decoder.Decode(&action)
	if err != nil {
		return nil
	}
	switch action.T {
	case "L":
		action.T = engine.OrderTypeLimit
	case "M":
		action.T = engine.OrderTypeMarket
	}
	switch action.S {
	case "A":
		action.S = engine.PageSideAsk
	case "B":
		action.S = engine.PageSideBid
	}
	return &action
}

func (ex *Exchange) requestMixinNetwork(ctx context.Context, checkpoint time.Time, limit int) ([]*Snapshot, error) {
	uri := fmt.Sprintf("/network/snapshots?offset=%s&order=ASC&limit=%d", checkpoint.Format(time.RFC3339Nano), limit)
	token, err := bot.SignAuthenticationToken(config.ClientId, config.SessionId, config.SessionKey, "GET", uri, "")
	if err != nil {
		return nil, err
	}
	body, err := bot.Request(ctx, "GET", uri, nil, token)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data  []*Snapshot `json:"data"`
		Error string      `json:"error"`
	}
	err = json.Unmarshal(body, &resp)
	if err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	return resp.Data, nil
}

func (ex *Exchange) sendTransfer(ctx context.Context, recipientId, assetId string, amount number.Decimal, traceId, memo string) error {
	if amount.Exhausted() {
		return nil
	}

	pin := encryptPIN(ctx, config.SessionAssetPIN, config.PinToken, config.SessionId, config.SessionKey, uint64(time.Now().UnixNano()))
	data, err := json.Marshal(map[string]interface{}{
		"asset_id":    assetId,
		"opponent_id": recipientId,
		"amount":      amount.Persist(),
		"pin":         pin,
		"trace_id":    traceId,
		"memo":        memo,
	})
	if err != nil {
		return err
	}

	token, err := bot.SignAuthenticationToken(config.ClientId, config.SessionId, config.SessionKey, "POST", "/transfers", string(data))
	if err != nil {
		return err
	}
	body, err := bot.Request(ctx, "POST", "/transfers", data, token)
	if err != nil {
		return err
	}

	var resp struct {
		Error Error `json:"error"`
	}
	err = json.Unmarshal(body, &resp)
	if err != nil {
		return err
	}
	if resp.Error.Code > 0 {
		return errors.New(resp.Error.Description)
	}
	return nil
}

func encryptPIN(ctx context.Context, pin, pinToken, sessionId, privateKey string, iterator uint64) string {
	privBlock, _ := pem.Decode([]byte(privateKey))
	if privBlock == nil {
		return ""
	}
	priv, err := x509.ParsePKCS1PrivateKey(privBlock.Bytes)
	if err != nil {
		return ""
	}
	token, _ := base64.StdEncoding.DecodeString(pinToken)
	keyBytes, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, priv, token, []byte(sessionId))
	if err != nil {
		return ""
	}
	pinByte := []byte(pin)
	timeBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(timeBytes, uint64(time.Now().Unix()))
	pinByte = append(pinByte, timeBytes...)
	iteratorBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(iteratorBytes, iterator)
	pinByte = append(pinByte, iteratorBytes...)
	padding := aes.BlockSize - len(pinByte)%aes.BlockSize
	padtext := bytes.Repeat([]byte{byte(padding)}, padding)
	pinByte = append(pinByte, padtext...)
	block, _ := aes.NewCipher(keyBytes)
	ciphertext := make([]byte, aes.BlockSize+len(pinByte))
	iv := ciphertext[:aes.BlockSize]
	io.ReadFull(rand.Reader, iv)
	mode := cipher.NewCBCEncrypter(block, iv)
	mode.CryptBlocks(ciphertext[aes.BlockSize:], pinByte)
	return base64.StdEncoding.EncodeToString(ciphertext)
}
