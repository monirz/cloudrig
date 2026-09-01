// Command run-hello is a sample Cloud Run service: an ordinary HTTP server on
// $PORT, which is the whole of Cloud Run's contract with your code.
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
		log.Fatal("PORT is not set")
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			name = "world"
		}
		fmt.Fprintf(w, "hello %s from %s\n", name, os.Getenv("K_SERVICE"))
	})
	http.HandleFunc("/env", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s|%s|%s\n",
			os.Getenv("K_SERVICE"), os.Getenv("K_REVISION"), os.Getenv("GREETING"))
	})

	log.Printf("listening on %s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
