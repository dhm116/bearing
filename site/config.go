package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"

	"go.yaml.in/yaml/v3"
)

// Config is site.yaml: what the site is called, where its Markdown comes
// from and which hand-written pages it has.
type Config struct {
	Title       string       `yaml:"title"`
	Tagline     string       `yaml:"tagline"`
	Description string       `yaml:"description"`
	Repo        string       `yaml:"repo"`
	Branch      string       `yaml:"branch"`
	Brand       string       `yaml:"brand"`
	Nav         []Link       `yaml:"nav"`
	Pages       []PageSpec   `yaml:"pages"`
	Collections []Collection `yaml:"collections"`
}

// Link is a label and a target. Href is site-relative ("spec/") unless it
// has a scheme.
type Link struct {
	Label string `yaml:"label"`
	Href  string `yaml:"href"`
}

// PageSpec is a hand-designed page rendered from templates/<Template>.html.
type PageSpec struct {
	Template    string `yaml:"template"`
	Path        string `yaml:"path"`
	Title       string `yaml:"title"`
	Description string `yaml:"description"`
}

// Collection is a directory of Markdown files published as one section,
// such as docs/spec. Each file becomes <path>/<name>/; README.md becomes
// the section's index.
type Collection struct {
	ID          string   `yaml:"id"`
	Title       string   `yaml:"title"`
	Short       string   `yaml:"short"`
	Description string   `yaml:"description"`
	Dir         string   `yaml:"dir"`
	Path        string   `yaml:"path"`
	Order       []string `yaml:"order"`
	Exclude     []string `yaml:"exclude"`
}

var (
	repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

// LoadConfig reads and checks site.yaml.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("site: read config: %w", err)
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("site: parse %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("site: %s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) validate() error {
	var errs []error
	if c.Title == "" {
		errs = append(errs, errors.New("title is required"))
	}
	if !repoPattern.MatchString(c.Repo) {
		errs = append(errs, fmt.Errorf("repo %q is not owner/name", c.Repo))
	}
	if c.Branch == "" {
		c.Branch = "main"
	}
	ids := map[string]bool{}
	for _, col := range c.Collections {
		if !slugPattern.MatchString(col.ID) || !slugPattern.MatchString(col.Path) {
			errs = append(errs, fmt.Errorf("collection %q: id and path must be lowercase slugs", col.ID))
		}
		if ids[col.ID] {
			errs = append(errs, fmt.Errorf("collection %q is listed twice", col.ID))
		}
		ids[col.ID] = true
		if col.Dir == "" || col.Title == "" {
			errs = append(errs, fmt.Errorf("collection %q: dir and title are required", col.ID))
		}
	}
	for _, p := range c.Pages {
		if p.Template == "" || p.Title == "" {
			errs = append(errs, fmt.Errorf("page %q: template and title are required", p.Path))
		}
	}
	return errors.Join(errs...)
}
