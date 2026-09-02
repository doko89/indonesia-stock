package stock

import (
	"context"
	"indonesia-stock/internal/domain/stock"
)

type Service struct {
	repo stock.Repository
}

func New(repo stock.Repository) *Service {
	return &Service{repo: repo}
}

func (s *Service) GetInfo(ctx context.Context, code string) (*stock.Stock, error) {
	return s.repo.GetInfo(ctx, code)
}
