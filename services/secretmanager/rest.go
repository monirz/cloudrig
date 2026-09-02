package secretmanager

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/monirz/cloudrig/core/gerr"
	"github.com/monirz/cloudrig/transport"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// REST serves the Secret Manager JSON API over the same service the gRPC half
// uses. gcloud speaks REST; the Go client speaks gRPC.
type REST struct {
	router *transport.Router
	svc    *Service
}

// NewREST wires the routes gcloud drives.
func NewREST(s *Service) *REST {
	a := &REST{router: transport.NewRouter(), svc: s}

	const secrets = "/v1/projects/{project}/secrets"
	a.router.Handle(http.MethodGet, secrets, a.listSecrets)
	a.router.Handle(http.MethodPost, secrets, a.createSecret)

	// The name segment carries :verb forms — :addVersion here — which the
	// router captures whole and the handler splits.
	a.router.Handle(http.MethodGet, secrets+"/{secret}", a.getSecret)
	a.router.Handle(http.MethodPost, secrets+"/{secret}", a.secretVerb)
	a.router.Handle(http.MethodPatch, secrets+"/{secret}", a.updateSecret)
	a.router.Handle(http.MethodDelete, secrets+"/{secret}", a.deleteSecret)

	const versions = secrets + "/{secret}/versions"
	a.router.Handle(http.MethodGet, versions, a.listVersions)
	a.router.Handle(http.MethodGet, versions+"/{version}", a.getVersion)
	a.router.Handle(http.MethodPost, versions+"/{version}", a.versionVerb)
	return a
}

// Matches reports whether a route here claims the request; /v1/ is shared.
func (a *REST) Matches(method, escapedPath string) bool {
	return a.router.Matches(method, escapedPath)
}

func (a *REST) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.router.ServeHTTP(w, r) }

func (a *REST) createSecret(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	var secret secretmanagerpb.Secret
	if err := decode(r, &secret); err != nil {
		return err
	}
	out, err := a.svc.CreateSecret(r.Context(), &secretmanagerpb.CreateSecretRequest{
		Parent:   "projects/" + p["project"],
		SecretId: r.URL.Query().Get("secretId"),
		Secret:   &secret,
	})
	return respond(w, out, err)
}

// getSecret answers the plain read and the :getIamPolicy form, which arrives
// glued to the name segment the way every other verb does.
func (a *REST) getSecret(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	name, verb, _ := strings.Cut(p["secret"], ":")

	if verb == "getIamPolicy" {
		return writeIamPolicy(w)
	}
	if verb != "" {
		return unsupportedVerb(name, verb)
	}

	out, err := a.svc.GetSecret(r.Context(), &secretmanagerpb.GetSecretRequest{
		Name: secretName(p),
	})
	return respond(w, out, err)
}

// updateSecret applies the fields named by the update mask, which is how
// gcloud changes a secret's labels.
func (a *REST) updateSecret(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	var secret secretmanagerpb.Secret
	if err := decode(r, &secret); err != nil {
		return err
	}
	secret.Name = secretName(p)

	mask := &fieldmaskpb.FieldMask{}
	if raw := r.URL.Query().Get("updateMask"); raw != "" {
		mask.Paths = strings.Split(raw, ",")
	}

	out, err := a.svc.UpdateSecret(r.Context(), &secretmanagerpb.UpdateSecretRequest{
		Secret: &secret, UpdateMask: mask,
	})
	return respond(w, out, err)
}

func (a *REST) listSecrets(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	out, err := a.svc.ListSecrets(r.Context(), &secretmanagerpb.ListSecretsRequest{
		Parent: "projects/" + p["project"],
	})
	return respond(w, out, err)
}

func (a *REST) deleteSecret(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	out, err := a.svc.DeleteSecret(r.Context(), &secretmanagerpb.DeleteSecretRequest{
		Name: secretName(p),
	})
	return respond(w, out, err)
}

// secretVerb answers the :verb forms posted to a secret.
func (a *REST) secretVerb(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	name, verb, ok := strings.Cut(p["secret"], ":")
	if !ok {
		return unsupportedVerb(p["secret"], "")
	}

	// The IAM verbs answer permissively: there is no identity here to check,
	// which UNSUPPORTED.md says plainly. Refusing them instead would stop
	// commands that only wanted to read a policy.
	switch verb {
	case "setIamPolicy":
		return writeIamPolicy(w)
	case "testIamPermissions":
		var body struct {
			Permissions []string `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		return writeJSON(w, map[string]any{"permissions": body.Permissions})
	}
	if verb != "addVersion" {
		return unsupportedVerb(name, verb)
	}

	var req secretmanagerpb.AddSecretVersionRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	req.Parent = "projects/" + p["project"] + "/secrets/" + name

	out, err := a.svc.AddSecretVersion(r.Context(), &req)
	return respond(w, out, err)
}

func (a *REST) listVersions(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	out, err := a.svc.ListSecretVersions(r.Context(), &secretmanagerpb.ListSecretVersionsRequest{
		Parent: secretName(p),
	})
	return respond(w, out, err)
}

// getVersion answers both the plain read and :access, which is the one that
// returns the value.
func (a *REST) getVersion(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	version, verb, _ := strings.Cut(p["version"], ":")
	name := secretName(p) + "/versions/" + version

	if verb == "access" {
		out, err := a.svc.AccessSecretVersion(r.Context(),
			&secretmanagerpb.AccessSecretVersionRequest{Name: name})
		return respond(w, out, err)
	}
	if verb != "" {
		return unsupportedVerb(name, verb)
	}

	out, err := a.svc.GetSecretVersion(r.Context(),
		&secretmanagerpb.GetSecretVersionRequest{Name: name})
	return respond(w, out, err)
}

// versionVerb answers the state changes: disable, enable and destroy.
func (a *REST) versionVerb(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	version, verb, ok := strings.Cut(p["version"], ":")
	if !ok {
		return unsupportedVerb(p["version"], "")
	}
	name := secretName(p) + "/versions/" + version

	ctx := r.Context()
	switch verb {
	case "disable":
		out, err := a.svc.DisableSecretVersion(ctx, &secretmanagerpb.DisableSecretVersionRequest{Name: name})
		return respond(w, out, err)
	case "enable":
		out, err := a.svc.EnableSecretVersion(ctx, &secretmanagerpb.EnableSecretVersionRequest{Name: name})
		return respond(w, out, err)
	case "destroy":
		out, err := a.svc.DestroySecretVersion(ctx, &secretmanagerpb.DestroySecretVersionRequest{Name: name})
		return respond(w, out, err)
	}
	return unsupportedVerb(name, verb)
}

// writeIamPolicy answers with an empty policy. Nothing is enforced, so there
// is nothing to report, and a missing policy stops a command that only wanted
// to read one.
func writeIamPolicy(w http.ResponseWriter) error {
	return writeJSON(w, map[string]any{"version": 1, "etag": "cloudrig"})
}

func writeJSON(w http.ResponseWriter, body any) error {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	return json.NewEncoder(w).Encode(body)
}

func secretName(p transport.Params) string {
	return "projects/" + p["project"] + "/secrets/" + p["secret"]
}

func unsupportedVerb(name, verb string) error {
	return gerr.New(gerr.Unimplemented, "unsupported verb "+verb+" on "+name).
		WithHTTPStatus(http.StatusNotImplemented)
}

// decode reads a JSON body into a proto. An empty body is not an error: a
// create carries what it needs in the path and the query.
func decode(r *http.Request, into proto.Message) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return gerr.New(gerr.InvalidArgument, "reading the request body: "+err.Error()).
			WithHTTPStatus(http.StatusBadRequest)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	if err := unmarshal.Unmarshal(body, into); err != nil {
		return gerr.New(gerr.InvalidArgument, "malformed JSON body: "+err.Error()).
			WithHTTPStatus(http.StatusBadRequest)
	}
	return nil
}

// respond writes a proto as JSON, or renders a gRPC error as the JSON API
// reports it.
func respond(w http.ResponseWriter, out proto.Message, err error) error {
	if err != nil {
		st, _ := status.FromError(err)
		return gerr.New(gerr.Code(st.Code()), st.Message()).WithHTTPStatus(httpStatusOf(st.Code()))
	}

	body, marshalErr := marshal.Marshal(out)
	if marshalErr != nil {
		return gerr.New(gerr.Internal, "encoding the response: "+marshalErr.Error())
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	// An empty proto encodes as {}, which is what a delete returns.
	if len(body) == 0 {
		body = []byte("{}")
	}
	_, _ = w.Write(append(body, '\n'))
	return nil
}

func httpStatusOf(c codes.Code) int {
	switch c {
	case codes.OK:
		return http.StatusOK
	case codes.InvalidArgument, codes.OutOfRange:
		return http.StatusBadRequest
	case codes.FailedPrecondition:
		return http.StatusBadRequest
	case codes.NotFound:
		return http.StatusNotFound
	case codes.AlreadyExists, codes.Aborted:
		return http.StatusConflict
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.Unimplemented:
		return http.StatusNotImplemented
	}
	return http.StatusInternalServerError
}
