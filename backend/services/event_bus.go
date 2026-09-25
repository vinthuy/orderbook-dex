package services

import (
	"fmt"
	"sync"
	"time"
)

// Event represents an event that can be published to the event bus
type Event struct {
	Topic     string
	Data      interface{}
	Timestamp time.Time
}

// EventHandler is a function type that handles events
type EventHandler func(event Event) error

// EventBus implements a publish/subscribe event system
type EventBus struct {
	subscribers map[string][]EventHandler
	mu          sync.RWMutex
}

// NewEventBus creates a new EventBus instance
func NewEventBus() *EventBus {
	return &EventBus{
		subscribers: make(map[string][]EventHandler),
	}
}

// Subscribe registers a handler for a specific topic
func (eb *EventBus) Subscribe(topic string, handler EventHandler) {
	eb.mu.Lock()
	defer eb.mu.Unlock()

	eb.subscribers[topic] = append(eb.subscribers[topic], handler)
}

// Publish sends an event to all subscribers of the event's topic
func (eb *EventBus) Publish(event Event) error {
	eb.mu.RLock()
	defer eb.mu.RUnlock()

	// Set timestamp if not provided
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	handlers, exists := eb.subscribers[event.Topic]
	if !exists {
		return nil // No subscribers for this topic
	}

	// Call all handlers
	for _, handler := range handlers {
		if err := handler(event); err != nil {
			return fmt.Errorf("handler error for topic %s: %w", event.Topic, err)
		}
	}

	return nil
}

// Unsubscribe removes all handlers for a specific topic
func (eb *EventBus) Unsubscribe(topic string) {
	eb.mu.Lock()
	defer eb.mu.Unlock()

	delete(eb.subscribers, topic)
}