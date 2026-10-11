// Package orders saves an order to Cloud Storage, then announces it on
// Pub/Sub. The two writes are not atomic, which is the bug the test finds.
package orders

import (
	"context"
	"fmt"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/storage"
)

// Place saves the order, then publishes its id. A publish that fails after the
// save leaves an order nobody downstream hears about.
func Place(ctx context.Context, bucket *storage.BucketHandle, pub *pubsub.Publisher, id string) error {
	w := bucket.Object(id + ".json").NewWriter(ctx)
	if _, err := fmt.Fprintf(w, `{"id":%q}`, id); err != nil {
		w.Close()
		return fmt.Errorf("save %s: %w", id, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("save %s: %w", id, err)
	}
	if _, err := pub.Publish(ctx, &pubsub.Message{Data: []byte(id)}).Get(ctx); err != nil {
		return fmt.Errorf("announce %s: %w", id, err)
	}
	return nil
}
