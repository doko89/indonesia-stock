package quote

import (
	"context"
	"indonesia-stock/internal/domain/stock"
)

type Service struct {
	repo stock.QuoteRepository
}

func New(repo stock.QuoteRepository) *Service {
	return &Service{repo: repo}
}

func (s *Service) GetQuote(ctx context.Context, symbol string) (*stock.Quote, error) {
	return s.repo.GetQuote(ctx, symbol)
}

func (s *Service) GetHistory(ctx context.Context, symbol, interval, rangeStr string) ([]stock.Candle, error) {
	return s.repo.GetHistory(ctx, symbol, interval, rangeStr)
}
