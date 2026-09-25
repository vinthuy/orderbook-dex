package services

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEventBus_SubscribeAndPublish(t *testing.T) {
	bus := NewEventBus()

	// Create handler
	handlerCalled := false
	handler := func(event Event) error {
		handlerCalled = true
		return nil
	}

	// Subscribe
	bus.Subscribe("test.event", handler)

	// Publish event
	event := Event{
		Topic: "test.event",
		Data:  "test data",
	}

	err := bus.Publish(event)

	assert.NoError(t, err)
	assert.True(t, handlerCalled)
}

func TestEventBus_MultipleSubscribers(t *testing.T) {
	bus := NewEventBus()

	callCount := 0
	handler1 := func(event Event) error {
		callCount++
		return nil
	}
	handler2 := func(event Event) error {
		callCount++
		return nil
	}

	bus.Subscribe("test.event", handler1)
	bus.Subscribe("test.event", handler2)

	event := Event{Topic: "test.event", Data: "test"}
	bus.Publish(event)

	assert.Equal(t, 2, callCount)
}