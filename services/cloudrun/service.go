package cloudrun

import (
	"fmt"
	"strings"
)

// Service is a deployed Cloud Run service.
type Service struct {
	Project  string
	Location string
	Name     string

	// Source is the directory to build and run. A prebuilt image is the case
	// that needs a container runtime, and is not supported.
	Source string

	// Env is what the service sees, as KEY=VALUE.
	Env []string

	// Generation counts deploys, so each one names a new revision the way
	// Cloud Run does.
	Generation int
}

// Defaults for a deploy that does not say.
const (
	DefaultProject  = "cloudrig-local"
	DefaultLocation = "us-central1"
)

// resolve fills the defaults and checks what cannot be defaulted.
func (s Service) resolve() (Service, error) {
	if s.Name == "" {
		return s, fmt.Errorf("a service needs a name")
	}
	if s.Source == "" {
		return s, fmt.Errorf("service %s: a source directory is required", s.Name)
	}
	if s.Project == "" {
		s.Project = DefaultProject
	}
	if s.Location == "" {
		s.Location = DefaultLocation
	}
	if s.Generation < 1 {
		s.Generation = 1
	}
	return s, nil
}

// ResourceName is the v2 API's name for the service.
func (s Service) ResourceName() string {
	return fmt.Sprintf("projects/%s/locations/%s/services/%s", s.Project, s.Location, s.Name)
}

// Revision names the current deploy, as Cloud Run spells it: service-00001-abc.
func (s Service) Revision() string {
	return fmt.Sprintf("%s-%05d-cri", s.Name, s.Generation)
}

// key identifies a service in the registry.
func key(project, location, name string) string {
	return strings.Join([]string{project, location, name}, "/")
}
