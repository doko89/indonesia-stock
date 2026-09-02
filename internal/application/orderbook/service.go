package orderbook

import (
	"context"
	"indonesia-stock/internal/domain/orderbook"
)

type Service struct {
	repo orderbook.Repository
}

func New(repo orderbook.Repository) *Service {
	return &Service{repo: repo}
}

func (s *Service) Get(ctx context.Context, symbol string) (*orderbook.Orderbook, error) {
	return s.repo.Get(ctx, symbol)
}

func (s *Service) Stream(ctx context.Context, symbol string, interval int) (<-chan orderbook.Orderbook, error) {
	return s.repo.Stream(ctx, symbol, interval)
}
