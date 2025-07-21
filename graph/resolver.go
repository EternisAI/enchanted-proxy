package graph

import (
	"sync"

	"github.com/charmbracelet/log"
	"github.com/eternisai/enchanted-proxy/graph/model"
	"github.com/eternisai/enchanted-proxy/pkg/telegram"
	"github.com/nats-io/nats.go"
)

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require here.

type Resolver struct {
	Logger          *log.Logger
	TelegramService *telegram.Service
	NatsClient      *nats.Conn

	// Subscription management
	subscriptions   map[string]map[string]chan *model.Message // chatUUID -> subscriptionID -> channel
	subscriptionsMu sync.RWMutex
}
