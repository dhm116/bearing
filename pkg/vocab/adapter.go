package vocab

import "fmt"

// CheckAdapter is what the server demands of a module an adapter publishes:
// it stays out of the core namespace, and its suggestions start at its own
// terms and end elsewhere (the registry then limits the end to core or
// operator terms).
func (m Module) CheckAdapter() error {
	if err := m.Check(); err != nil {
		return err
	}
	if m.Namespace == CoreNamespace {
		return fmt.Errorf("module %s: %w: adapters may not define core terms", m.Namespace, ErrNamespace)
	}
	for _, s := range m.Suggests {
		sns, _, _ := Split(s.Subject)
		ons, _, _ := Split(s.Object)
		if sns != m.Namespace || ons == m.Namespace {
			return fmt.Errorf("module %s: suggestion %s: %w: must start at its own term and end at another namespace", m.Namespace, s.ID, ErrMapping)
		}
	}
	return nil
}
