package relay

import "net/http"

// ServeHTTPSync is the priority CloudEvent handler for sync-over-Kafka jobs
// (KafkaSource → POST /sync).
//
// It increments the syncPriority counter for the duration of the job so that
// the async handler (ServeHTTP) returns 503 and KafkaSource defers those
// messages until all sync jobs on this pod are done. Using a counter (rather
// than a binary flag) is safe when containerConcurrency > 1 — multiple sync
// jobs can run concurrently without one accidentally clearing another's flag.
func (d *Dispatcher) ServeHTTPSync(w http.ResponseWriter, r *http.Request) {
	d.syncPriority.Add(1)
	defer d.syncPriority.Add(-1)
	d.serveHTTP(w, r)
}
