package gke

import (
	"io"
	"net/http"
	"strings"

	"cloud.google.com/go/container/apiv1/containerpb"
	"github.com/monirz/cloudrig/core/gerr"
	"github.com/monirz/cloudrig/transport"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// MaxBodyBytes caps a JSON request body; the port is shared.
const MaxBodyBytes = 4 << 20

// REST serves the GKE JSON API so `gcloud container clusters` works. The Go
// client speaks gRPC; gcloud speaks REST, over the same service.
type REST struct {
	router *transport.Router
	svc    *Service
}

// NewREST wires the routes gcloud drives.
func NewREST(s *Service) *REST {
	a := &REST{router: transport.NewRouter(), svc: s}

	const base = "/v1/projects/{project}/locations/{location}"
	a.router.Handle(http.MethodGet, base+"/clusters", a.listClusters)
	a.router.Handle(http.MethodPost, base+"/clusters", a.createCluster)
	a.router.Handle(http.MethodGet, base+"/clusters/{cluster}", a.getCluster)
	a.router.Handle(http.MethodDelete, base+"/clusters/{cluster}", a.deleteCluster)
	a.router.Handle(http.MethodGet, base+"/operations/{operation}", a.getOperation)
	return a
}

// Matches reports whether a route here claims the request; /v1/ is shared.
func (a *REST) Matches(method, escapedPath string) bool {
	return a.router.Matches(method, escapedPath)
}

func (a *REST) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.router.ServeHTTP(w, r) }

func parent(p transport.Params) string {
	return "projects/" + p["project"] + "/locations/" + p["location"]
}

func (a *REST) createCluster(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	var req containerpb.CreateClusterRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	req.Parent = parent(p)
	out, err := a.svc.CreateCluster(r.Context(), &req)
	return respond(w, out, err)
}

func (a *REST) getCluster(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	out, err := a.svc.GetCluster(r.Context(), &containerpb.GetClusterRequest{
		Name: parent(p) + "/clusters/" + p["cluster"],
	})
	return respond(w, out, err)
}

func (a *REST) listClusters(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	out, err := a.svc.ListClusters(r.Context(), &containerpb.ListClustersRequest{Parent: parent(p)})
	return respond(w, out, err)
}

func (a *REST) deleteCluster(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	out, err := a.svc.DeleteCluster(r.Context(), &containerpb.DeleteClusterRequest{
		Name: parent(p) + "/clusters/" + p["cluster"],
	})
	return respond(w, out, err)
}

func (a *REST) getOperation(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	out, err := a.svc.GetOperation(r.Context(), &containerpb.GetOperationRequest{
		Name: parent(p) + "/operations/" + p["operation"],
	})
	return respond(w, out, err)
}

func decode(r *http.Request, into proto.Message) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		return gerr.New(gerr.InvalidArgument, "reading the request body: "+err.Error()).
			WithHTTPStatus(http.StatusBadRequest)
	}
	if len(body) > MaxBodyBytes {
		return gerr.New(gerr.InvalidArgument, "the request body is too large").
			WithHTTPStatus(http.StatusRequestEntityTooLarge)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, into); err != nil {
		return gerr.New(gerr.InvalidArgument, "malformed JSON body: "+err.Error()).
			WithHTTPStatus(http.StatusBadRequest)
	}
	return nil
}

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
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		return http.StatusBadRequest
	case codes.NotFound:
		return http.StatusNotFound
	case codes.AlreadyExists, codes.Aborted:
		return http.StatusConflict
	case codes.Unimplemented:
		return http.StatusNotImplemented
	}
	return http.StatusInternalServerError
}
