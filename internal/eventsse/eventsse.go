// Package eventsse bridges the catalog pipeline's Kafka events to browsers over
// SSE. A per-pod consumer (unique group, LATEST offset — live tail, no history
// replay) publishes thin catalog.updated notifications into an in-process
// broker; each connected client receives them on GET /api/v1/events. Events are
// notifications, not data — {type, itemId, itemType, phase} — the SPA debounces
// and refetches its own queries, so nothing here couples to the catalog schema.
// A title's extra packaged is a note for the title, phase "extra.packaged".
//
// Everything is defensive: no brokers configured => the broker is inert (the
// endpoint still serves, clients just only get heartbeats), and consumer errors
// retry rather than crash. Heartbeat comments go out every 20s so idle streams
// survive the OpenShift router's ~30s inactivity timeout.
package eventsse

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	kafka "github.com/segmentio/kafka-go"
)

// tenantPrefix is the tenant prefix with its dot, "stube." when there is none
// (mirrors katalog-manager's events.Configure — a shared cluster hosts
// multiple platform instances, each under its own prefix).
func tenantPrefix(prefix string) string {
	p := strings.TrimSpace(prefix)
	if p == "" {
		p = "stube."
	}
	if !strings.HasSuffix(p, ".") {
		p += "."
	}
	return p
}

// catalogTopics derives the pipeline topic names from the tenant prefix.
func catalogTopics(prefix string) []string {
	p := tenantPrefix(prefix)
	return []string{
		p + "catalog.item.discovered",
		p + "catalog.item.enriched",
		p + "catalog.item.analyzed",
		p + "catalog.item.transcoded",
		p + "catalog.item.packaged", // pipeline end: the item became watchable
		p + "catalog.item.removed",  // catalog deletion — drop the item from open UIs
	}
}

// extraTopic is the topic of a title's extras (a trailer, a featurette, …)
// packaged: the title's detail lists one more. Its events name the title
// (itemId, type), as the item events do. It is tailed apart from the item
// topics, once it exists (Run).
func extraTopic(prefix string) string {
	return tenantPrefix(prefix) + "catalog.extra.packaged"
}

// phaseOf is a note's phase for a message of topic: the step that happened,
// the topic's last part ("packaged"), and for a topic of a title's extras
// "extra." before it ("extra.packaged") — an extra packaged is not its title
// packaged.
func phaseOf(topic string) string {
	parts := strings.Split(topic, ".")
	phase := parts[len(parts)-1]
	if len(parts) >= 2 && parts[len(parts)-2] == "extra" {
		return "extra." + phase
	}
	return phase
}

// noteOf is the note for a message of topic with the pipeline envelope
// value; false when value is no envelope.
func noteOf(topic string, value []byte) (Note, bool) {
	var ev itemEvent
	if json.Unmarshal(value, &ev) != nil {
		return Note{}, false
	}
	return Note{ItemID: ev.ItemID, ItemType: ev.Type, Phase: phaseOf(topic)}, true
}

const (
	heartbeatEvery = 20 * time.Second
	clientBuffer   = 16
	groupPrefix    = "chino-events"
	// topicPollEvery is how often Run asks whether the extras' topic
	// exists, until it does.
	topicPollEvery = time.Minute
)

// Note is the thin notification sent to browsers.
type Note struct {
	Type     string `json:"type"` // always "catalog.updated"
	ItemID   string `json:"itemId,omitempty"`
	ItemType string `json:"itemType,omitempty"`
	Phase    string `json:"phase,omitempty"`
}

// itemEvent is the subset of the pipeline envelope this bridge reads.
type itemEvent struct {
	ItemID string `json:"itemId"`
	Type   string `json:"type,omitempty"`
}

// Broker fans events out to subscribed SSE clients.
type Broker struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func NewBroker() *Broker { return &Broker{subs: map[chan []byte]struct{}{}} }

// Publish sends a note to every subscriber; slow clients are dropped (they
// reconnect and refetch — which is the contract anyway).
func (b *Broker) Publish(n Note) {
	n.Type = "catalog.updated"
	payload, err := json.Marshal(n)
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- payload:
		default:
			delete(b.subs, ch)
			close(ch)
		}
	}
}

func (b *Broker) subscribe() chan []byte {
	ch := make(chan []byte, clientBuffer)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *Broker) unsubscribe(ch chan []byte) {
	b.mu.Lock()
	if _, ok := b.subs[ch]; ok {
		delete(b.subs, ch)
		close(ch)
	}
	b.mu.Unlock()
}

// Handler serves the SSE stream. Mount inside an AUTHENTICATED group WITHOUT a
// request timeout (the server's WriteTimeout is already 0 for long-lived
// streams).
func (b *Broker) Handler(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, ": connected\n\n")
	fl.Flush()

	ch := b.subscribe()
	defer b.unsubscribe(ch)
	hb := time.NewTicker(heartbeatEvery)
	defer hb.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-hb.C:
			if _, err := fmt.Fprint(w, ": hb\n\n"); err != nil {
				return
			}
			fl.Flush()
		case payload, ok := <-ch:
			if !ok {
				return
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// Run consumes the catalog topics into the broker until ctx is cancelled.
// brokers empty => logged no-op. The group is unique per pod so every replica
// sees every event (each fans out only to its own connected clients).
//
// The extras' topic has a group of its own, started once the topic exists: a
// cluster that has no such topic yet (one not provisioned for extras) would
// leave a group that names it with no partition of any of its topics —
// kafka-go reads them in one metadata request and drops them all when one is
// unknown — and the item notes would stop.
func (b *Broker) Run(ctx context.Context, brokers []string, certDir, topicPrefix string) {
	if len(brokers) == 0 {
		slog.Info("eventsse: no Kafka brokers configured; live refresh inactive")
		return
	}
	tlsCfg, err := maybeTLS(certDir)
	if err != nil {
		slog.Warn("eventsse: kafka TLS material unreadable; consumer not started", "err", err)
		return
	}
	dialer := &kafka.Dialer{Timeout: 10 * time.Second, DualStack: true, TLS: tlsCfg}
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	extras := extraTopic(topicPrefix)
	go func() {
		exists := func(ctx context.Context) (bool, error) { return topicExists(ctx, dialer, brokers, extras) }
		if waitForTopic(ctx, topicPollEvery, extras, exists) {
			b.tail(ctx, dialer, brokers, groupPrefix+"-extras-"+host, []string{extras})
		}
	}()
	b.tail(ctx, dialer, brokers, groupPrefix+"-"+host, catalogTopics(topicPrefix))
}

// waitForTopic asks exists every interval until it says topic exists (true)
// or ctx ends (false). It says once that it waits.
func waitForTopic(ctx context.Context, every time.Duration, topic string, exists func(context.Context) (bool, error)) bool {
	said := false
	for {
		ok, err := exists(ctx)
		if ok {
			return true
		}
		if !said {
			slog.Info("eventsse: topic not there yet; asked again every "+every.String(), "topic", topic, "err", err)
			said = true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(every):
		}
	}
}

// topicExists asks the first broker that answers whether topic has
// partitions. A topic the cluster does not know is no error.
func topicExists(ctx context.Context, dialer *kafka.Dialer, brokers []string, topic string) (bool, error) {
	var lastErr error
	for _, addr := range brokers {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			lastErr = err
			continue
		}
		partitions, err := conn.ReadPartitions(topic)
		_ = conn.Close()
		if errors.Is(err, kafka.UnknownTopicOrPartition) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return len(partitions) > 0, nil
	}
	return false, lastErr
}

// tail consumes topics with the consumer group group into the broker until
// ctx is cancelled.
func (b *Broker) tail(ctx context.Context, dialer *kafka.Dialer, brokers []string, group string, topics []string) {
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        brokers,
		GroupID:        group,
		GroupTopics:    topics,
		Dialer:         dialer,
		StartOffset:    kafka.LastOffset,
		CommitInterval: 10 * time.Second,
	})
	defer reader.Close()
	slog.Info("eventsse: kafka tail active", "topics", len(topics), "group", group)

	for {
		msg, err := reader.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("eventsse: read error", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if note, ok := noteOf(msg.Topic, msg.Value); ok {
			b.Publish(note)
		}
	}
}

// maybeTLS returns nil (plaintext) when certDir is empty or holds no user.crt,
// or a populated mTLS config when the triple is present (the Strimzi profile).
func maybeTLS(dir string) (*tls.Config, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	certPath := filepath.Join(dir, "user.crt")
	if _, err := os.Stat(certPath); err != nil {
		return nil, nil
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "user.key"))
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("ca.crt contains no valid certificate")
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool}, nil
}
