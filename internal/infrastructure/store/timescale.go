package store

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	// blank import registers the "postgres" database/sql driver (required by
	// sql.Open in NewClient); the package is never referenced directly.
	_ "github.com/lib/pq"
)

type Client struct {
	dsn       string
	db        *sql.DB
	available bool
	mu        sync.Mutex
	mem       []CandleRow
}

type CandleRow struct {
	Symbol string
	Time   time.Time
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume int64
}

func New(dsn string) *Client {
	c := &Client{dsn: dsn, mem: []CandleRow{}}
	if dsn == "" {
		return c
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return c
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return c
	}
	c.db = db
	c.available = true
	return c
}

func (c *Client) Available() bool { return c.available }

func (c *Client) DB() *sql.DB { return c.db }

func (c *Client) InsertCandle(r CandleRow) error {
	if c.available && c.db != nil {
		_, err := c.db.Exec(`INSERT INTO running_trades(symbol,time,open,high,low,close,volume) VALUES ($1,$2,$3,$4,$5,$6,$7)`, r.Symbol, r.Time, r.Open, r.High, r.Low, r.Close, r.Volume)
		if err == nil {
			return nil
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mem = append(c.mem, r)
	if len(c.mem) > 10000 {
		c.mem = c.mem[1000:]
	}
	return nil
}

// queryCandlesDB reads candles from TimescaleDB; returns nil rows on any
// failure so the caller can fall back to memory.
func (c *Client) queryCandlesDB(symbol string, since time.Time) []CandleRow {
	if !c.available || c.db == nil {
		return nil
	}
	rows, err := c.db.Query(`SELECT symbol,time,open,high,low,close,volume FROM running_trades WHERE symbol=$1 AND time > $2 ORDER BY time ASC LIMIT 5000`, symbol, since)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []CandleRow
	for rows.Next() {
		var r CandleRow
		if err := rows.Scan(&r.Symbol, &r.Time, &r.Open, &r.High, &r.Low, &r.Close, &r.Volume); err == nil {
			out = append(out, r)
		}
	}
	if err := rows.Err(); err != nil {
		return nil
	}
	return out
}

// candlesFromMemory filters the in-memory ring for the symbol since a time.
func (c *Client) candlesFromMemory(symbol string, since time.Time) []CandleRow {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []CandleRow
	for _, r := range c.mem {
		if r.Symbol == symbol && r.Time.After(since) {
			out = append(out, r)
		}
	}
	return out
}

func (c *Client) QueryCandles(symbol string, since time.Time) ([]CandleRow, error) {
	if rows := c.queryCandlesDB(symbol, since); rows != nil {
		return rows, nil
	}
	return c.candlesFromMemory(symbol, since), nil
}

func (c *Client) Ping() error {
	if !c.available || c.db == nil {
		return fmt.Errorf("timescaledb not available (fallback memory active)")
	}
	return c.db.Ping()
}

func (c *Client) Close() error {
	if c.db != nil {
		return c.db.Close()
	}
	return nil
}
