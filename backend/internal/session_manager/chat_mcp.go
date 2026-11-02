package sessionmanager

import (
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// A chat session's caller-supplied tool servers (spawn mcpServers) are kept for
// this daemon's lifetime, so a resume (an idle agent brought back, a controller
// rebuilt) reattaches them. They are deliberately not persisted: their env and
// header values are credentials, and the caller re-supplies them for new
// sessions after a restart. Entries are bound to the harness they were given
// for, so an agent switch never hands one agent's servers to another.

const maxChatMCPEntries = 2000

type chatMCPEntry struct {
	harness domain.AgentHarness
	servers []ports.ChatMCPServerConfig
}

type chatMCPStore struct {
	mu      sync.Mutex
	entries map[domain.SessionID]chatMCPEntry
}

func (s *chatMCPStore) set(id domain.SessionID, harness domain.AgentHarness, servers []ports.ChatMCPServerConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(servers) == 0 {
		delete(s.entries, id)
		return
	}
	if s.entries == nil {
		s.entries = map[domain.SessionID]chatMCPEntry{}
	}
	if _, exists := s.entries[id]; !exists && len(s.entries) >= maxChatMCPEntries {
		for stale := range s.entries {
			delete(s.entries, stale)
			break
		}
	}
	s.entries[id] = chatMCPEntry{harness: harness, servers: ports.CloneChatMCPServers(servers)}
}

// get returns a copy of the servers recorded for id, only when they were given
// for the same harness.
func (s *chatMCPStore) get(id domain.SessionID, harness domain.AgentHarness) []ports.ChatMCPServerConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[id]
	if !ok || entry.harness != harness {
		return nil
	}
	return ports.CloneChatMCPServers(entry.servers)
}
