// Package ingest is a Storage-triggered function that publishes the new object
// to a Pub/Sub topic. It reaches CloudRig's Pub/Sub through PUBSUB_EMULATOR_HOST.
package ingest

import (
	"context"
	"encoding/json"
	"io"
	"log"
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
	id, err := res.Get(ctx)
	if err != nil {
		log.Printf("ERROR publishing %s: %v", e.Data.Name, err)
		http.Error(w, err.Error(), 500)
		return
	}
	log.Printf("gs://%s/%s -> published to orders (message %s)", e.Data.Bucket, e.Data.Name, id)
	w.WriteHeader(http.StatusNoContent)
}
