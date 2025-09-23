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

func CountDependencyGroupPackages(m map[string][]Dependency) int {
	count := 0
	for _, group := range m {
		count += len(group)
	}
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

// TypedSyncMap wraps sync.Map with type safety
type TypedSyncMap[K comparable, V any] struct {
	m sync.Map
}

func NewTypedSyncMap[K comparable, V any]() *TypedSyncMap[K, V] {
	return &TypedSyncMap[K, V]{}
}

func (tsm *TypedSyncMap[K, V]) Store(key K, value V) {
	tsm.m.Store(key, value)
}

func (tsm *TypedSyncMap[K, V]) Load(key K) (V, bool) {
	if val, ok := tsm.m.Load(key); ok {
		return val.(V), true
	}
	var zero V
	return zero, false
}

func (tsm *TypedSyncMap[K, V]) LoadOrStore(key K, value V) (V, bool) {
	if val, loaded := tsm.m.LoadOrStore(key, value); loaded {
		return val.(V), true
	}
	return value, false
}

func (tsm *TypedSyncMap[K, V]) Delete(key K) {
	tsm.m.Delete(key)
}

func (tsm *TypedSyncMap[K, V]) Range(f func(key K, value V) bool) {
	tsm.m.Range(func(key, value interface{}) bool {
		return f(key.(K), value.(V))
	})
}
