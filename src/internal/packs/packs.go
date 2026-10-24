// Package packs loads process-type manifests (YAML) into a registry.
package packs

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"sigs.k8s.io/yaml"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
)

// Registry holds the process types known to this deployment.
type Registry struct {
	types map[string]*v1.ProcessTypeSpec
	order []string
}

// LoadDir loads every *.yaml under dir (recursively).
func LoadDir(dir string) (*Registry, error) {
	r := &Registry{types: map[string]*v1.ProcessTypeSpec{}}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !(strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml")) {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		spec, err := Parse(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if _, dup := r.types[spec.Name]; dup {
			return fmt.Errorf("%s: process type %q defined twice", path, spec.Name)
		}
		r.types[spec.Name] = spec
		r.order = append(r.order, spec.Name)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(r.types) == 0 {
		return nil, fmt.Errorf("no process types found in %s", dir)
	}
	sort.Strings(r.order)
	return r, nil
}

// Parse decodes and validates one manifest.
func Parse(raw []byte) (*v1.ProcessTypeSpec, error) {
	js, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return nil, err
	}
	spec := &v1.ProcessTypeSpec{}
	if err := protojson.Unmarshal(js, spec); err != nil {
		return nil, err
	}
	for name, wf := range spec.Workflows {
		if wf.Name == "" {
			wf.Name = name
		}
	}
	return spec, Validate(spec)
}

func Validate(s *v1.ProcessTypeSpec) error {
	if s.Name == "" || len(s.Stages) == 0 || s.Image == "" {
		return fmt.Errorf("process type needs name, image and stages")
	}
	if len(s.Workflows) == 0 {
		return fmt.Errorf("%s: no workflows", s.Name)
	}
	if _, ok := s.Workflows[s.DefaultWorkflow]; !ok {
		return fmt.Errorf("%s: default_workflow %q is not defined", s.Name, s.DefaultWorkflow)
	}
	for name, wf := range s.Workflows {
		if len(wf.Stages) == 0 {
			return fmt.Errorf("%s/%s: no stages", s.Name, name)
		}
		last := -1
		for _, st := range wf.Stages {
			idx := slices.Index(s.Stages, st.Stage)
			if idx < 0 {
				return fmt.Errorf("%s/%s: stage %q is not a stage of %s", s.Name, name, st.Stage, s.Name)
			}
			if idx <= last {
				return fmt.Errorf("%s/%s: stage %q is out of order", s.Name, name, st.Stage)
			}
			last = idx
			switch st.Kind {
			case v1.StageKind_STAGE_KIND_AGENT:
				if strings.TrimSpace(st.Prompt) == "" {
					return fmt.Errorf("%s/%s/%s: agent stage needs a prompt", s.Name, name, st.Stage)
				}
			case v1.StageKind_STAGE_KIND_PARK:
				if st.ParkOn == "" {
					return fmt.Errorf("%s/%s/%s: park stage needs park_on", s.Name, name, st.Stage)
				}
			default:
				return fmt.Errorf("%s/%s/%s: stage kind is required", s.Name, name, st.Stage)
			}
		}
	}
	return nil
}

func (r *Registry) Get(name string) (*v1.ProcessTypeSpec, bool) {
	s, ok := r.types[name]
	return s, ok
}

// All returns the process types in name order.
func (r *Registry) All() []*v1.ProcessTypeSpec {
	out := make([]*v1.ProcessTypeSpec, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.types[n])
	}
	return out
}
