package cloudfunctions

import (
	"encoding/json"
	"net/http"
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
	UpdateTime   string        `json:"updateTime,omitempty"`
}

type httpsTrigger struct {
	URL           string `json:"url"`
	SecurityLevel string `json:"securityLevel,omitempty"`
}

func toV1(d functions.Descriptor, r *http.Request) v1Function {
	return v1Function{
		Name:       d.ResourceName(),
		Status:     d.State,
		Runtime:    string(d.Runtime),
		EntryPoint: d.EntryPoint,
		HTTPSTrigger: &httpsTrigger{
			// Derived from the request host rather than configured, so the URL
			// is reachable however the caller got here.
			URL:           functionURL(r, d),
			SecurityLevel: "SECURE_OPTIONAL",
		},
		UpdateTime: d.UpdateTime.UTC().Format(time.RFC3339Nano),
	}
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
		URL:           url,
		UpdateTime:    d.UpdateTime.UTC().Format(time.RFC3339Nano),
	}
}
