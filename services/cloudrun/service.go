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

	// Image is a container image to run, which is what Cloud Run deploys. It
	// needs a container runtime.
	Image string

	// Source is a directory to build and run as a process instead. It is the
	// faster path and needs no Docker, but it is not what Cloud Run does: it
	// honours the contract — an HTTP server on $PORT — without the container.
	Source string

	// Env is what the service sees, as KEY=VALUE.
	Env []string

	// Memory and CPU are Kubernetes quantities, as gcloud sends them:
	// "512Mi", "1Gi", "1", "500m". They are applied to the container, so a
	// service that would be killed for exceeding its memory is killed here
	// too. They mean nothing to a source deploy, which is not a container.
	Memory string
	CPU    string

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
	switch {
	case s.Image == "" && s.Source == "":
		return s, fmt.Errorf("service %s: an image or a source directory is required", s.Name)
	case s.Image != "" && s.Source != "":
		return s, fmt.Errorf("service %s: give an image or a source directory, not both", s.Name)
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
