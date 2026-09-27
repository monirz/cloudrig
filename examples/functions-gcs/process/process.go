// Package process is a Storage-triggered demo function: it reads the uploaded
// object from GCS, counts its lines and words, and writes a report to another bucket.
package process

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"cloud.google.com/go/storage"
)

type event struct {
	Data struct {
		Bucket string `json:"bucket"`
		Name   string `json:"name"`
	} `json:"data"`
}

func fail(w http.ResponseWriter, step string, err error) {
	log.Printf("ERROR %s: %v", step, err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func Process(w http.ResponseWriter, r *http.Request) {
	var e event
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("triggered by gs://%s/%s", e.Data.Bucket, e.Data.Name)

	ctx := context.Background()
	c, err := storage.NewClient(ctx) // STORAGE_EMULATOR_HOST points it at cloudrig
	if err != nil {
		fail(w, "storage client", err)
		return
	}
	defer c.Close()

	rd, err := c.Bucket(e.Data.Bucket).Object(e.Data.Name).NewReader(ctx)
	if err != nil {
		fail(w, "read object", err)
		return
	}
	body, _ := io.ReadAll(rd)
	rd.Close()
	text := string(body)
	lines, words := strings.Count(text, "\n"), len(strings.Fields(text))
	log.Printf("read %d bytes: %d lines, %d words", len(body), lines, words)

	out := e.Data.Name + ".report.json"
	ow := c.Bucket("processed").Object(out).NewWriter(ctx)
	json.NewEncoder(ow).Encode(map[string]any{
		"source": fmt.Sprintf("gs://%s/%s", e.Data.Bucket, e.Data.Name),
		"bytes":  len(body), "lines": lines, "words": words,
	})
	if err := ow.Close(); err != nil {
		fail(w, "write report", err)
		return
	}
	log.Printf("wrote gs://processed/%s", out)
	w.WriteHeader(http.StatusNoContent)
}
