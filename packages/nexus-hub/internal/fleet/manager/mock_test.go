package manager

import (
	"context"
	"sync"
)

// mockWSPool implements WSPool for testing.
type mockWSPool struct {
	mu                sync.Mutex
	broadcastCount    int
	lastBroadcastType string
	lastBroadcastMsg  []byte
	sendCalled        bool
	lastSendID        string
	connectedIDs      map[string]bool
	// sendReturn lets a test simulate a Send-time failure (write error or a
	// race where the conn was removed between IsConnected and Send). When
	// non-nil, Send returns sendReturn[thingID]; otherwise Send defaults to
	// returning true. Use this to exercise the WS→MQ fall-through path.
	sendReturn map[string]bool
	// sendCalls records every (thingID, msg) pair passed to Send.
	sendCalls []mockSendCall
}

// mockSendCall records a single Send invocation.
type mockSendCall struct {
	ThingID string
	Data    []byte
}

func (m *mockWSPool) Send(thingID string, msg []byte) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sendCalled = true
	m.lastSendID = thingID
	// Keep a copy of the data so the caller can inspect it after the call.
	cp := make([]byte, len(msg))
	copy(cp, msg)
	m.sendCalls = append(m.sendCalls, mockSendCall{ThingID: thingID, Data: cp})
	if m.sendReturn != nil {
		if v, ok := m.sendReturn[thingID]; ok {
			return v
		}
	}
	return true
}

func (m *mockWSPool) Broadcast(thingType string, msg []byte) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastBroadcastType = thingType
	m.lastBroadcastMsg = msg
	return m.broadcastCount
}

func (m *mockWSPool) IsConnected(thingID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.connectedIDs == nil {
		return false
	}
	return m.connectedIDs[thingID]
}

// mockMQProducer implements mq.Producer for testing.
type mockMQProducer struct {
	mu           sync.Mutex
	publishCount int
	lastTopic    string
	lastData     []byte
	publishErr   error
	enqueueCount int
}

func (m *mockMQProducer) Publish(_ context.Context, topic string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.publishCount++
	m.lastTopic = topic
	m.lastData = data
	return m.publishErr
}

func (m *mockMQProducer) Enqueue(_ context.Context, queue string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enqueueCount++
	return nil
}

func (m *mockMQProducer) Close() error { return nil }
