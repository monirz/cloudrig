// Command cloudrun-demo is a sample Cloud Run service.
//
// Cloud Run's contract with your code is an HTTP server on $PORT, and the
// K_ variables describing which service and revision is serving. That is the
// whole of what this uses.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		log.Fatal("PORT is not set; Cloud Run always sets it")
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello from %s (%s)\n",
			os.Getenv("K_SERVICE"), os.Getenv("K_REVISION"))
	})
	http.HandleFunc("/env", func(w http.ResponseWriter, r *http.Request) {
		for _, key := range []string{"K_SERVICE", "K_REVISION", "K_CONFIGURATION", "TIER"} {
			fmt.Fprintf(w, "%s=%s\n", key, os.Getenv(key))
		}
	})

	log.Printf("listening on %s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
