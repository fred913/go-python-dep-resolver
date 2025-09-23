package main

import "sync"

func CountSyncMap(m *sync.Map) int {
	count := 0
	m.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}

// Thread-safe visited map wrapper
type visitedMap struct {
	mu sync.RWMutex
	m  map[string]bool
}

func newVisitedMap() *visitedMap {
	return &visitedMap{
		m: make(map[string]bool),
	}
}

func (vm *visitedMap) isVisited(key string) bool {
	vm.mu.RLock()
	defer vm.mu.RUnlock()
	return vm.m[key]
}

func (vm *visitedMap) setVisited(key string) bool {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.m[key] {
		return true // already visited
	}
	vm.m[key] = true
	return false // newly visited
}
