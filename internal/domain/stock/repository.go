package stock

import "context"

type Repository interface {
	GetInfo(ctx context.Context, code string) (*Stock, error)
	List(ctx context.Context) ([]Stock, error)
}

type QuoteRepository interface {
	GetQuote(ctx context.Context, symbol string) (*Quote, error)
	GetHistory(ctx context.Context, symbol, interval, rangeStr string) ([]Candle, error)
}
