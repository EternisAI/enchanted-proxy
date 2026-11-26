package fai

import (
	"context"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

// chainClient is the chain connection the listener uses: an ethclient whose log
// subscriptions carry no block range.
type chainClient struct{ *ethclient.Client }

// SubscribeFilterLogs subscribes to new logs from q's addresses with q's topics.
// q's block range is not sent.
//
// ethclient's own version always sends a range, from block 0 to the latest
// block when q has none. A log subscription delivers only new logs either way,
// but some public endpoints check the range like a log query's and refuse it as
// too wide: PublicNode's Base endpoints answer "exceed maximum block range:
// 50000" on a connection whose first call was eth_chainId, as InitBlockchain's
// is.
func (c chainClient) SubscribeFilterLogs(ctx context.Context, q ethereum.FilterQuery, ch chan<- types.Log) (ethereum.Subscription, error) {
	filter := map[string]any{}
	if q.Addresses != nil {
		filter["address"] = q.Addresses
	}
	if q.Topics != nil {
		filter["topics"] = q.Topics
	}
	sub, err := c.Client.Client().EthSubscribe(ctx, ch, "logs", filter)
	if err != nil {
		// A nil interface, not one holding a nil *rpc.ClientSubscription.
		return nil, err
	}
	return sub, nil
}
