package wrapper

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/a2aproject/a2a-go/a2a"
)

// RegistryAgentLister returns a function that lists agents from a registry URL.
func RegistryAgentLister(registryURL string) func() []*a2a.AgentCard {
	return func() []*a2a.AgentCard {
		resp, err := http.Get(registryURL + "/agents")
		if err != nil {
			return nil
		}
		defer resp.Body.Close()
		var cards []*a2a.AgentCard
		json.NewDecoder(resp.Body).Decode(&cards)
		return cards
	}
}

// RegistryAgentCaller returns a function that calls an agent looked up from a registry.
func RegistryAgentCaller(registryURL string) func(ctx context.Context, agentName string, task string) (string, error) {
	return func(ctx context.Context, agentName string, task string) (string, error) {
		// Look up agent from registry
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, registryURL+"/agents/"+agentName, nil)
		if err != nil {
			return "", fmt.Errorf("lookup agent %s: %w", agentName, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("lookup agent %s: %w", agentName, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("agent %s not found in registry", agentName)
		}
		var card a2a.AgentCard
		if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
			return "", fmt.Errorf("decode agent card: %w", err)
		}

		// Create A2A client and send message
		client, err := NewAgentClient(ctx, &card)
		if err != nil {
			return "", err
		}
		return client.SendMessage(ctx, task)
	}
}
