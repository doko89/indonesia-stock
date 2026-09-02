package orderbook

import "context"

type Repository interface {
	Get(ctx context.Context, symbol string) (*Orderbook, error)
	Stream(ctx context.Context, symbol string, intervalSeconds int) (<-chan Orderbook, error)
}
