package main

import "log"

// The zk-drive worker binary will host NATS JetStream consumers for preview,
// scan, index, retention, and archive jobs. That work lands in Phase 2. For
// Phase 1 it is deliberately a stub so the CI build and deployment
// manifests can reference the binary.
func main() {
	log.Println("worker not yet implemented; scheduled for Phase 2")
}
