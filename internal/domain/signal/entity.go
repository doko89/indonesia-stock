package signal

import "time"

type WhaleResult struct {
	Symbol     string    `json:"symbol"`
	Volume     int64     `json:"volume"`
	MeanVol    float64   `json:"mean_volume"`
	StdDev     float64   `json:"stddev_volume"`
	ZScore     float64   `json:"z_score"`
	RelVolPct  float64   `json:"rel_vol_pct"`
	IsWhale    bool      `json:"is_whale"`
	Confidence float64   `json:"confidence"`
	Reason     string    `json:"reason"`
	Source     string    `json:"source"`
	Timestamp  time.Time `json:"timestamp"`
}

type Regime string

const (
	RegimeTrending Regime = "trending"
	RegimeRanging  Regime = "ranging"
	RegimeVolatile Regime = "volatile"
)

type RegimeResult struct {
	Symbol      string    `json:"symbol"`
	Regime      Regime    `json:"regime"`
	ATR         float64   `json:"atr"`
	ATRPercent  float64   `json:"atr_percent"`
	EMASlopePct float64   `json:"ema_slope_pct"`
	Confidence  float64   `json:"confidence"`
	Source      string    `json:"source"`
	Timestamp   time.Time `json:"timestamp"`
}

type SignalDecision string

const (
	DecisionBuy     SignalDecision = "BUY"
	DecisionWait    SignalDecision = "WAIT"
	DecisionNoTrade SignalDecision = "NO_TRADE"
)

type SignalResult struct {
	Symbol     string         `json:"symbol"`
	Decision   SignalDecision `json:"decision"`
	Confidence float64        `json:"confidence"`
	Whale      *WhaleResult   `json:"whale"`
	Regime     *RegimeResult  `json:"regime"`
	Imbalance  float64        `json:"imbalance_ratio"`
	ChangePct  float64        `json:"change_pct"`
	Reason     string         `json:"reason"`
	Source     string         `json:"source"`
	Timestamp  time.Time      `json:"timestamp"`
}
