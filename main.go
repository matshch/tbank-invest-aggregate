// Maximum T-Bank Invest Account Value Evaluator
// Copyright (C) 2025  Artem Leshchev
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"context"
	"errors"
	"maps"
	"math/big"
	"slices"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"opensource.tbank.ru/invest/invest-go/investgo"
	pb "opensource.tbank.ru/invest/invest-go/proto"
)

const TaxYear = 2025

// Updates are applied in reverse order, from newest to oldest
type Update func(portfolio, prices map[string]*big.Rat, currencies map[string]string)

var updates = map[time.Time][]Update{}

// https://fiscaldata.treasury.gov/datasets/treasury-reporting-rates-exchange/treasury-reporting-rates-of-exchange-source
var ExchangeRates = map[string]*big.Rat{
	"amd": big.NewRat(380, 1),
	"chf": big.NewRat(792, 1000),
	"cny": big.NewRat(6998, 1000),
	"eur": big.NewRat(851, 1000),
	"gbp": big.NewRat(743, 1000),
	"hkd": big.NewRat(7784, 1000),
	"jpy": big.NewRat(15661, 100),
	"kgs": big.NewRat(87412, 1000),
	"kzt": big.NewRat(50628, 100),
	"rub": big.NewRat(81996, 1000),
	"tjs": big.NewRat(92, 10),
	"try": big.NewRat(42951, 1000),
	"usd": big.NewRat(1, 1),
	"uzs": big.NewRat(1199941, 100),
}

type Quotation interface {
	GetUnits() int64
	GetNano() int32
}

func ToRat(value Quotation) *big.Rat {
	units := big.NewRat(value.GetUnits(), 1)
	nano := big.NewRat(int64(value.GetNano()), 1_000_000_000)
	return units.Add(units, nano)
}

func AddRat(x, y *big.Rat) *big.Rat {
	if x == nil {
		x = &big.Rat{}
	}
	if y == nil {
		y = &big.Rat{}
	}
	return (&big.Rat{}).Add(x, y)
}

func SubRat(x, y *big.Rat) *big.Rat {
	if x == nil {
		x = &big.Rat{}
	}
	if y == nil {
		y = &big.Rat{}
	}
	return (&big.Rat{}).Sub(x, y)
}

func getCurrencyInstruments(in *investgo.InstrumentsServiceClient) (map[string]string, error) {
	currencies, err := in.Currencies(pb.InstrumentStatus_INSTRUMENT_STATUS_ALL)
	if err != nil {
		return nil, err
	}
	currencyInstruments := make(map[string]string, len(ExchangeRates))
	for _, currency := range currencies.Instruments {
		if _, ok := ExchangeRates[currency.IsoCurrencyName]; ok {
			currencyInstruments[currency.PositionUid] = currency.IsoCurrencyName
		}
	}
	return currencyInstruments, nil
}

// instrumentUid -> assetUid
var assets = make(map[string]string)

// assetUid -> ticker
var tickers = make(map[string]string)

// instrumentUid -> currency
// some assets are traded in different currencies depending on the instrument
var instrumentCurrencies = make(map[string]string)

func getAssetUid(in *investgo.InstrumentsServiceClient, instrumentUid string) (string, error) {
	if assetUid, ok := assets[instrumentUid]; ok {
		return assetUid, nil
	}
	resp, err := in.InstrumentByUid(instrumentUid)
	if err != nil {
		return "", err
	}
	assetUid := resp.Instrument.AssetUid
	asset, err := in.GetAssetBy(assetUid)
	if err != nil {
		return "", err
	}
	for _, inst := range asset.Asset.Instruments {
		assets[inst.Uid] = assetUid
		instInfo, err := in.InstrumentByUid(inst.Uid)
		if err != nil {
			return "", err
		}
		instrumentCurrencies[inst.Uid] = instInfo.Instrument.Currency
	}
	assets[instrumentUid] = assetUid
	tickers[assetUid] = resp.Instrument.Ticker
	return assetUid, nil
}

func ToTickers(uids map[string]*big.Rat) map[string]*big.Rat {
	portfolio := make(map[string]*big.Rat, len(uids))
	for uid, value := range uids {
		ticker := tickers[uid]
		if ticker == "" {
			ticker = uid
		}
		portfolio[ticker] = AddRat(portfolio[ticker], value)
	}
	return portfolio
}

var UnsupportedOperationError = errors.New("unsupported operation type")

func OperationToUpdate(operation *pb.OperationItem) (Update, error) {
	switch operation.Type {
	case pb.OperationType_OPERATION_TYPE_BUY:
		return func(portfolio, _ map[string]*big.Rat, _ map[string]string) {
			portfolio[operation.AssetUid] = SubRat(portfolio[operation.AssetUid], big.NewRat(operation.Quantity, 1))
			if portfolio[operation.AssetUid].Cmp(&big.Rat{}) == 0 {
				delete(portfolio, operation.AssetUid)
			}
			portfolio[operation.Payment.Currency] = SubRat(portfolio[operation.Payment.Currency], ToRat(operation.Payment))
		}, nil
	case pb.OperationType_OPERATION_TYPE_SELL:
		return func(portfolio, _ map[string]*big.Rat, _ map[string]string) {
			portfolio[operation.AssetUid] = AddRat(portfolio[operation.AssetUid], big.NewRat(operation.Quantity, 1))
			portfolio[operation.Payment.Currency] = SubRat(portfolio[operation.Payment.Currency], ToRat(operation.Payment))
			if portfolio[operation.Payment.Currency].Cmp(&big.Rat{}) == 0 {
				delete(portfolio, operation.Payment.Currency)
			}
		}, nil
	case pb.OperationType_OPERATION_TYPE_BROKER_FEE,
		pb.OperationType_OPERATION_TYPE_DIVIDEND,
		pb.OperationType_OPERATION_TYPE_DIVIDEND_TAX,
		pb.OperationType_OPERATION_TYPE_INPUT,
		pb.OperationType_OPERATION_TYPE_TAX:
		return func(portfolio, _ map[string]*big.Rat, _ map[string]string) {
			portfolio[operation.Payment.Currency] = SubRat(portfolio[operation.Payment.Currency], ToRat(operation.Payment))
			if portfolio[operation.Payment.Currency].Cmp(&big.Rat{}) == 0 {
				delete(portfolio, operation.Payment.Currency)
			}
		}, nil
	case pb.OperationType_OPERATION_TYPE_INPUT_SECURITIES:
		return func(portfolio, _ map[string]*big.Rat, _ map[string]string) {
			portfolio[operation.AssetUid] = SubRat(portfolio[operation.AssetUid], big.NewRat(operation.Quantity, 1))
			if portfolio[operation.AssetUid].Cmp(&big.Rat{}) == 0 {
				delete(portfolio, operation.AssetUid)
			}
			// there is a payment, but it looks like it is for information purposes only
		}, nil
	default:
		return nil, UnsupportedOperationError
	}
}

func SellAll(portfolio, prices map[string]*big.Rat, currencies map[string]string) {
	for assetUid, quantity := range portfolio {
		if price, ok := prices[assetUid]; ok {
			currency := currencies[assetUid]
			portfolio[currency] = AddRat(portfolio[currency], (&big.Rat{}).Mul(price, quantity))
			delete(portfolio, assetUid)
		}
	}
}

func Aggregate(cost map[string]*big.Rat) *big.Rat {
	sum := new(big.Rat)
	for currency, quantity := range cost {
		sum = AddRat(sum, (&big.Rat{}).Quo(quantity, ExchangeRates[currency]))
	}
	return sum
}

func main() {
	logger := zap.Must(zap.NewDevelopment())
	defer logger.Sync()

	config, err := investgo.LoadConfig("config.yaml")
	if err != nil {
		logger.Fatal("error loading config", zap.Error(err))
	}

	logger.Debug("creating client")
	client, err := investgo.NewClient(context.Background(), config, logger.Sugar())
	if err != nil {
		logger.Fatal("error creating client", zap.Error(err))
	}
	defer func() {
		logger.Debug("closing client")
		err := client.Stop()
		if err != nil {
			logger.Error("error closing client", zap.Error(err))
		}
	}()

	if config.AccountId == "" {
		logger.Info("cannot proceed without account set in config, getting accounts")
		resp, err := client.NewUsersServiceClient().GetAccounts(nil)
		if err != nil {
			logger.Error("error getting accounts", zap.Error(err))
			return
		}
		for _, account := range resp.Accounts {
			logger.Info("found account", zap.String("id", account.Id), zap.String("name", account.Name))
		}
		logger.Error("set one of these accounts as AccountId in config.yaml")
		return
	}

	in := client.NewInstrumentsServiceClient()
	currencyInstruments, err := getCurrencyInstruments(in)
	if err != nil {
		logger.Error("error getting currency instruments", zap.Error(err))
		return
	}

	op := client.NewOperationsServiceClient()
	now := time.Now()
	positions, err := op.GetPortfolio(config.AccountId, pb.PortfolioRequest_RUB)
	if err != nil {
		logger.Error("error getting positions", zap.Error(err))
		return
	}

	portfolio := make(map[string]*big.Rat, len(positions.Positions))
	prices := make(map[string]*big.Rat, len(positions.Positions))
	currencies := make(map[string]string, len(positions.Positions))
	for _, position := range positions.Positions {
		var key string
		if currency, ok := currencyInstruments[position.PositionUid]; ok {
			key = currency
		} else {
			var err error
			key, err = getAssetUid(in, position.InstrumentUid)
			if err != nil {
				logger.Error("error getting instrument for position",
					zap.String("position", position.Figi),
					zap.Error(err))
				return
			}
			prices[key] = ToRat(position.CurrentPrice)
			currencies[key] = position.CurrentPrice.Currency
		}
		portfolio[key] = AddRat(portfolio[key], ToRat(position.Quantity))
	}
	cost := maps.Clone(portfolio)
	SellAll(cost, prices, currencies)
	logger.Info("current portfolio",
		zap.Any("portfolio", ToTickers(portfolio)),
		zap.Any("cost", cost),
		zap.Stringer("aggregate", Aggregate(cost)))

	req := &investgo.GetOperationsByCursorRequest{
		AccountId: config.AccountId,
		From:      time.Date(TaxYear, 1, 1, 0, 0, 0, 0, time.UTC),
		To:        now,
		State:     pb.OperationState_OPERATION_STATE_EXECUTED,
	}
	for {
		operations, err := op.GetOperationsByCursor(req)
		if err != nil {
			logger.Error("error getting operations",
				zap.Any("request", req),
				zap.Error(err))
			return
		}
		for _, operation := range operations.Items {
			if _, ok := tickers[operation.AssetUid]; !ok {
				_, err = getAssetUid(in, operation.InstrumentUid)
				if err != nil {
					logger.Error("error getting instrument for operation",
						zap.String("figi", operation.Figi),
						zap.String("name", operation.Name),
						zap.String("description", operation.Description),
						zap.Error(err))
				}
			}
			if operation.AssetUid != "" {
				assets[operation.InstrumentUid] = operation.AssetUid
			}
			update, err := OperationToUpdate(operation)
			if err != nil {
				logger.Error("cannot process operation",
					zap.Error(err),
					zap.Any("operation", operation))
				return
			}
			date := operation.Date.AsTime()
			updates[date] = append(updates[date], update)
		}
		if !operations.HasNext {
			break
		}
		req.Cursor = operations.NextCursor
	}
	logger.Info("instruments", zap.Any("assets", assets), zap.Any("tickers", tickers))

	md := client.NewMarketDataServiceClient()
	for instrumentUid, assetUid := range assets {
		candles, err := md.GetHistoricCandles(&investgo.GetHistoricCandlesRequest{
			Instrument: instrumentUid,
			Interval:   pb.CandleInterval_CANDLE_INTERVAL_HOUR,
			From:       time.Date(TaxYear, 1, 1, 0, 0, 0, 0, time.UTC),
			// There are some issues with future prices reuse as we are going backwards in time,
			// so it works better to have some extra data on the border to get the best possible approximation.
			To:     time.Date(TaxYear+1, 2, 1, 0, 0, 0, 0, time.UTC),
			Source: pb.GetCandlesRequest_CANDLE_SOURCE_INCLUDE_WEEKEND,
		})
		if err != nil {
			if status.Code(err) == codes.NotFound {
				logger.Warn("cannot found candles for instrument",
					zap.String("instrument", instrumentUid),
					zap.String("asset", assetUid),
					zap.String("ticker", tickers[assetUid]))
				continue
			}
			logger.Error("error getting candles for instrument",
				zap.String("instrument", instrumentUid),
				zap.String("asset", assetUid),
				zap.String("ticker", tickers[assetUid]),
				zap.Error(err))
			return
		}
		logger.Debug("got candles",
			zap.String("instrument", instrumentUid),
			zap.String("asset", assetUid),
			zap.String("ticker", tickers[assetUid]))
		asset := assetUid
		inst := instrumentUid
		for _, candle := range candles {
			date := candle.Time.AsTime()
			price := ToRat(candle.High)
			updates[date] = append(updates[date], func(_, prices map[string]*big.Rat, currencies map[string]string) {
				prices[asset] = price
				currencies[asset] = instrumentCurrencies[inst]
			})
		}
	}

	var bestPortfolio, bestCost, bestPrices map[string]*big.Rat
	var bestTime time.Time
	bestAggregate := &big.Rat{}

	logger.Info("going back in time", zap.Uint("tax_year", TaxYear))
	times := slices.SortedFunc(maps.Keys(updates), func(a, b time.Time) int {
		return b.Compare(a)
	})
	for _, date := range times {
		portfolio = maps.Clone(portfolio)
		prices = maps.Clone(prices)
		for _, update := range updates[date] {
			update(portfolio, prices, currencies)
		}
		cost := maps.Clone(portfolio)
		SellAll(cost, prices, currencies)
		aggregate := Aggregate(cost)
		logger.Debug("new portfolio",
			zap.Time("time", date),
			zap.Any("portfolio", ToTickers(portfolio)),
			zap.Any("cost", cost),
			zap.Stringer("aggregate", aggregate))
		if date.Year() != TaxYear {
			continue
		}
		if bestAggregate.Cmp(aggregate) < 0 {
			bestPortfolio = portfolio
			bestPrices = prices
			bestCost = cost
			bestTime = date
			bestAggregate = aggregate
			logger.Debug("new best shown above")
		}
	}
	logger.Info("best portfolio",
		zap.Time("time", bestTime),
		zap.Any("portfolio", ToTickers(bestPortfolio)),
		zap.Any("prices", ToTickers(bestPrices)),
		zap.Any("cost", bestCost),
		zap.Stringer("aggregate", bestAggregate))
}
