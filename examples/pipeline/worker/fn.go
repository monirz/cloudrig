// Package worker is a Pub/Sub-triggered function. It receives the order the
// ingest function published and enqueues a Cloud Task to process it later,
// which CloudRig then retries on a schedule you control with the clock.
package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"os"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type event struct {
	Data struct {
		Data string `json:"data"` // base64-encoded payload
	} `json:"data"`
}

func Handler(w http.ResponseWriter, r *http.Request) {
	var e event
	_ = json.NewDecoder(r.Body).Decode(&e)
	order, _ := base64.StdEncoding.DecodeString(e.Data.Data)

	// CLOUDRIG_ENDPOINT is a URL; a gRPC client wants a bare host:port.
	endpoint := os.Getenv("CLOUDRIG_ENDPOINT")
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		endpoint = u.Host
	}

	ctx := context.Background()
	c, err := cloudtasks.NewClient(ctx,
		option.WithEndpoint(endpoint),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer c.Close()

	if _, err := c.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{
		Parent: "projects/cloudrig-local/locations/us-central1/queues/pipeline",
		Task: &cloudtaskspb.Task{MessageType: &cloudtaskspb.Task_HttpRequest{
			HttpRequest: &cloudtaskspb.HttpRequest{
				Url:        os.Getenv("SINK_URL"),
				HttpMethod: cloudtaskspb.HttpMethod_POST,
				Body:       order,
			},
		}},
	}); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
