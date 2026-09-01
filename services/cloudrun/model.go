package cloudrun

import (
	"errors"
	"strings"
)

// The Knative shapes gcloud sends and reads. Cloud Run's regional API is
// Knative's serving API, so a deploy arrives as a Service object with the
// image buried in a pod template.

type knativeService struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   knativeMeta    `json:"metadata"`
	Spec       knativeSpec    `json:"spec"`
	Status     *knativeStatus `json:"status,omitempty"`
}

type knativeMeta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	UID         string            `json:"uid,omitempty"`
	Generation  int               `json:"generation,omitempty"`
}

type knativeSpec struct {
	Template knativeTemplate `json:"template"`
}

type knativeTemplate struct {
	Metadata knativeMeta         `json:"metadata,omitempty"`
	Spec     knativeTemplateSpec `json:"spec"`
}

type knativeTemplateSpec struct {
	Containers []knativeContainer `json:"containers"`
}

type knativeContainer struct {
	Image     string            `json:"image,omitempty"`
	Env       []knativeEnv      `json:"env,omitempty"`
	Ports     []knativePort     `json:"ports,omitempty"`
	Resources *knativeResources `json:"resources,omitempty"`
}

// knativeResources is where --memory and --cpu arrive, as Kubernetes quantity
// strings: "512Mi", "1", "500m".
type knativeResources struct {
	Limits map[string]string `json:"limits,omitempty"`
}

type knativeEnv struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type knativePort struct {
	Name          string `json:"name,omitempty"`
	ContainerPort int    `json:"containerPort,omitempty"`
}

type knativeStatus struct {
	URL                       string             `json:"url,omitempty"`
	LatestReadyRevisionName   string             `json:"latestReadyRevisionName,omitempty"`
	LatestCreatedRevisionName string             `json:"latestCreatedRevisionName,omitempty"`
	ObservedGeneration        int                `json:"observedGeneration,omitempty"`
	Conditions                []knativeCondition `json:"conditions,omitempty"`
	Traffic                   []knativeTraffic   `json:"traffic,omitempty"`
}

type knativeCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

type knativeTraffic struct {
	RevisionName   string `json:"revisionName,omitempty"`
	Percent        int    `json:"percent"`
	LatestRevision bool   `json:"latestRevision,omitempty"`
}

type knativeList struct {
	APIVersion string           `json:"apiVersion"`
	Kind       string           `json:"kind"`
	Metadata   listMeta         `json:"metadata"`
	Items      []knativeService `json:"items"`
}

// listMeta is Kubernetes' ListMeta. gcloud reads continue off it without
// checking for nil, so a list without one crashes the client rather than
// returning an error.
type listMeta struct {
	Continue        string `json:"continue"`
	ResourceVersion string `json:"resourceVersion"`
}

// SourcePrefix marks an image field that names a source directory rather than
// a container image. Cloud Run's API has nowhere to say "run this directory",
// so this is cloudrig's own spelling of it.
const SourcePrefix = "source:"

// toKnative renders a deployed service the way gcloud reads one back.
//
// The status is what makes a deploy finish: gcloud polls until Ready is True
// and reports the URL from here.
func toKnative(svc Service, url string) knativeService {
	container := knativeContainer{Image: svc.Image}
	if svc.Memory != "" || svc.CPU != "" {
		limits := map[string]string{}
		if svc.Memory != "" {
			limits["memory"] = svc.Memory
		}
		if svc.CPU != "" {
			limits["cpu"] = svc.CPU
		}
		container.Resources = &knativeResources{Limits: limits}
	}
	for _, kv := range svc.Env {
		if name, value, ok := splitEnv(kv); ok {
			container.Env = append(container.Env, knativeEnv{Name: name, Value: value})
		}
	}
	if svc.Image == "" {
		// A source deploy has no image to report. Naming the directory is
		// more honest than an empty field, and SourcePrefix makes the round
		// trip work: a deploy of what was read back deploys the same thing.
		container.Image = SourcePrefix + svc.Source
	}

	revision := svc.Revision()
	return knativeService{
		APIVersion: "serving.knative.dev/v1",
		Kind:       "Service",
		Metadata: knativeMeta{
			Name:       svc.Name,
			Namespace:  svc.Project,
			Generation: svc.Generation,
			// gcloud reads the region from this label, not from the
			// annotation: without it the REGION column comes out blank.
			Labels: map[string]string{
				"cloud.googleapis.com/location": svc.Location,
			},
			Annotations: map[string]string{
				"run.googleapis.com/location": svc.Location,
			},
		},
		Spec: knativeSpec{Template: knativeTemplate{
			Metadata: knativeMeta{Name: revision},
			Spec:     knativeTemplateSpec{Containers: []knativeContainer{container}},
		}},
		Status: &knativeStatus{
			URL:                       url,
			LatestReadyRevisionName:   revision,
			LatestCreatedRevisionName: revision,
			ObservedGeneration:        svc.Generation,
			// Every condition Ready, because by the time this is rendered the
			// container is already serving: the deploy waited for it.
			Conditions: []knativeCondition{
				{Type: "Ready", Status: "True"},
				{Type: "ConfigurationsReady", Status: "True"},
				{Type: "RoutesReady", Status: "True"},
			},
			Traffic: []knativeTraffic{{RevisionName: revision, Percent: 100, LatestRevision: true}},
		},
	}
}

// fromKnative reads a deploy request that arrived over the network.
//
// A source directory is deliberately not readable here. The image field is
// request-controlled, and turning it into a host path means any client that
// can reach the port can run a program on this machine as whoever started the
// emulator — the daemon binds every interface and enforces no IAM. Source
// deploys go through the Go API, where the caller already has the process.
func fromKnative(k knativeService, project, location string) (Service, error) {
	svc, err := readKnative(k, project, location)
	if err != nil {
		return svc, err
	}
	if svc.Source != "" {
		return Service{}, ErrSourceOverNetwork
	}
	return svc, nil
}

// ErrSourceOverNetwork is returned when a request asks the emulator to run a
// directory from this machine.
var ErrSourceOverNetwork = errors.New(
	"a source directory cannot be deployed over the API, because it runs a program on the " +
		"emulator's machine: deploy an image, or use cloudrun.Registry.Deploy from your own code")

// readKnative decodes a Service without the network check, for the library.
func readKnative(k knativeService, project, location string) (Service, error) {
	svc := Service{
		Project:  project,
		Location: location,
		Name:     k.Metadata.Name,
	}
	if k.Metadata.Namespace != "" {
		svc.Project = k.Metadata.Namespace
	}
	if containers := k.Spec.Template.Spec.Containers; len(containers) > 0 {
		if source, ok := strings.CutPrefix(containers[0].Image, SourcePrefix); ok {
			svc.Source = source
		} else {
			svc.Image = containers[0].Image
		}
		for _, e := range containers[0].Env {
			svc.Env = append(svc.Env, e.Name+"="+e.Value)
		}
		if limits := containers[0].Resources.GetLimits(); limits != nil {
			svc.Memory, svc.CPU = limits["memory"], limits["cpu"]
		}
	}
	return svc, nil
}

func splitEnv(kv string) (name, value string, ok bool) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}
	return "", "", false
}

// knativeRevision is what `gcloud run revisions list` renders.
type knativeRevision struct {
	APIVersion string              `json:"apiVersion"`
	Kind       string              `json:"kind"`
	Metadata   knativeMeta         `json:"metadata"`
	Spec       knativeTemplateSpec `json:"spec"`
	Status     *knativeStatus      `json:"status,omitempty"`
}

// toRevision renders the one revision a service has here.
func toRevision(svc Service) knativeRevision {
	image := svc.Image
	if image == "" {
		image = SourcePrefix + svc.Source
	}

	return knativeRevision{
		APIVersion: "serving.knative.dev/v1",
		Kind:       "Revision",
		Metadata: knativeMeta{
			Name:      svc.Revision(),
			Namespace: svc.Project,
			Labels: map[string]string{
				"cloud.googleapis.com/location":     svc.Location,
				"serving.knative.dev/service":       svc.Name,
				"serving.knative.dev/configuration": svc.Name,
			},
		},
		Spec:   knativeTemplateSpec{Containers: []knativeContainer{{Image: image}}},
		Status: &knativeStatus{Conditions: []knativeCondition{{Type: "Ready", Status: "True"}}},
	}
}

// GetLimits reads the limits from a resources block that may be absent.
func (r *knativeResources) GetLimits() map[string]string {
	if r == nil {
		return nil
	}
	return r.Limits
}
