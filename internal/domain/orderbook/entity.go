package orderbook

import "time"

type Level struct {
	Price float64 `json:"price"`
	Lot   int64   `json:"lot"`
	Orders int    `json:"orders,omitempty"`
}

type Orderbook struct {
	Symbol    string    `json:"symbol"`
	Timestamp time.Time `json:"timestamp"`
	Bids      []Level   `json:"bids"`
	Asks      []Level   `json:"asks"`
	Source    string    `json:"source"`
}

func (o Orderbook) BestBid() *Level {
	if len(o.Bids) == 0 {
		return nil
	}
	return &o.Bids[0]
}

func (o Orderbook) BestAsk() *Level {
	if len(o.Asks) == 0 {
		return nil
	}
	return &o.Asks[0]
}

func (o Orderbook) Spread() float64 {
	bid := o.BestBid()
	ask := o.BestAsk()
	if bid == nil || ask == nil {
		return 0
	}
	return ask.Price - bid.Price
}

func (o Orderbook) MidPrice() float64 {
	bid := o.BestBid()
	ask := o.BestAsk()
	if bid == nil || ask == nil {
		return 0
	}
	return (bid.Price + ask.Price) / 2
}

type Imbalance struct {
	BidVolume int64   `json:"bid_volume"`
	AskVolume int64   `json:"ask_volume"`
	Ratio     float64 `json:"ratio"`
}

func (o Orderbook) Imbalance() Imbalance {
	var bv, av int64
	for _, l := range o.Bids {
		bv += l.Lot
	}
	for _, l := range o.Asks {
		av += l.Lot
	}
	total := bv + av
	var ratio float64
	if total > 0 {
		ratio = float64(bv-av) / float64(total)
	}
	return Imbalance{BidVolume: bv, AskVolume: av, Ratio: ratio}
}
