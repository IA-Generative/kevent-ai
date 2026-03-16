package relay

import "net/http"

// ServeHTTPSync is the priority CloudEvent handler for sync-over-Kafka jobs
// (KafkaSource → POST /sync).
//
// It sets the syncPriority flag for the duration of the job so that the async
// handler (ServeHTTP) returns 503 and KafkaSource defers those messages until
// the sync job is done. This gives sync requests first access to the GPU within
// each pod, without requiring any shared state across pods.
func (d *Dispatcher) ServeHTTPSync(w http.ResponseWriter, r *http.Request) {
	d.syncPriority.Store(1)
	defer d.syncPriority.Store(0)
	d.serveHTTP(w, r)
}
