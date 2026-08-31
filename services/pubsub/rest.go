package pubsub

import (
	"net/http"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/monirz/cloudrig/transport"
)

// REST serves the Pub/Sub JSON API over the same service the gRPC half uses.
//
// It exists for Terraform: the Google provider speaks REST for every resource,
// so a gRPC-only Pub/Sub is invisible to it however complete it is.
type REST struct {
	router *transport.Router
	pub    *Publisher
	sub    *Subscriber
}

// NewREST wires the JSON routes Terraform drives.
func NewREST(s *Service) *REST {
	a := &REST{router: transport.NewRouter(), pub: NewPublisher(s), sub: NewSubscriber(s)}

	const topics = "/v1/projects/{project}/topics"
	a.router.Handle(http.MethodPut, topics+"/{topic}", a.createTopic)
	a.router.Handle(http.MethodGet, topics+"/{topic}", a.getTopic)
	a.router.Handle(http.MethodPatch, topics+"/{topic}", a.updateTopic)
	a.router.Handle(http.MethodDelete, topics+"/{topic}", a.deleteTopic)
	a.router.Handle(http.MethodGet, topics, a.listTopics)

	const subs = "/v1/projects/{project}/subscriptions"
	a.router.Handle(http.MethodPut, subs+"/{subscription}", a.createSubscription)
	a.router.Handle(http.MethodGet, subs+"/{subscription}", a.getSubscription)
	a.router.Handle(http.MethodPatch, subs+"/{subscription}", a.updateSubscription)
	a.router.Handle(http.MethodDelete, subs+"/{subscription}", a.deleteSubscription)
	a.router.Handle(http.MethodGet, subs, a.listSubscriptions)
	return a
}

// Matches reports whether a route here claims the request. Cloud Functions
// mounts the same /v1/ prefix, so the two are told apart by route, not prefix.
func (a *REST) Matches(method, escapedPath string) bool {
	return a.router.Matches(method, escapedPath)
}

func (a *REST) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.router.ServeHTTP(w, r) }

func (a *REST) createTopic(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	var t pubsubpb.Topic
	if err := decode(r, &t); err != nil {
		return err
	}
	// The name lives in the path; a body that omits it is normal.
	t.Name = "projects/" + p["project"] + "/topics/" + p["topic"]

	out, err := a.pub.CreateTopic(r.Context(), &t)
	return respond(w, out, err)
}

func (a *REST) getTopic(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	out, err := a.pub.GetTopic(r.Context(), &pubsubpb.GetTopicRequest{
		Topic: "projects/" + p["project"] + "/topics/" + p["topic"],
	})
	return respond(w, out, err)
}

func (a *REST) updateTopic(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	var req pubsubpb.UpdateTopicRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.Topic == nil {
		req.Topic = &pubsubpb.Topic{}
	}
	req.Topic.Name = "projects/" + p["project"] + "/topics/" + p["topic"]
	req.UpdateMask = maskFrom(r, req.GetUpdateMask())

	out, err := a.pub.UpdateTopic(r.Context(), &req)
	return respond(w, out, err)
}

func (a *REST) deleteTopic(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	out, err := a.pub.DeleteTopic(r.Context(), &pubsubpb.DeleteTopicRequest{
		Topic: "projects/" + p["project"] + "/topics/" + p["topic"],
	})
	return respond(w, out, err)
}

func (a *REST) listTopics(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	out, err := a.pub.ListTopics(r.Context(), &pubsubpb.ListTopicsRequest{
		Project: "projects/" + p["project"],
	})
	return respond(w, out, err)
}

func (a *REST) createSubscription(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	var sub pubsubpb.Subscription
	if err := decode(r, &sub); err != nil {
		return err
	}
	sub.Name = "projects/" + p["project"] + "/subscriptions/" + p["subscription"]

	out, err := a.sub.CreateSubscription(r.Context(), &sub)
	return respond(w, out, err)
}

func (a *REST) getSubscription(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	out, err := a.sub.GetSubscription(r.Context(), &pubsubpb.GetSubscriptionRequest{
		Subscription: "projects/" + p["project"] + "/subscriptions/" + p["subscription"],
	})
	return respond(w, out, err)
}

func (a *REST) updateSubscription(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	var req pubsubpb.UpdateSubscriptionRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.Subscription == nil {
		req.Subscription = &pubsubpb.Subscription{}
	}
	req.Subscription.Name = "projects/" + p["project"] + "/subscriptions/" + p["subscription"]
	req.UpdateMask = maskFrom(r, req.GetUpdateMask())

	out, err := a.sub.UpdateSubscription(r.Context(), &req)
	return respond(w, out, err)
}

func (a *REST) deleteSubscription(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	out, err := a.sub.DeleteSubscription(r.Context(), &pubsubpb.DeleteSubscriptionRequest{
		Subscription: "projects/" + p["project"] + "/subscriptions/" + p["subscription"],
	})
	return respond(w, out, err)
}

func (a *REST) listSubscriptions(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	out, err := a.sub.ListSubscriptions(r.Context(), &pubsubpb.ListSubscriptionsRequest{
		Project: "projects/" + p["project"],
	})
	return respond(w, out, err)
}
