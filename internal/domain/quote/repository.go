package quote

import (
	"context"
	"indonesia-stock/internal/domain/stock"
)

type Repository interface {
	GetQuote(ctx context.Context, symbol string) (*stock.Quote, error)
	GetHistory(ctx context.Context, symbol string, interval string, rangeStr string) ([]stock.Candle, error)
}
