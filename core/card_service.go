package core

import "sync"

// CardHandler processes a card navigation or action event and returns a card to display.
type CardHandler func(args, sessionKey string) *Card

// CardService is a registry of card navigation and action handlers.
// It replaces the monolithic switch/case in handleCardNav.
type CardService struct {
	mu          sync.RWMutex
	navHandlers map[string]CardHandler // "nav:/model" → handler
	actHandlers map[string]CardHandler // "act:/model" → handler
}

func NewCardService() *CardService {
	return &CardService{
		navHandlers: make(map[string]CardHandler),
		actHandlers: make(map[string]CardHandler),
	}
}

func (cs *CardService) RegisterNav(path string, handler CardHandler) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.navHandlers[path] = handler
}

func (cs *CardService) RegisterAct(path string, handler CardHandler) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.actHandlers[path] = handler
}

func (cs *CardService) HandleNav(path, args, sessionKey string) *Card {
	cs.mu.RLock()
	handler, ok := cs.navHandlers[path]
	cs.mu.RUnlock()
	if !ok {
		return nil
	}
	return handler(args, sessionKey)
}

func (cs *CardService) HandleAct(path, args, sessionKey string) *Card {
	cs.mu.RLock()
	handler, ok := cs.actHandlers[path]
	cs.mu.RUnlock()
	if !ok {
		return nil
	}
	return handler(args, sessionKey)
}
