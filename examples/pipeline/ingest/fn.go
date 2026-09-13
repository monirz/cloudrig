// Package ingest is a Storage-triggered function that publishes the new object
// to a Pub/Sub topic. It reaches CloudRig's Pub/Sub through PUBSUB_EMULATOR_HOST.
package ingest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"cloud.google.com/go/pubsub/v2"
)

type event struct {
	Data struct {
		Bucket string `json:"bucket"`
		Name   string `json:"name"`
	} `json:"data"`
}

func Handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var e event
	_ = json.Unmarshal(body, &e)

	ctx := context.Background()
	c, err := pubsub.NewClient(ctx, "cloudrig-local")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer c.Close()
	res := c.Publisher("projects/cloudrig-local/topics/orders").Publish(ctx, &pubsub.Message{
		Data: []byte(e.Data.Name),
	})
	if _, err := res.Get(ctx); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
