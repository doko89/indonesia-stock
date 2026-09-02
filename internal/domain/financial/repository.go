package financial

import "context"

type Repository interface {
	GetGeneralInfo(ctx context.Context, code string) (*GeneralInfo, error)
	GetReport(ctx context.Context, code string, period Period, reportType ReportType) (*Report, error)
}
