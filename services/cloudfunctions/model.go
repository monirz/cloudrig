package cloudfunctions

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/monirz/cloudrig/functions"
)

// v1Function is the CloudFunction resource as v1 spells it.
//
// It is a projection of functions.Descriptor, not a stored shape: the registry
// holds the facts, and v2 will render the same facts differently.
type v1Function struct {
	Name         string        `json:"name"`
	Status       string        `json:"status"`
	Runtime      string        `json:"runtime,omitempty"`
	EntryPoint   string        `json:"entryPoint,omitempty"`
	HTTPSTrigger *httpsTrigger `json:"httpsTrigger,omitempty"`
	EventTrigger *v1Event      `json:"eventTrigger,omitempty"`
	UpdateTime   string        `json:"updateTime,omitempty"`
}

type v1Event struct {
	EventType string `json:"eventType"`
	Resource  string `json:"resource,omitempty"`
	Service   string `json:"service,omitempty"`
}

type httpsTrigger struct {
	URL           string `json:"url"`
	SecurityLevel string `json:"securityLevel,omitempty"`
}

func toV1(d functions.Descriptor, r *http.Request) v1Function {
	fn := v1Function{
		Name:       d.ResourceName(),
		Status:     d.State,
		Runtime:    string(d.Runtime),
		EntryPoint: d.EntryPoint,
		UpdateTime: d.UpdateTime.UTC().Format(time.RFC3339Nano),
	}
	// v1 has one trigger per function: an event or HTTPS, never both.
	if t := d.Trigger; t.IsSet() {
		fn.EventTrigger = &v1Event{EventType: t.EventType}
		switch {
		case t.Resource == "":
		case isStorageEvent(t.EventType):
			fn.EventTrigger.Resource = "projects/_/buckets/" + t.Resource
			fn.EventTrigger.Service = "storage.googleapis.com"
		case isPubsubEvent(t.EventType):
			fn.EventTrigger.Resource = topicName(d.Project, t.Resource)
			fn.EventTrigger.Service = "pubsub.googleapis.com"
		default:
			fn.EventTrigger.Resource = t.Resource
		}
		return fn
	}
	fn.HTTPSTrigger = &httpsTrigger{
		// Derived from the request host rather than configured, so the URL
		// is reachable however the caller got here.
		URL:           functionURL(r, d),
		SecurityLevel: "SECURE_OPTIONAL",
	}
	return fn
}

func isStorageEvent(t string) bool { return strings.HasPrefix(t, "google.storage.") }
func isPubsubEvent(t string) bool  { return strings.HasPrefix(t, "google.pubsub.") }

// topicName accepts a bare topic or a full projects/P/topics/T name.
func topicName(project, topic string) string {
	if strings.HasPrefix(topic, "projects/") {
		return topic
	}
	return "projects/" + project + "/topics/" + topic
}

func functionURL(r *http.Request, d functions.Descriptor) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/" + d.Location + "-" + d.Project + "/" + d.Name
}

func writeJSON(w http.ResponseWriter, status int, body any) error {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(body)
}

// v2Function is the same facts as v1Function, nested the way v2 spells them.
// Two projections of one Descriptor, never a translation of each other.
type v2Function struct {
	Name          string         `json:"name"`
	Environment   string         `json:"environment"`
	State         string         `json:"state"`
	BuildConfig   *v2BuildConfig `json:"buildConfig,omitempty"`
	ServiceConfig *v2ServiceCfg  `json:"serviceConfig,omitempty"`
	EventTrigger  *v2Event       `json:"eventTrigger,omitempty"`
	URL           string         `json:"url,omitempty"`
	UpdateTime    string         `json:"updateTime,omitempty"`
}

type v2BuildConfig struct {
	Runtime    string `json:"runtime,omitempty"`
	EntryPoint string `json:"entryPoint,omitempty"`
}

type v2ServiceCfg struct {
	URI string `json:"uri,omitempty"`
}

type v2Event struct {
	EventType    string          `json:"eventType"`
	EventFilters []v2EventFilter `json:"eventFilters,omitempty"`
	PubsubTopic  string          `json:"pubsubTopic,omitempty"`
}

type v2EventFilter struct {
	Attribute string `json:"attribute"`
	Value     string `json:"value"`
}

func toV2Event(d functions.Descriptor) *v2Event {
	t := d.Trigger
	if !t.IsSet() {
		return nil
	}
	e := &v2Event{EventType: t.EventType}
	switch {
	case t.Resource == "":
	case isStorageEvent(t.EventType):
		e.EventFilters = []v2EventFilter{{Attribute: "bucket", Value: t.Resource}}
	case isPubsubEvent(t.EventType):
		e.PubsubTopic = topicName(d.Project, t.Resource)
	}
	return e
}

// environment is what these functions really are. Claiming GEN_2 would make
// gcloud mint an identity token and call a Cloud Run URL that does not exist.
const environment = "GEN_1"

func toV2(d functions.Descriptor, r *http.Request) v2Function {
	url := functionURL(r, d)
	return v2Function{
		Name:          d.ResourceName(),
		Environment:   environment,
		State:         d.State,
		BuildConfig:   &v2BuildConfig{Runtime: string(d.Runtime), EntryPoint: d.EntryPoint},
		ServiceConfig: &v2ServiceCfg{URI: url},
		EventTrigger:  toV2Event(d),
		URL:           url,
		UpdateTime:    d.UpdateTime.UTC().Format(time.RFC3339Nano),
	}
}
