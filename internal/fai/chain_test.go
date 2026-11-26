package fai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// rangeCheckingEth is a Base Sepolia endpoint that, like some public ones,
// refuses a log subscription whose block range spans the whole chain. It
// records each subscription's filter and sends one log on each it accepts.
type rangeCheckingEth struct {
	log     types.Log
	mu      sync.Mutex
	filters []map[string]json.RawMessage
}

func (e *rangeCheckingEth) ChainId() *hexutil.Big {
	return (*hexutil.Big)(big.NewInt(84532))
}

func (e *rangeCheckingEth) Logs(ctx context.Context, filter map[string]json.RawMessage) (*rpc.Subscription, error) {
	e.mu.Lock()
	e.filters = append(e.filters, filter)
	e.mu.Unlock()
	if string(filter["fromBlock"]) == `"0x0"` {
		return nil, errors.New("exceed maximum block range: 50000")
	}
	notifier, ok := rpc.NotifierFromContext(ctx)
	if !ok {
		return nil, rpc.ErrNotificationsUnsupported
	}
	sub := notifier.CreateSubscription()
	_ = notifier.Notify(sub.ID, e.log) // sent once the subscription is confirmed
	return sub, nil
}

func (e *rangeCheckingEth) lastFilter() map[string]json.RawMessage {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.filters[len(e.filters)-1]
}

// The listener subscribes with only the contract address and the event topic,
// so an endpoint that refuses ethclient's own subscription (block 0 to the
// latest) accepts it, and logs arrive on it.
func TestSubscriptionCarriesNoBlockRange(t *testing.T) {
	contract := common.HexToAddress("0x02")
	topic := common.HexToHash("0x01")
	eth := &rangeCheckingEth{log: types.Log{Address: contract, Topics: []common.Hash{topic}, BlockNumber: 7, TxHash: common.HexToHash("0x03")}}
	srv := rpc.NewServer()
	if err := srv.RegisterName("eth", eth); err != nil {
		t.Fatal(err)
	}
	ws := httptest.NewServer(srv.WebsocketHandler([]string{"*"}))
	defer func() { ws.Close(); srv.Stop() }()
	url := "ws://" + ws.Listener.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The endpoint refuses ethclient's own subscription.
	plain, err := ethclient.DialContext(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	q := ethereum.FilterQuery{Addresses: []common.Address{contract}, Topics: [][]common.Hash{{topic}}}
	if _, err := plain.SubscribeFilterLogs(ctx, q, make(chan types.Log, 1)); err == nil || !strings.Contains(err.Error(), "exceed maximum block range") {
		t.Fatalf("ethclient's own subscription: err = %v, want the range refused", err)
	}

	s := newChainService(nil, &fakeChain{})
	if err := s.InitBlockchain(ctx, url, contract.Hex()); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.eventSignature = topic
	logs := make(chan types.Log, 1)
	sub, err := s.subscribe(ctx, logs)
	if err != nil {
		t.Fatalf("listener's subscription refused: %v", err)
	}
	defer sub.Unsubscribe()

	filter := eth.lastFilter()
	var addresses []common.Address
	var topics [][]common.Hash
	if len(filter) != 2 || json.Unmarshal(filter["address"], &addresses) != nil || json.Unmarshal(filter["topics"], &topics) != nil ||
		fmt.Sprint(addresses) != fmt.Sprint(q.Addresses) || fmt.Sprint(topics) != fmt.Sprint(q.Topics) {
		t.Fatalf("subscription filter = %s, want only the contract address and the event topic", filter)
	}
	select {
	case got := <-logs:
		if got.BlockNumber != 7 || got.TxHash != eth.log.TxHash {
			t.Fatalf("received log %+v, want block 7", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no log arrived on the subscription")
	}
}
