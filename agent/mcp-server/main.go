package main

import (
	"log"
	"net/http"
	"os"
)

func main() {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8081"
	}

	mux, hasClient := newMuxForEnvironment()
	if hasClient {
		log.Printf("mcp-server: in-cluster Kubernetes client ready")
	} else {
		log.Printf("mcp-server: no in-cluster Kubernetes config found; k8s_get/k8s_describe will error until deployed in-cluster")
	}

	log.Printf("mcp-server listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
