package profile

import (
	"sync"

	"github.com/Public/convert-tux-to-go/internal/common"
)

// gonav is profile #1: the reference Go service conventions (sqlx, tx
// variants, pkg/services/<svc> layout) — today's gen/convert constants
// extracted verbatim (P1). The zero-config default.
type gonav struct{}

// Registry resolves config selections to profiles. Built-in profiles
// register at init; nothing else writes the map after init.
var (
	mu       sync.RWMutex
	registry = map[string]Profile{}
)

// Register adds a profile under its config selector. Reserved for the
// registry table below (P4's dotnet profile registers here).
func Register(p Profile) {
	mu.Lock()
	defer mu.Unlock()
	registry[p.ID()] = p
}

// Resolve returns the profile for a config value; absent/empty selects the
// gonav default; an unknown selector is a config error surfaced by Resolve
// (validated in cmd, P3).
func For(id string) (Profile, error) {
	mu.RLock()
	defer mu.RUnlock()
	if id == "" {
		return gonav{}, nil
	}
	p, ok := registry[id]
	if !ok {
		return nil, ErrUnknownProfile{ID: id}
	}
	return p, nil
}

// Default returns the zero-config profile (gonav). Callers that predate
// profile wiring treat nil as Default.
func Default() Profile {
	p, err := For("")
	if err != nil {
		panic("profile: gonav default missing from the registry")
	}
	return p
}

// ErrUnknownProfile is a config-validation error (P3 wires it into
// config.Validate).
type ErrUnknownProfile struct{ ID string }

func (e ErrUnknownProfile) Error() string {
	return "profile: unknown target.profile " + e.ID
}

// id implements Profile.ID for gonav.
func (gonav) ID() string { return "gonav" }

// Naming implements Profile.Naming with the gen conventions (verbatim
// from gen.requestType/responseType/RowName/lowerFirst — P1 re-points).
func (gonav) Naming() Naming {
	return Naming{
		Request:  func(endpoint string) string { return endpoint + "Request" },
		Response: func(endpoint string) string { return endpoint + "Response" },
		Row: func(methodName string) string {
			for _, verb := range []string{"Get", "Insert", "Update", "Delete", "Merge"} {
				if n, ok := cutPrefix(methodName, verb); ok {
					return n
				}
			}
			return methodName
		},
		Receiver: common.LowerFirst,
	}
}

// Layout implements Profile.Layout with the reference service tree.
func (gonav) Layout() Layout {
	return Layout{
		Layers: map[string]string{
			"db":         "db",
			"controller": "controller",
			"handler":    "handler",
			"models":     "models",
		},
		ServiceDir: func(module, service string) string {
			return module + "/pkg/services/" + common.LowerFirst(service)
		},
	}
}

// DB implements Profile.DB with the sqlx/tx-variant contract (decision 27).
func (gonav) DB() DBRules {
	return DBRules{
		StoreReceiver: "s.store.",
		TxVariants: func(queryType string) bool {
			switch queryType {
			case "INSERT", "UPDATE", "DELETE":
				return true
			}
			return false
		},
	}
}

// cutPrefix reports s with prefix removed.
func cutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return "", false
}
