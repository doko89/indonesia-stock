package financial

import (
	"context"
	"indonesia-stock/internal/domain/financial"
)

type Service struct {
	repo financial.Repository
}

func New(repo financial.Repository) *Service {
	return &Service{repo: repo}
}

func (s *Service) GetReport(ctx context.Context, code string, period financial.Period, t financial.ReportType) (*financial.Report, error) {
	return s.repo.GetReport(ctx, code, period, t)
}

func (s *Service) GetGeneralInfo(ctx context.Context, code string) (*financial.GeneralInfo, error) {
	return s.repo.GetGeneralInfo(ctx, code)
}
