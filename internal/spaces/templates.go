package spaces

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// Template is a saved set of space-creation settings, so a host can stamp out
// identical spaces (a class of students, one space per experiment) without
// re-entering limits and switches every time. It deliberately mirrors
// CreateOptions and nothing else — no members, no host keys, no state.
type Template struct {
	CodexRoute     string    `json:"codex_route,omitempty"`
	CodexModel     string    `json:"codex_model,omitempty"`
	NetworkMode    string    `json:"network_mode,omitempty"`
	Name           string    `json:"name"`
	MemoryGB       int       `json:"memory_gb"`
	CPUs           int       `json:"cpus"`
	Providers      []string  `json:"providers"`
	UsdLimit       float64   `json:"usd_limit"`
	MaxConcurrency int       `json:"max_concurrency"`
	FullAuto       bool      `json:"full_auto"`
	SavedAt        time.Time `json:"saved_at"`
}

// Options converts a template into the CreateOptions for a new space.
func (t Template) Options() CreateOptions {
	return CreateOptions{
		CodexRoute: t.CodexRoute, CodexModel: t.CodexModel, NetworkMode: t.NetworkMode,
		MemoryGB:       t.MemoryGB,
		CPUs:           t.CPUs,
		Providers:      append([]string(nil), t.Providers...),
		UsdLimit:       t.UsdLimit,
		MaxConcurrency: t.MaxConcurrency,
		FullAuto:       t.FullAuto,
	}
}

// TemplateFromSpace captures a space's current settings as a template.
func TemplateFromSpace(name string, r *Space, usdLimit float64, maxConcurrency int) Template {
	return Template{
		CodexRoute: r.CodexRoute, CodexModel: r.CodexModel, NetworkMode: r.NetworkMode,
		Name:           name,
		MemoryGB:       r.MemoryGB,
		CPUs:           r.CPUs,
		Providers:      append([]string(nil), providersOf(r)...),
		UsdLimit:       usdLimit,
		MaxConcurrency: maxConcurrency,
		FullAuto:       r.FullAuto,
		SavedAt:        time.Now().UTC(),
	}
}

// TemplateStore persists templates as one JSON map keyed by template name.
type TemplateStore struct {
	path string
	mu   sync.Mutex
}

func NewTemplateStore(path string) *TemplateStore {
	return &TemplateStore{path: path}
}

func (s *TemplateStore) read() (map[string]Template, error) {
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return map[string]Template{}, nil
	}
	if err != nil {
		return nil, err
	}
	var out map[string]Template
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("corrupt template store %s: %w", s.path, err)
	}
	return out, nil
}

func (s *TemplateStore) write(all map[string]Template) error {
	b, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *TemplateStore) List() ([]Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.read()
	if err != nil {
		return nil, err
	}
	out := make([]Template, 0, len(all))
	for _, t := range all {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get looks a template up by its saved (normalized) name, so the name a user
// typed in a dialog resolves the same way it was stored.
func (s *TemplateStore) Get(name string) (Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.read()
	if err != nil {
		return Template{}, err
	}
	t, ok := all[Normalize(name)]
	if !ok {
		return Template{}, fmt.Errorf("no template %q", name)
	}
	return t, nil
}

// Save upserts a template. Names share the space-name shape so they can prefill
// a space name directly.
func (s *TemplateStore) Save(t Template) (Template, error) {
	name := Normalize(t.Name)
	if !nameRe.MatchString(name) {
		return Template{}, fmt.Errorf("%w: invalid template name %q: use at least 2 characters (letters, digits, hyphens)", ErrInvalid, t.Name)
	}
	t.Name = name
	if t.SavedAt.IsZero() {
		t.SavedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.read()
	if err != nil {
		return Template{}, err
	}
	all[name] = t
	if err := s.write(all); err != nil {
		return Template{}, err
	}
	return t, nil
}

func (s *TemplateStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.read()
	if err != nil {
		return err
	}
	key := Normalize(name)
	if _, ok := all[key]; !ok {
		return fmt.Errorf("no template %q", name)
	}
	delete(all, key)
	return s.write(all)
}
