package financial

type Period string

const (
	PeriodAnnual   Period = "annual"
	PeriodQuarterly Period = "quarterly"
)

type ReportType string

const (
	TypeIncomeStatement ReportType = "income_statement"
	TypeBalanceSheet    ReportType = "balance_sheet"
	TypeCashFlow        ReportType = "cash_flow"
)

type Report struct {
	StockCode string     `json:"stock_code"`
	Period    Period     `json:"period"`
	Type      ReportType `json:"type"`
	Years     []int      `json:"years"`
	Quarters  []int      `json:"quarters,omitempty"`
	Data      map[string]any `json:"data"`
	Source    string     `json:"source"`
	Currency  string     `json:"currency,omitempty"`
}

type GeneralInfo struct {
	StockCode  string  `json:"stock_code"`
	Name       string  `json:"name"`
	Price      float64 `json:"price"`
	Currency   string  `json:"currency"`
	ListedDate string  `json:"listed_date"`
	Shares     int64   `json:"shares"`
	Board      string  `json:"board"`
	Source     string  `json:"source"`
}

type XBRLReport struct {
	StockCode      string         `json:"stock_code"`
	Year           int            `json:"year"`
	Quarter        int            `json:"quarter"`
	GeneralInfo    map[string]string `json:"general_info"`
	BalanceSheet   map[string]float64 `json:"balance_sheet"`
	IncomeStatement map[string]float64 `json:"income_statement,omitempty"`
	CashFlow       map[string]float64 `json:"cash_flow,omitempty"`
	Source         string         `json:"source"`
}
