package camera

import (
	"sort"
	"sync"

	"github.com/corticoide/mockvision/sdk/engine"
)

// accountStore holds the camera's accounts. The service replaces them when
// they are edited, and engines read them on every request, so a new
// password works without restarting the camera.
type accountStore struct {
	mu    sync.RWMutex
	users []engine.User
}

func (a *accountStore) set(users []engine.User) {
	list := append([]engine.User(nil), users...)
	sort.Slice(list, func(i, j int) bool { return list[i].Username < list[j].Username })
	a.mu.Lock()
	a.users = list
	a.mu.Unlock()
}

// List implements engine.Accounts.
func (a *accountStore) List() []engine.User {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return append([]engine.User(nil), a.users...)
}

// Lookup implements engine.Accounts.
func (a *accountStore) Lookup(username string) (engine.User, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, u := range a.users {
		if u.Username == username {
			return u, true
		}
	}
	return engine.User{}, false
}
