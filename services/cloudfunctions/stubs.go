package cloudfunctions

import (
	"encoding/json"
	"net/http"

	"github.com/monirz/cloudrig/core/gerr"
	"github.com/monirz/cloudrig/transport"
)

// The methods here belong to other GCP services, but gcloud consults them
// during a functions deploy. Serving them is not scope creep: without them
// gcloud reaches cloudresourcemanager.googleapis.com, serviceusage and
// cloudbuild on the public internet — which fails the deploy, and with live
// credentials would touch a real project.
//
// Each answers the minimum gcloud reads. A full emulation of these services
// belongs to their own milestones.

// getProject is cloudresourcemanager projects.get. Every project exists here:
// an emulator has no project registry to consult, and refusing unknown ones
// would only make the user create something that means nothing.
func (s *Service) getProject(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	return writeJSON(w, http.StatusOK, map[string]any{
		"projectId":      p["project"],
		"name":           p["project"],
		"projectNumber":  "000000000000",
		"lifecycleState": "ACTIVE",
	})
}

// getService is serviceusage services.get. Everything reports enabled: there is
// no billing or quota here to gate a service behind.
func (s *Service) getService(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	return writeJSON(w, http.StatusOK, map[string]any{
		"name":   "projects/" + p["project"] + "/services/" + p["service"],
		"config": map[string]any{"name": p["service"]},
		"state":  "ENABLED",
	})
}

// testProjectPermissions is cloudresourcemanager projects.testIamPermissions.
// Every requested permission is granted: there is no IAM here to deny one, and
// answering with an empty set makes gcloud refuse the deploy.
func (s *Service) testProjectPermissions(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	_, verb := splitVerb(p["project"])
	if verb != "testIamPermissions" {
		return gerr.NewUnimplemented("cloudresourcemanager.projects." + verb)
	}

	var body struct {
		Permissions []string `json:"permissions"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	return writeJSON(w, http.StatusOK, map[string]any{"permissions": body.Permissions})
}

// defaultServiceAccount is cloudbuild projects.locations.getDefaultServiceAccount.
// gcloud reads it to warn about build permissions, which do not apply locally.
func (s *Service) defaultServiceAccount(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	return writeJSON(w, http.StatusOK, map[string]any{
		"name":                "projects/" + p["project"] + "/locations/" + p["location"] + "/defaultServiceAccount",
		"serviceAccountEmail": p["project"] + "@cloudrig.local",
	})
}
