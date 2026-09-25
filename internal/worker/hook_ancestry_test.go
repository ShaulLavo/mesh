package worker

import "testing"

// Agent helper 100 runs provider 200. Hooks are 9xx.
func hookProcessTable(entries map[int]ancestorProcess) ancestorProcessReader {
	return func(pid int) (ancestorProcess, bool) {
		process, ok := entries[pid]
		return process, ok
	}
}

func TestHookFromAgentAcceptsTheLaunchedProvider(t *testing.T) {
	read := hookProcessTable(map[int]ancestorProcess{
		100: {parentID: 50, args: []string{"mesh", "agent", "claude"}},
		200: {parentID: 100, args: []string{"claude"}},
		300: {parentID: 200, args: []string{"/bin/sh", "-c", "mesh agent-hook claude"}},
		901: {parentID: 200, args: []string{"mesh", "agent-hook", "claude"}},
		902: {parentID: 300, args: []string{"mesh", "agent-hook", "claude"}},
	})
	for _, peer := range []int{100, 901, 902} {
		if !hookFromAgent(peer, 100, read) {
			t.Errorf("hookFromAgent(%d) = false, want true", peer)
		}
	}
}

func TestHookFromAgentRejectsNestedProviders(t *testing.T) {
	read := hookProcessTable(map[int]ancestorProcess{
		100: {parentID: 50, args: []string{"mesh", "agent", "claude"}},
		200: {parentID: 100, args: []string{"claude"}},
		// The provider's tool shell exec'd a headless provider in place.
		400: {parentID: 200, args: []string{"claude", "-p", "hi"}},
		901: {parentID: 400, args: []string{"mesh", "agent-hook", "claude"}},
		401: {parentID: 400, args: []string{"/bin/sh", "-c", "mesh agent-hook claude"}},
		902: {parentID: 401, args: []string{"mesh", "agent-hook", "claude"}},
		// A test runner under a tool shell started one.
		500: {parentID: 200, args: []string{"/bin/bash", "-c", "bun test"}},
		501: {parentID: 500, args: []string{"bun", "test"}},
		502: {parentID: 501, args: []string{"claude", "-p", "hi"}},
		503: {parentID: 502, args: []string{"/bin/sh", "-c", "mesh agent-hook claude"}},
		903: {parentID: 503, args: []string{"mesh", "agent-hook", "claude"}},
	})
	for _, peer := range []int{901, 902, 903, 1} {
		if hookFromAgent(peer, 100, read) {
			t.Errorf("hookFromAgent(%d) = true, want false", peer)
		}
	}
}
