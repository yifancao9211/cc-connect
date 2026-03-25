package core

import "sync"

// AgentPool manages multiple agent backends with per-user active agent selection.
type AgentPool struct {
	mu      sync.RWMutex
	agents  map[string]Agent  // name → Agent
	active  string            // global default agent name
	perUser map[string]string // sessionKey → agent name override
}

func NewAgentPool() *AgentPool {
	return &AgentPool{
		agents:  make(map[string]Agent),
		perUser: make(map[string]string),
	}
}

func (p *AgentPool) Register(name string, agent Agent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.agents[name] = agent
	if p.active == "" {
		p.active = name
	}
}

func (p *AgentPool) SetDefault(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.agents[name]; !ok {
		return false
	}
	p.active = name
	return true
}

func (p *AgentPool) GetAgent(sessionKey string) Agent {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if name, ok := p.perUser[sessionKey]; ok {
		if agent, ok := p.agents[name]; ok {
			return agent
		}
	}
	return p.agents[p.active]
}

func (p *AgentPool) SetUserAgent(sessionKey, name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.agents[name]; !ok {
		return false
	}
	p.perUser[sessionKey] = name
	return true
}

func (p *AgentPool) ListAgents() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	names := make([]string, 0, len(p.agents))
	for name := range p.agents {
		names = append(names, name)
	}
	return names
}

func (p *AgentPool) ActiveName(sessionKey string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if name, ok := p.perUser[sessionKey]; ok {
		return name
	}
	return p.active
}
