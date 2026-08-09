package host

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/extension"
)

// ManagedExtension is one reloadable Extension generation source
type ManagedExtension interface {
	agent.ToolSource
	agent.ComponentSource
	ExtensionID() string
	CurrentGeneration() uint64
	AcquireCommands(ctx context.Context) ([]extension.Command, func(), error)
	Reload(ctx context.Context, options ProcessOptions) (uint64, error)
	CloseContext(ctx context.Context) error
}

// GenerationSet pins one generation from every configured Extension for a Run
//
// Component and Command acquisition is serialized with Reload at the set
// boundary, so callers see either the complete generation set before a reload
// or the complete set after it. Generations remain independently numbered.
type GenerationSet struct {
	mu      sync.RWMutex
	sources map[string]ManagedExtension
	ids     []string
	closed  bool
}

// NewGenerationSet validates and constructs an Extension Generation Set
func NewGenerationSet(sources []ManagedExtension) (*GenerationSet, error) {
	if len(sources) == 0 {
		return nil, errors.New("at least one managed Extension is required")
	}
	set := &GenerationSet{sources: make(map[string]ManagedExtension, len(sources))}
	for _, source := range sources {
		if source == nil {
			return nil, errors.New("managed Extension must not be nil")
		}
		id := source.ExtensionID()
		if id == "" {
			return nil, errors.New("managed Extension ID is required")
		}
		if _, duplicate := set.sources[id]; duplicate {
			return nil, fmt.Errorf("Extension %q is configured more than once", id)
		}
		set.sources[id] = source
		set.ids = append(set.ids, id)
	}
	sort.Strings(set.ids)
	return set, nil
}

// AcquireTools pins the current generation of every Extension until release
func (set *GenerationSet) AcquireTools(ctx context.Context) ([]agent.Tool, func(), error) {
	components, release, err := set.AcquireComponents(ctx)
	if err != nil {
		return nil, nil, err
	}
	return components.Tools, release, nil
}

// AcquireComponents atomically pins Tools and Hooks from every Extension
func (set *GenerationSet) AcquireComponents(ctx context.Context) (agent.RunComponents, func(), error) {
	if ctx == nil {
		return agent.RunComponents{}, nil, errors.New("Extension Generation Set context must not be nil")
	}
	set.mu.RLock()
	if set.closed {
		set.mu.RUnlock()
		return agent.RunComponents{}, nil, ErrHostClosed
	}
	var tools []agent.Tool
	var hooks []agent.Hook
	releases := make([]func(), 0, len(set.ids))
	for _, id := range set.ids {
		acquired, release, err := set.sources[id].AcquireComponents(ctx)
		if err != nil {
			for index := len(releases) - 1; index >= 0; index-- {
				releases[index]()
			}
			set.mu.RUnlock()
			return agent.RunComponents{}, nil, fmt.Errorf("acquire Extension %q generation: %w", id, err)
		}
		if release == nil {
			release = func() {}
		}
		releases = append(releases, release)
		tools = append(tools, acquired.Tools...)
		hooks = append(hooks, acquired.Hooks...)
	}
	set.mu.RUnlock()

	var once sync.Once
	return agent.RunComponents{Tools: tools, Hooks: hooks}, func() {
		once.Do(func() {
			for index := len(releases) - 1; index >= 0; index-- {
				releases[index]()
			}
		})
	}, nil
}

// AcquireCommands pins commands from every Extension generation atomically
func (set *GenerationSet) AcquireCommands(ctx context.Context) ([]extension.Command, func(), error) {
	if ctx == nil {
		return nil, nil, errors.New("Extension Generation Set context must not be nil")
	}
	set.mu.RLock()
	if set.closed {
		set.mu.RUnlock()
		return nil, nil, ErrHostClosed
	}
	var commands []extension.Command
	releases := make([]func(), 0, len(set.ids))
	for _, id := range set.ids {
		acquired, release, err := set.sources[id].AcquireCommands(ctx)
		if err != nil {
			for index := len(releases) - 1; index >= 0; index-- {
				releases[index]()
			}
			set.mu.RUnlock()
			return nil, nil, fmt.Errorf("acquire Extension %q commands: %w", id, err)
		}
		if release == nil {
			release = func() {}
		}
		releases = append(releases, release)
		commands = append(commands, acquired...)
	}
	set.mu.RUnlock()
	var once sync.Once
	return commands, func() {
		once.Do(func() {
			for index := len(releases) - 1; index >= 0; index-- {
				releases[index]()
			}
		})
	}, nil
}

// Reload atomically excludes new generation snapshots while one Extension swaps
func (set *GenerationSet) Reload(ctx context.Context, extensionID string, options ProcessOptions) (uint64, error) {
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.closed {
		return 0, ErrHostClosed
	}
	source := set.sources[extensionID]
	if source == nil {
		return 0, fmt.Errorf("Extension %q is not configured", extensionID)
	}
	return source.Reload(ctx, options)
}

// Generations returns the generation selected for the next Run by Extension ID
func (set *GenerationSet) Generations() map[string]uint64 {
	set.mu.RLock()
	defer set.mu.RUnlock()
	result := make(map[string]uint64, len(set.sources))
	if set.closed {
		return result
	}
	for _, id := range set.ids {
		result[id] = set.sources[id].CurrentGeneration()
	}
	return result
}

// ExtensionIDs returns configured Extension IDs in lexical order
func (set *GenerationSet) ExtensionIDs() []string {
	set.mu.RLock()
	defer set.mu.RUnlock()
	return append([]string(nil), set.ids...)
}

// CloseContext rejects new snapshots and closes all managed Extensions
func (set *GenerationSet) CloseContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("Extension Generation Set close context must not be nil")
	}
	set.mu.Lock()
	if set.closed {
		set.mu.Unlock()
		return nil
	}
	set.closed = true
	sources := make([]ManagedExtension, 0, len(set.ids))
	for _, id := range set.ids {
		sources = append(sources, set.sources[id])
	}
	set.mu.Unlock()

	var closeErr error
	for index, source := range sources {
		if err := source.CloseContext(ctx); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("close Extension %q: %w", set.ids[index], err))
		}
	}
	return closeErr
}

// Close rejects new snapshots and closes all managed Extensions
func (set *GenerationSet) Close() error {
	return set.CloseContext(context.Background())
}
