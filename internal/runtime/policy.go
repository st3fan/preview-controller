package runtime

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var composeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

var allowedServiceKeys = map[string]bool{
	"image": true, "build": true, "command": true, "entrypoint": true,
	"environment": true, "depends_on": true, "healthcheck": true,
	"volumes": true, "expose": true, "working_dir": true, "user": true,
	"restart": true, "init": true, "stop_grace_period": true,
}

// ValidateCompose accepts a deliberately small Compose subset before calling
// Docker Compose. In particular, no external files or host-executed features
// are allowed to be interpreted by the Compose CLI before this check.
func ValidateCompose(filename, checkout, publicService string) error {
	if err := resolvedInsideCheckout(checkout, filename); err != nil {
		return fmt.Errorf("Compose file: %w", err)
	}
	info, err := os.Lstat(filename)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("Compose file must be a regular file")
	}
	if info.Size() > 1<<20 {
		return errors.New("Compose file is too large")
	}
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	decoder := yaml.NewDecoder(io.LimitReader(f, 1<<20+1))
	var doc yaml.Node
	if err := decoder.Decode(&doc); err != nil {
		return fmt.Errorf("parse Compose file: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("Compose file must contain one YAML document")
	}
	if len(doc.Content) != 1 {
		return errors.New("Compose file must contain a mapping")
	}
	if err := inspectNode(doc.Content[0]); err != nil {
		return err
	}
	root, err := mapping(doc.Content[0])
	if err != nil {
		return err
	}
	for key := range root {
		if key != "services" && key != "volumes" {
			return fmt.Errorf("unsupported top-level Compose key %q", key)
		}
	}
	services, err := mapping(root["services"])
	if err != nil || len(services) == 0 {
		return errors.New("Compose services must be a non-empty mapping")
	}
	if _, ok := services[publicService]; !ok {
		return fmt.Errorf("public service %q is missing", publicService)
	}
	volumes := map[string]*yaml.Node{}
	if raw, ok := root["volumes"]; ok {
		volumes, err = mapping(raw)
		if err != nil {
			return fmt.Errorf("top-level volumes: %w", err)
		}
		for name, value := range volumes {
			if !composeName.MatchString(name) || (value.Tag != "!!null" && (value.Kind != yaml.MappingNode || len(value.Content) != 0)) {
				return fmt.Errorf("volume %q must be an empty named volume", name)
			}
		}
	}
	for name, raw := range services {
		if !composeName.MatchString(name) {
			return fmt.Errorf("invalid service name %q", name)
		}
		service, err := mapping(raw)
		if err != nil {
			return fmt.Errorf("service %q: %w", name, err)
		}
		for key, value := range service {
			if !allowedServiceKeys[key] {
				return fmt.Errorf("service %q uses unsupported Compose key %q", name, key)
			}
			if key == "build" {
				if err := validateBuild(value, checkout, filepath.Dir(filename)); err != nil {
					return fmt.Errorf("service %q build: %w", name, err)
				}
			}
			if key == "volumes" {
				if err := validateMounts(value, checkout, filepath.Dir(filename), volumes); err != nil {
					return fmt.Errorf("service %q volumes: %w", name, err)
				}
			}
		}
		if _, image := service["image"]; !image {
			if _, build := service["build"]; !build {
				return fmt.Errorf("service %q needs image or build", name)
			}
		}
	}
	return nil
}

func inspectNode(n *yaml.Node) error {
	if n == nil {
		return errors.New("empty YAML node")
	}
	if n.Anchor != "" || n.Alias != nil || n.Kind == yaml.AliasNode {
		return errors.New("YAML anchors and aliases are unsupported")
	}
	if n.Kind == yaml.ScalarNode && strings.Contains(n.Value, "$") {
		return errors.New("Compose interpolation is unsupported")
	}
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
				return errors.New("Compose mapping has an invalid or duplicate key")
			}
			seen[key.Value] = true
		}
	}
	for _, child := range n.Content {
		if err := inspectNode(child); err != nil {
			return err
		}
	}
	return nil
}

func mapping(n *yaml.Node) (map[string]*yaml.Node, error) {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, errors.New("expected mapping")
	}
	result := make(map[string]*yaml.Node, len(n.Content)/2)
	for i := 0; i < len(n.Content); i += 2 {
		result[n.Content[i].Value] = n.Content[i+1]
	}
	return result, nil
}

func validateBuild(n *yaml.Node, checkout, composeDir string) error {
	if n.Kind == yaml.ScalarNode {
		if err := insideCheckout(checkout, composeDir, n.Value); err != nil {
			return err
		}
		return insideCheckout(checkout, filepath.Join(composeDir, n.Value), "Dockerfile")
	}
	build, err := mapping(n)
	if err != nil {
		return err
	}
	for key := range build {
		if key != "context" && key != "dockerfile" && key != "target" && key != "args" {
			return fmt.Errorf("unsupported build key %q", key)
		}
	}
	context := "."
	if value, ok := build["context"]; ok {
		context = value.Value
	}
	if err := insideCheckout(checkout, composeDir, context); err != nil {
		return err
	}
	dockerfile := "Dockerfile"
	if value, ok := build["dockerfile"]; ok {
		dockerfile = value.Value
	}
	return insideCheckout(checkout, filepath.Join(composeDir, context), dockerfile)
}

func validateMounts(n *yaml.Node, checkout, composeDir string, volumes map[string]*yaml.Node) error {
	if n.Kind != yaml.SequenceNode {
		return errors.New("volumes must be a list")
	}
	for _, item := range n.Content {
		if item.Kind != yaml.ScalarNode {
			return errors.New("only short-form volumes are supported")
		}
		parts := strings.Split(item.Value, ":")
		if len(parts) < 2 || len(parts) > 3 || !filepath.IsAbs(parts[1]) {
			return fmt.Errorf("invalid mount %q", item.Value)
		}
		if len(parts) == 3 && parts[2] != "ro" && parts[2] != "rw" {
			return fmt.Errorf("unsupported mount mode %q", parts[2])
		}
		source := parts[0]
		if strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../") || filepath.IsAbs(source) {
			if err := insideCheckout(checkout, composeDir, source); err != nil {
				return err
			}
		} else if _, ok := volumes[source]; !ok {
			return fmt.Errorf("undeclared named volume %q", source)
		}
	}
	return nil
}

func insideCheckout(root, base, relative string) error {
	if relative == "" || filepath.IsAbs(relative) {
		return errors.New("path must be relative to the checkout")
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	target, err := filepath.EvalSymlinks(filepath.Join(base, relative))
	if err != nil {
		return err
	}
	path, err := filepath.Rel(root, target)
	if err != nil || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
		return errors.New("path escapes the checkout")
	}
	return nil
}
