package stock

import "time"

type Board string

const (
	BoardUtama      Board = "Utama"
	BoardPengembangan Board = "Pengembangan"
	BoardAkselerasi Board = "Akselerasi"
	BoardEkonomiBaru Board = "Ekonomi Baru"
)

type Stock struct {
	Code        string    `json:"code"`
	Name        string    `json:"name"`
	Board       Board     `json:"board"`
	ListedDate  time.Time `json:"listed_date"`
	Shares      int64     `json:"shares"`
	Currency    string    `json:"currency"`
	Sector      string    `json:"sector"`
}

type Quote struct {
	Symbol    string    `json:"symbol"`
	Price     float64   `json:"price"`
	Open      float64   `json:"open"`
	High      float64   `json:"high"`
	Low       float64   `json:"low"`
	Close     float64   `json:"close"`
	PrevClose float64   `json:"prev_close"`
	Volume    int64     `json:"volume"`
	Change    float64   `json:"change"`
	ChangePct float64   `json:"change_pct"`
	Timestamp time.Time `json:"timestamp"`
	Source    string    `json:"source"`
}

type Candle struct {
	Timestamp time.Time `json:"timestamp"`
	Open      float64   `json:"open"`
	High      float64   `json:"high"`
	Low       float64   `json:"low"`
	Close     float64   `json:"close"`
	Volume    int64     `json:"volume"`
}
